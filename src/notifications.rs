use crate::{engine::App, store::notifications::NotificationDelivery};
use anyhow::{Context, Result, bail, ensure};
use chrono::Utc;
use serde::Serialize;
use sha2::{Digest, Sha256};
use std::time::Duration;
use tokio::task::JoinHandle;
use tokio_util::sync::CancellationToken;

pub const WEBHOOK_ENV: &str = "OCTOMUS_NOTIFICATION_WEBHOOK_URL";
const MAX_URL_BYTES: usize = 8192;
const MAX_PAYLOAD_BYTES: usize = 8192;
const MAX_REPOSITORY_BYTES: usize = 256;
const MAX_ID_BYTES: usize = 128;

struct WebhookDestination {
    url: reqwest::Url,
    id: String,
}
impl WebhookDestination {
    fn parse(raw: &str) -> Result<Self> {
        let raw = raw.trim();
        ensure!(
            !raw.is_empty() && raw.len() <= MAX_URL_BYTES,
            "Notification webhook URL is empty or exceeds the 8192-byte limit"
        );
        let url =
            reqwest::Url::parse(raw).context("Notification webhook URL is not a valid URL")?;
        ensure!(
            url.username().is_empty() && url.password().is_none(),
            "Notification webhook URL must not contain credentials"
        );
        ensure!(
            url.fragment().is_none(),
            "Notification webhook URL must not contain a fragment"
        );
        let host = url
            .host()
            .context("Notification webhook URL must contain a host")?;
        match url.scheme() {
            "https" => {}
            "http" => ensure!(
                match host {
                    url::Host::Ipv4(ip) => ip.is_loopback(),
                    url::Host::Ipv6(ip) => ip.is_loopback(),
                    url::Host::Domain(_) => false,
                },
                "Plain HTTP notification webhooks require a loopback IP address"
            ),
            _ => bail!("Notification webhook URL must use https, or http for a loopback IP"),
        }
        Ok(Self {
            id: format!("{:x}", Sha256::digest(url.as_str().as_bytes())),
            url,
        })
    }
}

#[derive(Serialize)]
pub struct AttentionEvent {
    pub schema_version: u32,
    pub event_id: String,
    pub occurred_at: String,
    pub repository: String,
    pub cycle_id: Option<String>,
    pub run_id: Option<String>,
    pub task_id: Option<String>,
    pub category: String,
    pub action: String,
}

pub fn payload(delivery: &NotificationDelivery) -> Result<Vec<u8>> {
    let event = AttentionEvent {
        schema_version: 1,
        event_id: delivery.event_id.clone(),
        occurred_at: delivery.created_at.clone(),
        repository: delivery.repository.clone(),
        cycle_id: delivery.cycle_id.clone(),
        run_id: delivery.run_id.clone(),
        task_id: delivery.task_id.clone(),
        category: delivery.category.clone(),
        action: delivery.action.clone(),
    };
    ensure!(
        event.repository.len() <= MAX_REPOSITORY_BYTES
            && event.event_id.len() <= MAX_ID_BYTES
            && event.cycle_id.as_deref().unwrap_or("").len() <= MAX_ID_BYTES
            && event.run_id.as_deref().unwrap_or("").len() <= MAX_ID_BYTES
            && event.task_id.as_deref().unwrap_or("").len() <= MAX_ID_BYTES
            && event.category.len() <= MAX_ID_BYTES
            && event.action.len() <= MAX_ID_BYTES,
        "invalid_payload"
    );
    let bytes = serde_json::to_vec(&event).context("invalid_payload")?;
    ensure!(bytes.len() <= MAX_PAYLOAD_BYTES, "invalid_payload");
    Ok(bytes)
}

fn classify(status: u16) -> (&'static str, bool) {
    if status == 408 || status == 429 || status >= 500 {
        ("http_status", true)
    } else {
        ("http_status", false)
    }
}

async fn deliver(
    client: &reqwest::Client,
    destination: &WebhookDestination,
    delivery: &NotificationDelivery,
) -> Result<u16, &'static str> {
    let body = payload(delivery).map_err(|_| "invalid_payload")?;
    let response = client
        .post(destination.url.clone())
        .header("content-type", "application/json")
        .body(body)
        .send()
        .await
        .map_err(|error| {
            if error.is_timeout() {
                "timeout"
            } else {
                "transport_error"
            }
        })?;
    let status = response.status().as_u16();
    drop(response);
    Ok(status)
}

pub fn start(app: &App, configured_url: Option<String>) -> Result<JoinHandle<()>> {
    let (destination, state, error) = match configured_url
        .as_deref()
        .map(str::trim)
        .filter(|value| !value.is_empty())
    {
        None => (None, "disabled", None),
        Some(raw) => match WebhookDestination::parse(raw) {
            Ok(destination) => (Some(destination), "enabled", None),
            Err(error) => (None, "invalid", Some(error.to_string())),
        },
    };
    let client = destination.as_ref().map(|_| {
        reqwest::Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .no_proxy()
            .timeout(Duration::from_secs(10))
            .connect_timeout(Duration::from_secs(5))
            .build()
    });
    let (destination, state, error, client) = match client {
        Some(Ok(client)) => (destination, state, error, Some(client)),
        Some(Err(_)) => (
            None,
            "invalid",
            Some("Notification webhook client could not be configured".to_owned()),
            None,
        ),
        None => (destination, state, error, None),
    };
    app.store.configure_notifications(
        destination.as_ref().map(|d| d.id.as_str()),
        state,
        error.as_deref(),
    )?;
    let app = app.clone();
    let shutdown: CancellationToken = app.shutdown.child_token();
    Ok(tokio::spawn(async move {
        let Some((destination, client)) = destination.zip(client) else {
            shutdown.cancelled().await;
            return;
        };
        let mut interval = tokio::time::interval(Duration::from_secs(1));
        interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        loop {
            tokio::select! {
                _ = shutdown.cancelled() => break,
                _ = interval.tick() => {}
            }
            if shutdown.is_cancelled() {
                break;
            }
            let delivery = match app.store.claim_notification(&destination.id, Utc::now()) {
                Ok(delivery) => delivery,
                Err(_) => {
                    tracing::warn!("Notification outbox claim failed");
                    continue;
                }
            };
            let Some(delivery) = delivery else { continue };
            let outcome = tokio::select! {
                _ = shutdown.cancelled() => return,
                outcome = deliver(&client, &destination, &delivery) => outcome,
            };
            let result = match outcome {
                Err(category) => app.store.finish_notification_failure(
                    delivery.seq,
                    category,
                    None,
                    category != "invalid_payload",
                ),
                Ok(status) if (200..300).contains(&status) => app
                    .store
                    .finish_notification_delivered(delivery.seq, Utc::now()),
                Ok(status) => {
                    let (category, retryable) = classify(status);
                    app.store.finish_notification_failure(
                        delivery.seq,
                        category,
                        Some(status),
                        retryable,
                    )
                }
            };
            if result.is_err() {
                tracing::warn!("Notification delivery update failed");
            }
        }
    }))
}
