use crate::model::{Event, now};
use anyhow::{Context, Result};
use rusqlite::{Connection, OpenFlags, OptionalExtension, params};

mod capacity;
mod migrate;
pub mod notifications;
mod queries;
pub use capacity::{PrReservation, pr_union};
pub use notifications::NotificationDelivery;
pub use queries::{HistoryQuery, Page};
use serde::{Deserialize, Serialize, de::DeserializeOwned};
use std::{
    path::Path,
    sync::{Arc, Mutex, MutexGuard},
    time::Duration,
};

/// A budget admission, not a completed turn or a provider charge.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Admission {
    pub id: String,
    pub at: String,
    pub cycle_id: String,
    pub task_id: Option<String>,
    pub role: String,
    pub route: crate::config::Route,
}
impl Admission {
    pub fn new(
        cycle_id: &str,
        task_id: Option<&str>,
        role: &str,
        route: &crate::config::Route,
    ) -> Self {
        Self {
            id: crate::model::id(),
            at: now(),
            cycle_id: cycle_id.into(),
            task_id: task_id.map(str::to_owned),
            role: role.into(),
            route: route.clone(),
        }
    }
}

#[derive(Clone)]
pub struct Store(Arc<Mutex<Connection>>);
impl Store {
    fn conn(&self) -> MutexGuard<'_, Connection> {
        self.0.lock().unwrap_or_else(|e| e.into_inner())
    }
    pub fn open(path: &Path) -> Result<Self> {
        let c = Connection::open(path)?;
        c.busy_timeout(std::time::Duration::from_secs(5))?;
        c.execute_batch(
            r#"
            PRAGMA journal_mode=WAL;
            PRAGMA synchronous=FULL;
            CREATE TABLE IF NOT EXISTS records (
                kind TEXT NOT NULL, id TEXT NOT NULL, data TEXT NOT NULL,
                PRIMARY KEY(kind,id)
            );
            CREATE TABLE IF NOT EXISTS events (
                id INTEGER PRIMARY KEY AUTOINCREMENT, at TEXT NOT NULL,
                entity_id TEXT NOT NULL, kind TEXT NOT NULL, message TEXT NOT NULL
            );
            CREATE TABLE IF NOT EXISTS usage (
                day TEXT PRIMARY KEY, sessions INTEGER NOT NULL
            );
            CREATE TABLE IF NOT EXISTS admissions (
                id TEXT PRIMARY KEY, at TEXT NOT NULL, day TEXT NOT NULL, data TEXT NOT NULL
            );
            CREATE INDEX IF NOT EXISTS admissions_day ON admissions(day);
            -- Retain only the most recent configured number of events.
            CREATE TRIGGER IF NOT EXISTS cap_activity AFTER INSERT ON events BEGIN
                DELETE FROM events WHERE id <= NEW.id - COALESCE(json_extract(
                    (SELECT data FROM records WHERE kind='settings' AND id='config'),
                    '$.retain_events'
                ),10000);
            END;
            "#,
        )?;
        migrate::migrate(&c)?;
        notifications::migrate(&c)?;
        Ok(Self(Arc::new(Mutex::new(c))))
    }
    pub fn put<T: Serialize>(&self, kind: &str, id: &str, value: &T) -> Result<()> {
        self.conn().execute(
            "INSERT INTO records VALUES (?1,?2,?3)
             ON CONFLICT(kind,id) DO UPDATE SET data=excluded.data",
            params![kind, id, serde_json::to_string(value)?],
        )?;
        Ok(())
    }
    pub fn commit_plan(
        &self,
        cycle: &crate::model::Cycle,
        tasks: &[crate::model::Task],
    ) -> Result<()> {
        let mut connection = self.conn();
        let transaction = connection.transaction()?;
        transaction.execute(
            "INSERT INTO records VALUES ('cycle',?1,?2)
             ON CONFLICT(kind,id) DO UPDATE SET data=excluded.data",
            params![cycle.id, serde_json::to_string(cycle)?],
        )?;
        for task in tasks {
            transaction.execute(
                "INSERT INTO records VALUES ('task',?1,?2)",
                params![task.id, serde_json::to_string(task)?],
            )?;
        }
        let saved: Option<String> = transaction
            .query_row(
                "SELECT data FROM records WHERE kind='settings' AND id='control'",
                [],
                |r| r.get(0),
            )
            .optional()?;
        if let Some(saved) = saved {
            let mut control: crate::model::Control = serde_json::from_str(&saved)?;
            if let Some(batch) = &mut control.batch
                && cycle.run_id.as_deref() == Some(&batch.id)
                && batch.cycle_id.as_deref() == Some(&cycle.id)
            {
                batch.phase = crate::model::BatchPhase::Executing;
                transaction.execute(
                    "UPDATE records SET data=?1 WHERE kind='settings' AND id='control'",
                    [serde_json::to_string(&control)?],
                )?;
            }
        }
        for task in tasks {
            for old_id in &task.supersedes {
                let data: String = transaction
                    .query_row(
                        "SELECT data FROM records WHERE kind='task' AND id=?1",
                        [old_id],
                        |r| r.get(0),
                    )
                    .with_context(|| format!("Missing lineage task {old_id}"))?;
                let mut old: crate::model::Task = serde_json::from_str(&data)?;
                anyhow::ensure!(
                    old.rediscovery_requested
                        && old
                            .config
                            .github_repo
                            .eq_ignore_ascii_case(&task.config.github_repo),
                    "Invalid rediscovery lineage"
                );
                old.superseded_by.push(task.id.clone());
                transaction.execute(
                    "UPDATE records SET data=?1 WHERE kind='task' AND id=?2",
                    params![serde_json::to_string(&old)?, old_id],
                )?;
            }
        }
        for decision in &cycle.decision_memory {
            let id = decision["id"]
                .as_str()
                .ok_or_else(|| anyhow::anyhow!("Missing decision identity"))?;
            transaction.execute(
                "INSERT INTO records VALUES ('decision',?1,?2)
                 ON CONFLICT(kind,id) DO UPDATE SET data=excluded.data",
                params![id, serde_json::to_string(decision)?],
            )?;
        }
        if cycle.mode == crate::model::CycleMode::Execution {
            for proposal in &cycle.proposals {
                for id in &proposal.reconsiders {
                    let data: String = transaction
                        .query_row(
                            "SELECT data FROM records WHERE kind='task' AND id=?1",
                            [id],
                            |r| r.get(0),
                        )
                        .with_context(|| format!("Missing lineage task {id}"))?;
                    let mut old: crate::model::Task = serde_json::from_str(&data)?;
                    anyhow::ensure!(
                        old.rediscovery_requested
                            && old.status == crate::model::Status::Cancelled
                            && old
                                .config
                                .github_repo
                                .eq_ignore_ascii_case(&cycle.repository)
                            && old.proposal.target == proposal.target,
                        "Rediscovery decisions must reference an eligible request in this repository and target"
                    );
                    old.rediscovery_requested = false;
                    old.rediscovery_result =
                        Some(format!("{}: {}", proposal.decision, proposal.reason));
                    transaction.execute(
                        "UPDATE records SET data=?1 WHERE kind='task' AND id=?2",
                        params![serde_json::to_string(&old)?, id],
                    )?;
                }
            }
            let saved: Option<String> = transaction
                .query_row(
                    "SELECT data FROM records WHERE kind='settings' AND id='control'",
                    [],
                    |r| r.get(0),
                )
                .optional()?;
            if let Some(saved) = saved {
                let mut control: crate::model::Control = serde_json::from_str(&saved)?;
                control.idle_streak = if tasks.is_empty() {
                    control.idle_streak.saturating_add(1)
                } else {
                    0
                };
                transaction.execute(
                    "UPDATE records SET data=?1 WHERE kind='settings' AND id='control'",
                    [serde_json::to_string(&control)?],
                )?;
            }
        }
        transaction.commit()?;
        Ok(())
    }
    pub fn get<T: DeserializeOwned>(&self, kind: &str, id: &str) -> Result<Option<T>> {
        let c = self.conn();
        let mut s = c.prepare("SELECT data FROM records WHERE kind=?1 AND id=?2")?;
        let mut rows = s.query(params![kind, id])?;
        Ok(match rows.next()? {
            Some(r) => Some(serde_json::from_str(&r.get::<_, String>(0)?)?),
            None => None,
        })
    }
    pub fn list<T: DeserializeOwned>(&self, kind: &str) -> Result<Vec<T>> {
        let c = self.conn();
        let mut s = c.prepare("SELECT data FROM records WHERE kind=?1 ORDER BY rowid DESC")?;
        let rows = s.query_map([kind], |r| r.get::<_, String>(0))?;
        rows.map(|r| Ok(serde_json::from_str(&r?)?)).collect()
    }
    pub fn event(&self, entity: &str, kind: &str, message: &str) -> Result<()> {
        self.conn().execute(
            "INSERT INTO events(at,entity_id,kind,message) VALUES (?1,?2,?3,?4)",
            params![now(), entity, kind, redact(message)],
        )?;
        Ok(())
    }
    pub fn events(&self, entity: Option<&str>) -> Result<Vec<Event>> {
        let c = self.conn();
        let mut s = c.prepare(
            "SELECT id,at,entity_id,kind,message FROM events WHERE (?1 IS NULL OR entity_id=?1) ORDER BY id DESC LIMIT 200",
        )?;
        Ok(s.query_map([entity], |r| {
            Ok(Event {
                id: r.get(0)?,
                at: r.get(1)?,
                entity_id: r.get(2)?,
                kind: r.get(3)?,
                message: r.get(4)?,
            })
        })?
        .collect::<rusqlite::Result<Vec<_>>>()?)
    }
    pub fn prune_events(&self, retain: usize) -> Result<()> {
        self.conn().execute(
            "DELETE FROM events WHERE id NOT IN (SELECT id FROM events ORDER BY id DESC LIMIT ?1)",
            [retain as i64],
        )?;
        Ok(())
    }
    pub fn reserve_session(&self, measured_bytes: u64, admission: &Admission) -> Result<()> {
        // Derive both timestamps from the same instant, including across UTC midnight.
        let day = chrono::DateTime::parse_from_rfc3339(&admission.at)?
            .with_timezone(&chrono::Utc)
            .format("%F")
            .to_string();
        let mut c = self.conn();
        let tx = c.transaction_with_behavior(rusqlite::TransactionBehavior::Immediate)?;
        // Policy is read under the same write transaction as the reservation. Callers
        // cannot accidentally supply a queued task's historical admission limits.
        let config: crate::config::Config = tx
            .query_row(
                "SELECT data FROM records WHERE kind='settings' AND id='config'",
                [],
                |r| r.get::<_, String>(0),
            )
            .optional()?
            .map(|s| serde_json::from_str(&s))
            .transpose()?
            .unwrap_or_default();
        let limit = config.max_sessions_per_day;
        anyhow::ensure!(limit > 0, "Daily session budget must be positive");
        if measured_bytes >= config.max_workspace_bytes {
            return Err(anyhow::Error::new(crate::model::BlockedReason::StorageLimit)
                .context(format!("Workspace storage limit reached ({measured_bytes} bytes). Resolve retained tasks or increase the limit")));
        }
        let changed = tx.execute(
            "INSERT INTO usage(day,sessions) VALUES (?1,1)
             ON CONFLICT(day) DO UPDATE SET sessions=sessions+1 WHERE sessions < ?2",
            params![day, limit.min(i64::MAX as u64) as i64],
        )?;
        if changed != 1 {
            return Err(anyhow::Error::new(crate::model::BlockedReason::BudgetExhausted)
                .context("Daily session budget exhausted; increase the configured limit or wait until UTC midnight"));
        }
        tx.execute(
            "INSERT INTO admissions(id,at,day,data) VALUES (?1,?2,?3,?4)",
            params![
                admission.id,
                admission.at,
                day,
                serde_json::to_string(admission)?
            ],
        )?;
        tx.commit()?;
        Ok(())
    }
    pub fn sessions_today(&self) -> Result<u64> {
        let c = self.conn();
        Ok(c.query_row(
            "SELECT sessions FROM usage WHERE day=?1",
            [chrono::Utc::now().format("%F").to_string()],
            |r| r.get::<_, i64>(0),
        )
        .optional()?
        .unwrap_or(0) as u64)
    }
    pub fn planning_capacity(&self) -> Result<crate::model::PlanningCapacity> {
        self.planning_capacity_at(chrono::Utc::now())
    }
    fn planning_capacity_at(
        &self,
        at: chrono::DateTime<chrono::Utc>,
    ) -> Result<crate::model::PlanningCapacity> {
        use crate::model::{PlanningCapacity, PlanningCapacityStatus};
        let day = at.format("%F").to_string();
        let mut c = self.conn();
        let tx = c.transaction()?;
        let config: crate::config::Config = tx
            .query_row(
                "SELECT data FROM records WHERE kind='settings' AND id='config'",
                [],
                |r| r.get::<_, String>(0),
            )
            .optional()?
            .map(|s| serde_json::from_str(&s))
            .transpose()?
            .unwrap_or_default();
        let used = tx
            .query_row("SELECT sessions FROM usage WHERE day=?1", [&day], |r| {
                r.get::<_, i64>(0)
            })
            .optional()?
            .unwrap_or(0)
            .max(0) as u64;
        tx.commit()?;
        drop(c);
        let limit = config.max_sessions_per_day;
        let required = config.planning_admissions_required();
        let remaining = limit.saturating_sub(used);
        let next_reset_at = at
            .date_naive()
            .checked_add_days(chrono::Days::new(1))
            .and_then(|d| d.and_hms_opt(0, 0, 0))
            .map(|d| d.and_utc().timestamp())
            .unwrap_or(i64::MAX);
        let status = if limit < required {
            PlanningCapacityStatus::LimitTooLow
        } else if remaining < required {
            PlanningCapacityStatus::DailyExhausted
        } else {
            PlanningCapacityStatus::Ready
        };
        Ok(PlanningCapacity {
            day,
            limit,
            used,
            remaining,
            required,
            next_reset_at,
            status,
        })
    }
    /// Targeted status write for the operator-cancel path: a full-record save from a
    /// stale task copy could resurrect fields the running worker already updated.
    /// Publication checkpoints must remain recoverable even if the worker has
    /// already saved a final blocked status by the time cancellation reaches us.
    pub fn cancel_task(&self, id: &str) -> Result<bool> {
        let changed = self.conn().execute(
            "UPDATE records SET data=json_set(data,'$.status','cancelled','$.updated_at',?2)
             WHERE kind='task' AND id=?1
               AND json_extract(data,'$.status') NOT IN ('publishing','published')
               AND json_extract(data,'$.output_commit') IS NULL",
            params![id, now()],
        )?;
        Ok(changed > 0)
    }
    /// Opens the state database read-only for reporting paths that must not migrate,
    /// create directories or take the service lock.
    pub fn open_readonly(path: &Path, what: &str) -> Result<Connection> {
        let c = Connection::open_with_flags(path, OpenFlags::SQLITE_OPEN_READ_ONLY)
            .with_context(|| format!("Cannot open existing state database for read-only {what}"))?;
        c.busy_timeout(Duration::from_secs(5))?;
        Ok(c)
    }
}
/// An error rendered for an operator, with secrets scrubbed. Errors reach
/// operators through saved records and API responses, so every stored error
/// message is built here rather than formatted at each site.
pub fn error_message(error: &anyhow::Error) -> String {
    redact(&format!("{error:#}"))
}
pub fn redact(input: &str) -> String {
    use std::sync::LazyLock;
    static TOKEN: LazyLock<regex::Regex> = LazyLock::new(|| {
        regex::Regex::new(r"(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+|(?:gh[pousr]_|github_pat_|sk-)[A-Za-z0-9_-]{10,}|[a-z]+://[^\s/@]+:[^\s/@]+@").unwrap()
    });
    let mut s = TOKEN.replace_all(input, "[redacted]").into_owned();
    static SECRETS: LazyLock<Vec<String>> = LazyLock::new(|| {
        std::env::vars()
            .filter(|(key, value)| {
                value.len() >= 8
                    && (key == crate::notifications::WEBHOOK_ENV
                        || ["TOKEN", "SECRET", "PASSWORD", "API_KEY"]
                            .iter()
                            .any(|p| key.contains(p)))
            })
            .map(|(_, value)| value)
            .collect()
    });
    for value in SECRETS.iter() {
        s = s.replace(value, "[redacted]");
    }
    s.chars().take(16384).collect()
}
pub fn redact_json(value: &mut serde_json::Value) {
    match value {
        serde_json::Value::String(text) => *text = redact(text),
        serde_json::Value::Array(values) => values.iter_mut().for_each(redact_json),
        serde_json::Value::Object(values) => values.values_mut().for_each(redact_json),
        _ => {}
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn durable_and_budget_atomic() {
        let d = tempfile::tempdir().unwrap();
        let p = d.path().join("state.db");
        {
            let s = Store::open(&p).unwrap();
            s.put("x", "a", &vec![1, 2]).unwrap();
            s.put(
                "settings",
                "config",
                &crate::config::Config {
                    max_sessions_per_day: 1,
                    ..Default::default()
                },
            )
            .unwrap();
            s.reserve_session(
                1,
                &Admission::new(
                    "test-cycle",
                    None,
                    "grounding",
                    &crate::config::Route::new("test", "low"),
                ),
            )
            .unwrap();
            assert!(
                s.reserve_session(
                    1,
                    &Admission::new(
                        "test-cycle",
                        None,
                        "grounding",
                        &crate::config::Route::new("test", "low")
                    )
                )
                .is_err()
            );
        }
        assert_eq!(
            Store::open(&p).unwrap().get::<Vec<i32>>("x", "a").unwrap(),
            Some(vec![1, 2])
        );
    }
    #[test]
    fn task_guard_keeps_reservation_until_fallback_write_finishes() {
        use crate::{
            config::{Config, Route},
            engine::App,
            model::{BlockedReason, Status, Task},
        };
        use serde_json::json;

        struct PendingCleanup {
            app: App,
            writer: Connection,
            reserved: bool,
        }
        thread_local! {
            static CLEANUP: std::cell::RefCell<Option<PendingCleanup>> = const {
                std::cell::RefCell::new(None)
            };
        }
        for fail_write in [false, true] {
            let temp = tempfile::tempdir().unwrap();
            let path = temp.path().join("state.db");
            let store = Store::open(&path).unwrap();
            let app = App::new(store.clone(), temp.path().into());
            let task: Task = serde_json::from_value(json!({
                "id":"task", "cycle_id":"cycle",
                "proposal":{"id":"proposal","title":"Improve behavior","problem":"Missing behavior","benefit":"Useful behavior","scope":"one file","evidence":[],"category":"features","target":"main","tier":"M","dependencies":[],"prompt":"Implement behavior","decision":"accepted","reason":"Grounded"},
                "status":"publishing", "route":Route::new("fixture","low"), "config":Config::default(),
                "source_revision":"source", "comparison_base":"source", "default_revision":"source",
                "branch":"tyk/task", "workspace":"", "execution_session":null, "repair_session":null,
                "sessions":[], "reviews":[], "verification":[], "output_commit":"reviewed-output",
                "pr_number":null, "pr_url":null, "attempts":0, "error":null,
                "created_at":now(), "updated_at":now()
            }))
            .unwrap();
            store.put("task", &task.id, &task).unwrap();
            app.runtime()
                .tasks
                .insert(task.id.clone(), app.shutdown.child_token());
            let writer = Connection::open(&path).unwrap();
            if fail_write {
                writer
                    .execute_batch(
                        "CREATE TRIGGER fail_cleanup BEFORE UPDATE ON records
                         WHEN OLD.kind='task' BEGIN SELECT RAISE(ABORT,'fixture failure'); END;",
                    )
                    .unwrap();
            }
            // WAL readers still work, but the guard's fallback save must wait.
            writer.execute_batch("BEGIN IMMEDIATE").unwrap();
            CLEANUP.set(Some(PendingCleanup {
                app: app.clone(),
                writer,
                reserved: false,
            }));
            store
                .conn()
                .busy_handler(Some(|_| {
                    CLEANUP.with_borrow_mut(|pending| {
                        let pending = pending.as_mut().unwrap();
                        pending.reserved = pending.app.runtime().tasks.contains_key("task");
                        pending.writer.execute_batch("ROLLBACK").unwrap();
                    });
                    true
                }))
                .unwrap();
            drop(app.task_guard(&task.id));
            assert!(CLEANUP.take().unwrap().reserved);
            assert!(!app.runtime().tasks.contains_key(&task.id));
            let saved: Task = store.get("task", &task.id).unwrap().unwrap();
            if fail_write {
                assert_eq!(json!(saved), json!(task));
            } else {
                assert_eq!(saved.status, Status::Blocked);
                assert_eq!(saved.blocked_reason, Some(BlockedReason::Unknown));
                assert!(saved.error.unwrap().contains("exited unexpectedly"));
                assert_eq!(saved.output_commit, task.output_commit);
            }
        }
    }

    #[test]
    fn planning_capacity_reflects_policy_usage_and_utc_day() {
        use crate::model::{BlockedReason, PlanningCapacityStatus};
        let d = tempfile::tempdir().unwrap();
        let s = Store::open(&d.path().join("state.db")).unwrap();
        let route = crate::config::Route::new("fixture", "low");
        for (agents, required) in [(8, 12), (9, 13), (10, 14)] {
            s.put(
                "settings",
                "config",
                &crate::config::Config {
                    discovery_agents: agents,
                    ..Default::default()
                },
            )
            .unwrap();
            let capacity = s.planning_capacity().unwrap();
            assert_eq!(capacity.required, required);
            assert_eq!(capacity.status, PlanningCapacityStatus::Ready);
            capacity.ensure_available().unwrap();
        }
        s.put(
            "settings",
            "config",
            &crate::config::Config {
                max_sessions_per_day: 12,
                ..Default::default()
            },
        )
        .unwrap();
        let capacity = s.planning_capacity().unwrap();
        assert_eq!(capacity.status, PlanningCapacityStatus::LimitTooLow);
        assert!(capacity.message().contains("cannot fund"));
        let error = capacity.ensure_available().unwrap_err();
        assert_eq!(
            BlockedReason::from_error(&error),
            BlockedReason::BudgetExhausted
        );
        s.put(
            "settings",
            "config",
            &crate::config::Config {
                max_sessions_per_day: 14,
                ..Default::default()
            },
        )
        .unwrap();
        s.reserve_session(0, &Admission::new("cycle", None, "grounding", &route))
            .unwrap();
        let capacity = s.planning_capacity().unwrap();
        assert_eq!((capacity.used, capacity.remaining), (1, 13));
        capacity.ensure_available().unwrap();
        s.reserve_session(0, &Admission::new("cycle", None, "discovery-0", &route))
            .unwrap();
        let capacity = s.planning_capacity().unwrap();
        assert_eq!((capacity.used, capacity.remaining), (2, 12));
        assert_eq!(capacity.status, PlanningCapacityStatus::DailyExhausted);
        assert!(capacity.message().contains("Wait until UTC midnight"));
        assert!(capacity.ensure_available().is_err());
        let at = |s: &str| {
            chrono::DateTime::parse_from_rfc3339(s)
                .unwrap()
                .with_timezone(&chrono::Utc)
        };
        for role in ["grounding", "discovery-0"] {
            let mut admission = Admission::new("cycle", None, role, &route);
            admission.at = "2026-03-01T23:30:00Z".into();
            s.reserve_session(0, &admission).unwrap();
        }
        let capacity = s.planning_capacity_at(at("2026-03-01T23:59:00Z")).unwrap();
        assert_eq!(capacity.day, "2026-03-01");
        assert_eq!((capacity.used, capacity.remaining), (2, 12));
        assert_eq!(capacity.status, PlanningCapacityStatus::DailyExhausted);
        assert_eq!(
            capacity.next_reset_at,
            at("2026-03-02T00:00:00Z").timestamp()
        );
        let capacity = s.planning_capacity_at(at("2026-03-02T00:00:00Z")).unwrap();
        assert_eq!(capacity.day, "2026-03-02");
        assert_eq!((capacity.used, capacity.remaining), (0, 14));
        assert_eq!(capacity.status, PlanningCapacityStatus::Ready);
    }

    #[test]
    fn redacts_tokens() {
        assert!(!redact("Bearer secretkey123 ghp_abcdefghijklmnop").contains("secretkey"));
    }
}
