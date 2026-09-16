use anyhow::{Context, Result, bail};
use std::{future::Future, path::Path, process::Stdio, time::Duration};
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
        // SAFETY: kill() with a negative PID signals a process group and is safe to
        // call with any value; the worst outcome is ESRCH, which we ignore. The stored
        // group ID is the one recorded at spawn, so it stays valid even after wait()
        // reaps the leader. This also terminates background descendants after normal
        // completion.
        #[allow(unsafe_code)]
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
        .env_remove(crate::api::TOKEN_ENV)
        .env_remove(crate::notifications::WEBHOOK_ENV)
        .env("GIT_TERMINAL_PROMPT", "0");
    c
}
pub const DIAGNOSTIC_LIMIT: usize = 262_144;
pub const MACHINE_LIMIT: usize = 16 * 1024 * 1024;
#[derive(Debug, Clone, Copy)]
pub enum CaptureMode {
    Diagnostic,
    Machine,
}
#[derive(Debug)]
pub struct Captured {
    pub bytes: Vec<u8>,
    pub truncated: bool,
}
impl Captured {
    fn preview(&self) -> String {
        let text = String::from_utf8_lossy(&self.bytes);
        if self.truncated {
            format!("{text}\n[diagnostic output truncated]")
        } else {
            text.into_owned()
        }
    }
}
#[derive(Debug)]
pub struct ProcessOutput {
    pub status: std::process::ExitStatus,
    pub stdout: Captured,
    pub stderr: Captured,
}
#[derive(Debug)]
pub struct OutputTooLarge {
    pub limit: usize,
}
impl std::fmt::Display for OutputTooLarge {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "Machine output exceeds {} bytes; complete output was not captured",
            self.limit
        )
    }
}
impl std::error::Error for OutputTooLarge {}
async fn bounded_read(mut r: impl AsyncRead + Unpin, limit: usize) -> std::io::Result<Captured> {
    let mut kept = Vec::new();
    let mut buf = [0; 8192];
    let mut truncated = false;
    loop {
        let n = r.read(&mut buf).await?;
        if n == 0 {
            break;
        }
        let remaining = limit.saturating_sub(kept.len());
        truncated |= n > remaining;
        kept.extend_from_slice(&buf[..n.min(remaining)]);
    }
    Ok(Captured {
        bytes: kept,
        truncated,
    })
}
pub async fn capture(
    binary: &str,
    args: &[&str],
    cwd: &Path,
    seconds: u64,
    cancel: &CancellationToken,
    mode: CaptureMode,
) -> Result<ProcessOutput> {
    anyhow::ensure!(!cancel.is_cancelled(), "Operation cancelled");
    let mut child = GroupChild::new(
        command(binary, cwd)
            .args(args)
            .spawn()
            .with_context(|| format!("Could not start {binary}"))?,
    );
    let stdout = child.0.stdout.take().unwrap();
    let stderr = child.0.stderr.take().unwrap();
    let limit = match mode {
        CaptureMode::Diagnostic => DIAGNOSTIC_LIMIT,
        CaptureMode::Machine => MACHINE_LIMIT,
    };
    let future = async {
        let (status, out, err) = tokio::join!(
            async move {
                let status = child.0.wait().await;
                // Stop owned descendants as soon as the leader finishes, so their
                // inherited pipes reach EOF. Readers still drain buffered output.
                drop(child);
                status
            },
            bounded_read(stdout, limit),
            bounded_read(stderr, DIAGNOSTIC_LIMIT)
        );
        Ok(ProcessOutput {
            status: status?,
            stdout: out?,
            stderr: err?,
        })
    };
    tokio::select! {
        result=tokio::time::timeout(Duration::from_secs(seconds),future)=>result.context("Command timed out")?,
        _=cancel.cancelled()=>bail!("Operation cancelled")
    }
}
fn ensure_success(binary: &str, output: &ProcessOutput) -> Result<()> {
    if !output.status.success() {
        bail!(
            "{binary} exited with {}: {}",
            output.status,
            crate::store::redact(&format!(
                "{}\n{}",
                output.stdout.preview(),
                output.stderr.preview()
            ))
        );
    }
    Ok(())
}
// Human-readable evidence: bounded stdout, with bounded stderr appended when present.
pub(crate) fn diagnostic_text(binary: &str, output: &ProcessOutput) -> Result<String> {
    ensure_success(binary, output)?;
    let mut text = output.stdout.preview().trim().to_owned();
    let stderr = output.stderr.preview();
    let stderr = stderr.trim();
    if !stderr.is_empty() {
        text.push_str("\n[stderr]\n");
        text.push_str(stderr);
    }
    Ok(text)
}
async fn checked(
    binary: &str,
    args: &[&str],
    cwd: &Path,
    seconds: u64,
    cancel: &CancellationToken,
    mode: CaptureMode,
) -> Result<String> {
    let output = capture(binary, args, cwd, seconds, cancel, mode).await?;
    match mode {
        CaptureMode::Diagnostic => diagnostic_text(binary, &output),
        CaptureMode::Machine => {
            if output.stdout.truncated {
                return Err(OutputTooLarge {
                    limit: MACHINE_LIMIT,
                }
                .into());
            }
            String::from_utf8(output.stdout.bytes).context("Machine output is not valid UTF-8")
        }
    }
}
/// Runs a command for human-readable evidence, keeping bounded stdout and stderr on success.
pub async fn run(
    binary: &str,
    args: &[&str],
    cwd: &Path,
    seconds: u64,
    cancel: &CancellationToken,
) -> Result<String> {
    checked(binary, args, cwd, seconds, cancel, CaptureMode::Diagnostic).await
}
pub async fn run_machine(
    binary: &str,
    args: &[&str],
    cwd: &Path,
    seconds: u64,
    cancel: &CancellationToken,
) -> Result<String> {
    checked(binary, args, cwd, seconds, cancel, CaptureMode::Machine).await
}

