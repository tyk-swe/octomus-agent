use anyhow::Result;
use chrono::{DateTime, Utc};
use rusqlite::{Connection, OptionalExtension, params};
use serde::Serialize;
use serde_json::{Value, json};

pub const QUEUE_CAP: i64 = 1000;
pub const MAX_ATTEMPTS: i64 = 5;
pub const EXPIRY_SECONDS: i64 = 24 * 60 * 60;
pub const RETRY_DELAY_SECONDS: [i64; 5] = [30, 120, 600, 1800, 1800];

#[derive(Debug, Clone, Serialize)]
pub struct NotificationDelivery {
    pub seq: i64,
    pub event_id: String,
    pub created_at: String,
    pub repository: String,
    pub cycle_id: Option<String>,
    pub run_id: Option<String>,
    pub task_id: Option<String>,
    pub category: String,
    pub action: String,
    pub attempts: i64,
}

const TASK_CATEGORY: &str = "CASE WHEN COALESCE(json_extract(NEW.data,'$.blocked_reason'),'') IN ('budget_exhausted','storage_limit','stale_base','remote_conflict','publication_uncertain','runner_unavailable','invalid_review','verification_failed','dependency_blocked','invalid_plan','workspace_invalid','retry_limit','timeout') THEN json_extract(NEW.data,'$.blocked_reason') ELSE 'unknown' END";

const OVERFLOW: &str = "UPDATE notification_outbox SET status='failed', last_error='queue_overflow' WHERE seq IN (SELECT seq FROM notification_outbox WHERE status='pending' ORDER BY seq DESC LIMIT -1 OFFSET 1000)";

pub(super) fn migrate(c: &Connection) -> Result<()> {
    c.execute_batch(&format!(
        r#"
        CREATE TABLE IF NOT EXISTS notification_policy (
            id INTEGER PRIMARY KEY CHECK(id=1), destination_id TEXT, enabled INTEGER NOT NULL DEFAULT 0,
            state TEXT NOT NULL DEFAULT 'disabled', error TEXT
        );
        CREATE TABLE IF NOT EXISTS notification_outbox (
            seq INTEGER PRIMARY KEY AUTOINCREMENT, event_id TEXT NOT NULL UNIQUE,
            destination_id TEXT NOT NULL, created_at TEXT NOT NULL,
            repository TEXT NOT NULL, cycle_id TEXT, run_id TEXT, task_id TEXT,
            category TEXT NOT NULL, action TEXT NOT NULL,
            status TEXT NOT NULL DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0,
            next_attempt_at INTEGER NOT NULL, last_attempt_at TEXT,
            delivered_at TEXT, last_error TEXT, http_status INTEGER
        );
        CREATE INDEX IF NOT EXISTS notification_due ON notification_outbox(destination_id,status,next_attempt_at,seq);
        CREATE TRIGGER IF NOT EXISTS notify_task_insert AFTER INSERT ON records
        WHEN NEW.kind='task'
            AND (SELECT enabled FROM notification_policy WHERE id=1)=1
            AND json_extract(NEW.data,'$.status') IN ('blocked','failed')
        BEGIN
            INSERT INTO notification_outbox
                (event_id,destination_id,created_at,repository,cycle_id,run_id,task_id,category,action,status,attempts,next_attempt_at)
                SELECT lower(hex(randomblob(16))), destination_id, strftime('%Y-%m-%dT%H:%M:%fZ','now'),
                    COALESCE(json_extract(NEW.data,'$.config.github_repo'),''),
                    json_extract(NEW.data,'$.cycle_id'), json_extract(NEW.data,'$.run_id'), NEW.id,
                    {TASK_CATEGORY}, 'inspect_task', 'pending', 0, unixepoch('now')
                FROM notification_policy WHERE id=1;
            {OVERFLOW};
        END;
        CREATE TRIGGER IF NOT EXISTS notify_task_update AFTER UPDATE ON records
        WHEN NEW.kind='task'
            AND (SELECT enabled FROM notification_policy WHERE id=1)=1
            AND json_extract(NEW.data,'$.status') IN ('blocked','failed')
            AND COALESCE(json_extract(OLD.data,'$.status'),'') NOT IN ('blocked','failed')
        BEGIN
            INSERT INTO notification_outbox
                (event_id,destination_id,created_at,repository,cycle_id,run_id,task_id,category,action,status,attempts,next_attempt_at)
                SELECT lower(hex(randomblob(16))), destination_id, strftime('%Y-%m-%dT%H:%M:%fZ','now'),
                    COALESCE(json_extract(NEW.data,'$.config.github_repo'),''),
                    json_extract(NEW.data,'$.cycle_id'), json_extract(NEW.data,'$.run_id'), NEW.id,
                    {TASK_CATEGORY}, 'inspect_task', 'pending', 0, unixepoch('now')
                FROM notification_policy WHERE id=1;
            {OVERFLOW};
        END;
        CREATE TRIGGER IF NOT EXISTS notify_control_insert AFTER INSERT ON records
        WHEN NEW.kind='settings' AND NEW.id='control'
            AND (SELECT enabled FROM notification_policy WHERE id=1)=1
            AND COALESCE(json_extract(NEW.data,'$.paused'),0)=1
            AND json_extract(NEW.data,'$.error') IS NOT NULL
        BEGIN
            INSERT INTO notification_outbox
                (event_id,destination_id,created_at,repository,cycle_id,run_id,task_id,category,action,status,attempts,next_attempt_at)
                SELECT lower(hex(randomblob(16))), destination_id, strftime('%Y-%m-%dT%H:%M:%fZ','now'),
                    COALESCE(json_extract((SELECT data FROM records WHERE kind='settings' AND id='config'),'$.github_repo'),''),
                    json_extract(NEW.data,'$.batch.cycle_id'), json_extract(NEW.data,'$.batch.id'), NULL,
                    'service_error_paused', 'inspect_service', 'pending', 0, unixepoch('now')
                FROM notification_policy WHERE id=1;
            {OVERFLOW};
        END;
        CREATE TRIGGER IF NOT EXISTS notify_control_update AFTER UPDATE ON records
        WHEN NEW.kind='settings' AND NEW.id='control'
            AND (SELECT enabled FROM notification_policy WHERE id=1)=1
            AND COALESCE(json_extract(NEW.data,'$.paused'),0)=1
            AND json_extract(NEW.data,'$.error') IS NOT NULL
            AND NOT (COALESCE(json_extract(OLD.data,'$.paused'),0)=1 AND json_extract(OLD.data,'$.error') IS NOT NULL)
        BEGIN
            INSERT INTO notification_outbox
                (event_id,destination_id,created_at,repository,cycle_id,run_id,task_id,category,action,status,attempts,next_attempt_at)
                SELECT lower(hex(randomblob(16))), destination_id, strftime('%Y-%m-%dT%H:%M:%fZ','now'),
                    COALESCE(json_extract((SELECT data FROM records WHERE kind='settings' AND id='config'),'$.github_repo'),''),
                    json_extract(NEW.data,'$.batch.cycle_id'), json_extract(NEW.data,'$.batch.id'), NULL,
                    'service_error_paused', 'inspect_service', 'pending', 0, unixepoch('now')
                FROM notification_policy WHERE id=1;
            {OVERFLOW};
        END;
        "#
    ))?;
    Ok(())
}

