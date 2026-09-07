use anyhow::{Context, Result, bail};
use std::{path::Path, process::Stdio, time::Duration};
use tokio::{
    io::{AsyncRead, AsyncReadExt},
    process::{Child, Command},
};
use tokio_util::sync::CancellationToken;

// Own a process group so timeout, task cancellation and service shutdown also stop children.
pub struct GroupChild(pub Child, u32);
impl GroupChild {
    pub fn new(child: Child) -> Self {
        let id = child.id().expect("new child has a process id");
        Self(child, id)
    }
}
impl Drop for GroupChild {
    fn drop(&mut self) {
        // Keep the original group ID even after wait() reaps the leader.
        // This also terminates background descendants after normal completion.
        unsafe {
            libc::kill(-(self.1 as i32), libc::SIGKILL);
        }
        let _ = self.0.start_kill();
    }
}
pub fn command(binary: &str, cwd: &Path) -> Command {
    let mut c = Command::new(binary);
    c.current_dir(cwd)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(true)
        .process_group(0)
        .env_remove("OCTOMUS_TOKEN")
        .env("GIT_TERMINAL_PROMPT", "0");
    c
}
async fn bounded_read(mut r: impl AsyncRead + Unpin) -> std::io::Result<String> {
    let mut kept = Vec::new();
    let mut buf = [0; 8192];
    let mut truncated = false;
    loop {
        let n = r.read(&mut buf).await?;
        if n == 0 {
            break;
        }
        let remaining = 262144usize.saturating_sub(kept.len());
        truncated |= n > remaining;
        kept.extend_from_slice(&buf[..n.min(remaining)]);
    }
    let mut text = String::from_utf8_lossy(&kept).into_owned();
    if truncated {
        text.push_str("\n[output truncated at 262144 bytes]");
    }
    Ok(text)
}
pub async fn run(
    binary: &str,
    args: &[&str],
    cwd: &Path,
    seconds: u64,
    cancel: &CancellationToken,
) -> Result<String> {
    anyhow::ensure!(!cancel.is_cancelled(), "Operation cancelled");
    let mut child = GroupChild::new(
        command(binary, cwd)
            .args(args)
            .spawn()
            .with_context(|| format!("Could not start {binary}"))?,
    );
    let stdout = child.0.stdout.take().unwrap();
    let stderr = child.0.stderr.take().unwrap();
    let future = async {
        let (status, out, err) =
            tokio::join!(child.0.wait(), bounded_read(stdout), bounded_read(stderr));
        let status = status?;
        let out = out?;
        let err = err?;
        if !status.success() {
            bail!(
                "{binary} exited with {status}: {}",
                crate::store::redact(&format!("{out}\n{err}"))
            );
        }
        Ok(out.trim().to_owned())
    };
    tokio::select! { result=tokio::time::timeout(Duration::from_secs(seconds),future)=>result.context("Command timed out")?, _=cancel.cancelled()=>bail!("Operation cancelled") }
}
