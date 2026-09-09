//! Best-effort outbound notifications: one JSON POST per operator-relevant event.
//! Delivery never blocks the scheduler, never carries the operator token, and never
//! changes control state; failures are recorded as events.
use crate::{engine::App, model::now, process, store::Store};
use anyhow::{Context, Result};
use serde_json::{Value, json};
use std::{path::PathBuf, time::Duration};
use tokio_util::sync::CancellationToken;

pub const DELIVERY_TIMEOUT_SECONDS: u64 = 10;

/// Empty, or an http(s) URL of bounded length whose authority carries no user information.
pub fn valid_url(url: &str) -> bool {
    if url.is_empty() {
        return true;
    }
    let Some(rest) = url
        .strip_prefix("https://")
        .or_else(|| url.strip_prefix("http://"))
    else {
        return false;
    };
    let authority = rest.split(['/', '?', '#']).next().unwrap_or("");
    url.len() <= 2048
        && !authority.is_empty()
        && !authority.contains('@')
        && !url.chars().any(|c| c.is_whitespace() || c.is_control())
}

pub fn payload(event: &str, repository: &str, detail: Value) -> Value {
    let mut value = json!({"event":event,"at":now(),"repository":repository,"detail":detail});
    crate::store::redact_json(&mut value);
    value
}

impl App {
    /// Queue one notification for the configured webhook; a no-op without a URL.
    pub fn notify(&self, event: &str, detail: Value) {
        let Ok(config) = self.config() else {
            return;
        };
        if config.notification_url.is_empty() {
            return;
        }
        let payload = payload(event, &config.github_repo, detail);
        let delivery = Delivery {
            url: config.notification_url,
            data_dir: self.data_dir.clone(),
            store: self.store.clone(),
            event: event.to_owned(),
            cancel: self.shutdown.child_token(),
        };
        tokio::spawn(delivery.run(payload));
    }
}

struct Delivery {
    url: String,
    data_dir: PathBuf,
    store: Store,
    event: String,
    cancel: CancellationToken,
}
impl Delivery {
    async fn run(self, payload: Value) {
        let mut last = None;
        for attempt in 0..2 {
            if attempt > 0 {
                tokio::select! {
                    _ = tokio::time::sleep(Duration::from_secs(5)) => {},
                    _ = self.cancel.cancelled() => return,
                }
            }
            match self.post(&payload).await {
                Ok(()) => return,
                Err(error) => last = Some(error),
            }
        }
        let _ = self.store.event(
            "system",
            "notification",
            &format!(
                "Delivery of {} failed: {:#}",
                self.event,
                last.expect("an error after failed attempts")
            ),
        );
    }
    async fn post(&self, payload: &Value) -> Result<()> {
        let dir = self.data_dir.join("notifications");
        tokio::fs::create_dir_all(&dir).await?;
        let file = dir.join(format!("{}.json", crate::model::id()));
        tokio::fs::write(&file, serde_json::to_vec(payload)?).await?;
        let body = format!("@{}", file.to_str().context("Non UTF-8 data directory")?);
        let result = process::run(
            "curl",
            &[
                "-fsS",
                "-o",
                "/dev/null",
                "-m",
                &DELIVERY_TIMEOUT_SECONDS.to_string(),
                "--max-redirs",
                "0",
                "-X",
                "POST",
                "-H",
                "Content-Type: application/json",
                "-H",
                concat!("User-Agent: octomus-agent/", env!("CARGO_PKG_VERSION")),
                "--data-binary",
                &body,
                &self.url,
            ],
            &self.data_dir,
            DELIVERY_TIMEOUT_SECONDS + 5,
            &self.cancel,
        )
        .await;
        let _ = tokio::fs::remove_file(&file).await;
        result.map(|_| ())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn payload_is_redacted_and_shaped() {
        let v = payload(
            "task_blocked",
            "owner/repo",
            json!({"error":"Bearer secretkey123 ghp_abcdefghijklmnop","title":"x"}),
        );
        assert_eq!(v["event"], "task_blocked");
        assert_eq!(v["repository"], "owner/repo");
        assert!(v["at"].as_str().is_some_and(|s| !s.is_empty()));
        let error = v["detail"]["error"].as_str().unwrap();
        assert!(!error.contains("secretkey") && !error.contains("ghp_abc"));
        assert_eq!(v["detail"]["title"], "x");
    }
    #[test]
    fn urls_are_plain_http_without_credentials() {
        assert!(valid_url(""));
        assert!(valid_url("https://127.0.0.1:1/hook"));
        assert!(valid_url("http://ntfy.internal/octomus"));
        assert!(valid_url("https://host"));
        assert!(valid_url("https://host/hook?token=1"));
        assert!(!valid_url("ftp://x"));
        assert!(!valid_url("https://"));
        assert!(!valid_url("https://u:p@host/hook"));
        assert!(!valid_url("https://user@example.com/hook"));
        assert!(!valid_url("http://@host/x"));
        assert!(!valid_url("https://host/hook with space"));
        assert!(!valid_url(&format!("https://h/{}", "a".repeat(2048))));
    }
}
