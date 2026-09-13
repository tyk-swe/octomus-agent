use octomus_agent::process::{self, CaptureMode};
use std::{path::PathBuf, time::Duration};
use tokio_util::sync::CancellationToken;

// Independent of capture's cleanup, including when an assertion unwinds.
struct FixtureGroup(PathBuf);
impl Drop for FixtureGroup {
    fn drop(&mut self) {
        if let Ok(pid) = std::fs::read_to_string(&self.0)
            && let Ok(pid) = pid.trim().parse::<i32>()
        {
            unsafe { libc::kill(-pid, libc::SIGKILL) };
        }
    }
}

async fn inherited_pipe(pipe: &str, exit_code: i32) {
    let temp = tempfile::tempdir().unwrap();
    let _cleanup = FixtureGroup(temp.path().join("group.pid"));
    let script = r#"
import os, pathlib, sys, time
pathlib.Path('group.pid').write_text(str(os.getpid()))
ready_read, ready_write = os.pipe()
pid = os.fork()
if pid == 0:
    os.close(ready_read)
    os.close(2 if sys.argv[1] == 'stdout' else 1)
    os.write(ready_write, b'ready')
    os.close(ready_write)
    time.sleep(60)
    os._exit(0)
os.close(ready_write)
assert os.read(ready_read, 5) == b'ready'
os.close(ready_read)
pathlib.Path('child.pid').write_text(str(pid))
sys.stdout.write('o' * 131072 + 'stdout end\n')
sys.stdout.flush()
sys.stderr.write('e' * 131072 + 'stderr end\n')
sys.stderr.flush()
sys.exit(int(sys.argv[2]))
"#;
    // The descendant outlives this generous deadline unless group cleanup runs.
    let output = tokio::time::timeout(
        Duration::from_secs(15),
        process::capture(
            "python3",
            &["-c", script, pipe, &exit_code.to_string()],
            temp.path(),
            10,
            &CancellationToken::new(),
            CaptureMode::Diagnostic,
        ),
    )
    .await
    .expect("capture exceeded its outer deadline")
    .expect("completed leader must not time out on descendant-held pipes");
    assert_eq!(output.status.code(), Some(exit_code));
    assert_eq!(
        output.stdout.bytes,
        format!("{}stdout end\n", "o".repeat(131072)).as_bytes()
    );
    assert_eq!(
        output.stderr.bytes,
        format!("{}stderr end\n", "e".repeat(131072)).as_bytes()
    );
    assert!(!output.stdout.truncated && !output.stderr.truncated);

    let pid = std::fs::read_to_string(temp.path().join("child.pid")).unwrap();
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            match std::fs::read_to_string(format!("/proc/{pid}/stat")) {
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => break,
                Ok(stat) if stat.split_whitespace().nth(2) == Some("Z") => break,
                Err(error) => panic!("could not inspect descendant: {error}"),
                Ok(_) => tokio::time::sleep(Duration::from_millis(20)).await,
            }
        }
    })
    .await
    .expect("descendant survived leader completion");
}

#[tokio::test]
async fn inherited_stdout_zero_exit() {
    inherited_pipe("stdout", 0).await;
}

#[tokio::test]
async fn inherited_stdout_nonzero_exit() {
    inherited_pipe("stdout", 23).await;
}

#[tokio::test]
async fn inherited_stderr_zero_exit() {
    inherited_pipe("stderr", 0).await;
}

#[tokio::test]
async fn inherited_stderr_nonzero_exit() {
    inherited_pipe("stderr", 23).await;
}