impl super::Store {
    pub fn configure_notifications(
        &self,
        destination: Option<&str>,
        state: &str,
        error: Option<&str>,
    ) -> Result<()> {
        let mut connection = self.conn();
        let transaction = connection.transaction()?;
        let enabled = (state == "enabled" && destination.is_some()) as i64;
        transaction.execute(
            "UPDATE notification_outbox SET status='cancelled', last_error='destination_changed'
             WHERE status='pending' AND (?1=0 OR destination_id IS NOT ?2)",
            params![enabled, destination],
        )?;
        transaction.execute(
            "INSERT INTO notification_policy VALUES (1,?2,?1,?3,?4)
             ON CONFLICT(id) DO UPDATE SET destination_id=excluded.destination_id, enabled=excluded.enabled, state=excluded.state, error=excluded.error",
            params![enabled, destination, state, error],
        )?;
        transaction.commit()?;
        Ok(())
    }
    pub fn claim_notification(
        &self,
        destination: &str,
        now: DateTime<Utc>,
    ) -> Result<Option<NotificationDelivery>> {
        let mut connection = self.conn();
        let transaction = connection.transaction()?;
        let epoch = now.timestamp();
        transaction.execute(
            "UPDATE notification_outbox SET status='failed', last_error='expired' WHERE status='pending' AND unixepoch(created_at) < ?1",
            params![epoch - EXPIRY_SECONDS],
        )?;
        transaction.execute(
            "UPDATE notification_outbox SET status='failed', last_error='delivery_uncertain' WHERE status='pending' AND attempts >= ?1",
            params![MAX_ATTEMPTS],
        )?;
        let row = transaction
            .query_row(
                "SELECT seq,event_id,created_at,repository,cycle_id,run_id,task_id,category,action,attempts
                 FROM notification_outbox
                 WHERE status='pending' AND next_attempt_at <= ?1 AND destination_id=?2
                     AND destination_id = (SELECT destination_id FROM notification_policy WHERE id=1 AND enabled=1)
                 ORDER BY seq LIMIT 1",
                params![epoch, destination],
                |r| {
                    Ok(NotificationDelivery {
                        seq: r.get(0)?,
                        event_id: r.get(1)?,
                        created_at: r.get(2)?,
                        repository: r.get(3)?,
                        cycle_id: r.get(4)?,
                        run_id: r.get(5)?,
                        task_id: r.get(6)?,
                        category: r.get(7)?,
                        action: r.get(8)?,
                        attempts: r.get(9)?,
                    })
                },
            )
            .optional()?;
        let row = if let Some(mut delivery) = row {
            let attempt = delivery.attempts + 1;
            let delay = RETRY_DELAY_SECONDS[(attempt - 1).min(MAX_ATTEMPTS - 1) as usize];
            transaction.execute(
                "UPDATE notification_outbox SET attempts=?1, last_attempt_at=?2, next_attempt_at=?3 WHERE seq=?4",
                params![attempt, now.to_rfc3339(), epoch + delay, delivery.seq],
            )?;
            delivery.attempts = attempt;
            Some(delivery)
        } else {
            None
        };
        transaction.execute(
            "DELETE FROM notification_outbox WHERE status != 'pending' AND seq NOT IN (
                SELECT seq FROM notification_outbox WHERE status != 'pending' ORDER BY seq DESC
                LIMIT COALESCE(json_extract((SELECT data FROM records WHERE kind='settings' AND id='config'),'$.retain_events'),10000))",
            [],
        )?;
        transaction.commit()?;
        Ok(row)
    }
    pub fn finish_notification_delivered(&self, seq: i64, now: DateTime<Utc>) -> Result<()> {
        self.conn().execute(
            "UPDATE notification_outbox SET status='delivered', delivered_at=?1, last_error=NULL, http_status=NULL WHERE seq=?2 AND status='pending'",
            params![now.to_rfc3339(), seq],
        )?;
        Ok(())
    }
    pub fn finish_notification_failure(
        &self,
        seq: i64,
        category: &str,
        http_status: Option<u16>,
        retryable: bool,
    ) -> Result<()> {
        self.conn().execute(
            "UPDATE notification_outbox SET status=CASE WHEN ?1=1 AND attempts<?2 THEN 'pending' ELSE 'failed' END, last_error=?3, http_status=?4 WHERE seq=?5 AND status='pending'",
            params![retryable as i64, MAX_ATTEMPTS, category, http_status, seq],
        )?;
        Ok(())
    }
    pub fn notification_health(&self) -> Result<Value> {
        let connection = self.conn();
        let policy = connection
            .query_row(
                "SELECT state, state!='disabled', error FROM notification_policy WHERE id=1",
                [],
                |r| {
                    Ok((
                        r.get::<_, String>(0)?,
                        r.get::<_, bool>(1)?,
                        r.get::<_, Option<String>>(2)?,
                    ))
                },
            )
            .optional()?;
        let (state, configured, policy_error) =
            policy.unwrap_or_else(|| ("disabled".into(), false, None));
        let pending: i64 = connection.query_row(
            "SELECT COUNT(*) FROM notification_outbox WHERE status='pending'",
            [],
            |r| r.get(0),
        )?;
        let failed: i64 = connection.query_row(
            "SELECT COUNT(*) FROM notification_outbox WHERE status='failed'",
            [],
            |r| r.get(0),
        )?;
        let last_delivered_at: Option<String> = connection.query_row(
            "SELECT MAX(delivered_at) FROM notification_outbox WHERE status='delivered'",
            [],
            |r| r.get(0),
        )?;
        let (last_error, last_http_status) = connection
            .query_row(
                "SELECT last_error, http_status FROM notification_outbox WHERE last_error IS NOT NULL OR http_status IS NOT NULL ORDER BY seq DESC LIMIT 1",
                [],
                |r| Ok((r.get::<_, Option<String>>(0)?, r.get::<_, Option<i64>>(1)?)),
            )
            .optional()?
            .unwrap_or((None, None));
        Ok(json!({
            "state": state,
            "configured": configured,
            "pending": pending,
            "failed": failed,
            "last_delivered_at": last_delivered_at,
            "last_error": policy_error.or(last_error),
            "last_http_status": last_http_status,
        }))
    }
}