// Bounded window for a cancelled future to unwind before the caller reports expiry.
const GRACE: Duration = Duration::from_secs(8);

pub enum Deadline<T> {
    Done(T),
    Expired { already_cancelled: bool },
}

/// Runs `future` under `limit`. On expiry, cancels `cancel` and then waits a bounded
/// grace period: the cancelled token prevents new turns and publication commands while
/// runner abort handlers stop their own detached process groups. `already_cancelled`
/// separates an operator cancellation from a genuine deadline.
pub async fn with_deadline<F: Future>(
    limit: Duration,
    cancel: &CancellationToken,
    future: F,
) -> Deadline<F::Output> {
    tokio::pin!(future);
    match tokio::time::timeout(limit, &mut future).await {
        Ok(output) => Deadline::Done(output),
        Err(_) => {
            let already_cancelled = cancel.is_cancelled();
            cancel.cancel();
            let _ = tokio::time::timeout(GRACE, &mut future).await;
            Deadline::Expired { already_cancelled }
        }
    }
}

/// Runs `future` until it completes, `seconds` elapse, or `cancel` fires. `what` is
/// the complete timeout message so callers keep their existing wording.
pub async fn bounded<F: Future>(
    seconds: u64,
    cancel: &CancellationToken,
    what: &str,
    future: F,
) -> Result<F::Output> {
    bounded_at(
        tokio::time::Instant::now() + Duration::from_secs(seconds),
        cancel,
        what,
        future,
    )
    .await
}

/// `bounded` against an absolute deadline.
pub async fn bounded_at<F: Future>(
    deadline: tokio::time::Instant,
    cancel: &CancellationToken,
    what: &str,
    future: F,
) -> Result<F::Output> {
    tokio::select! {
        result = tokio::time::timeout_at(deadline, future) => {
            result.with_context(|| what.to_owned())
        }
        _ = cancel.cancelled() => bail!("Session cancelled"),
    }
}
