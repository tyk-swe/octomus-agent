use crate::model::{Event, now};
use anyhow::Result;
use rusqlite::{Connection, OptionalExtension, params};

mod queries;
pub use queries::{HistoryQuery, Page};
use serde::{Deserialize, Serialize, de::DeserializeOwned};
use std::{
    path::Path,
    sync::{Arc, Mutex},
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
    pub fn open(path: &Path) -> Result<Self> {
        let c = Connection::open(path)?;
        c.busy_timeout(std::time::Duration::from_secs(5))?;
        c.execute_batch("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; CREATE TABLE IF NOT EXISTS records (kind TEXT NOT NULL, id TEXT NOT NULL, data TEXT NOT NULL, PRIMARY KEY(kind,id)); CREATE TABLE IF NOT EXISTS events (id INTEGER PRIMARY KEY AUTOINCREMENT, at TEXT NOT NULL, entity_id TEXT NOT NULL, kind TEXT NOT NULL, message TEXT NOT NULL); CREATE TABLE IF NOT EXISTS usage (day TEXT PRIMARY KEY, sessions INTEGER NOT NULL); CREATE TABLE IF NOT EXISTS admissions (id TEXT PRIMARY KEY, at TEXT NOT NULL, day TEXT NOT NULL, data TEXT NOT NULL); CREATE INDEX IF NOT EXISTS admissions_day ON admissions(day); CREATE TRIGGER IF NOT EXISTS cap_activity AFTER INSERT ON events BEGIN DELETE FROM events WHERE id <= NEW.id - COALESCE(json_extract((SELECT data FROM records WHERE kind='settings' AND id='config'), '$.retain_events'),10000); END;")?;
        queries::migrate(&c)?;
        Ok(Self(Arc::new(Mutex::new(c))))
    }
    pub fn put<T: Serialize>(&self, kind: &str, id: &str, value: &T) -> Result<()> {
        self.0.lock().unwrap().execute("INSERT INTO records VALUES (?1,?2,?3) ON CONFLICT(kind,id) DO UPDATE SET data=excluded.data",params![kind,id,serde_json::to_string(value)?])?;
        Ok(())
    }
    pub fn commit_plan(
        &self,
        cycle: &crate::model::Cycle,
        tasks: &[crate::model::Task],
    ) -> Result<()> {
        let mut connection = self.0.lock().unwrap();
        let transaction = connection.transaction()?;
        transaction.execute("INSERT INTO records VALUES ('cycle',?1,?2) ON CONFLICT(kind,id) DO UPDATE SET data=excluded.data", params![cycle.id, serde_json::to_string(cycle)?])?;
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
                let data: String = transaction.query_row(
                    "SELECT data FROM records WHERE kind='task' AND id=?1",
                    [old_id],
                    |r| r.get(0),
                )?;
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
            transaction.execute("INSERT INTO records VALUES ('decision',?1,?2) ON CONFLICT(kind,id) DO UPDATE SET data=excluded.data",params![id,serde_json::to_string(decision)?])?;
        }
        if cycle.mode == crate::model::CycleMode::Execution {
            for proposal in &cycle.proposals {
                for id in &proposal.reconsiders {
                    let data: String = transaction.query_row(
                        "SELECT data FROM records WHERE kind='task' AND id=?1",
                        [id],
                        |r| r.get(0),
                    )?;
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
        let c = self.0.lock().unwrap();
        let mut s = c.prepare("SELECT data FROM records WHERE kind=?1 AND id=?2")?;
        let mut rows = s.query(params![kind, id])?;
        Ok(match rows.next()? {
            Some(r) => Some(serde_json::from_str(&r.get::<_, String>(0)?)?),
            None => None,
        })
    }
    pub fn list<T: DeserializeOwned>(&self, kind: &str) -> Result<Vec<T>> {
        let c = self.0.lock().unwrap();
        let mut s = c.prepare("SELECT data FROM records WHERE kind=?1 ORDER BY rowid DESC")?;
        let rows = s.query_map([kind], |r| r.get::<_, String>(0))?;
        rows.map(|r| Ok(serde_json::from_str(&r?)?)).collect()
    }
    pub fn remove(&self, kind: &str, id: &str) -> Result<()> {
        self.0.lock().unwrap().execute(
            "DELETE FROM records WHERE kind=?1 AND id=?2",
            params![kind, id],
        )?;
        Ok(())
    }
    pub fn event(&self, entity: &str, kind: &str, message: &str) -> Result<()> {
        self.0.lock().unwrap().execute(
            "INSERT INTO events(at,entity_id,kind,message) VALUES (?1,?2,?3,?4)",
            params![now(), entity, kind, redact(message)],
        )?;
        Ok(())
    }
    pub fn events(&self, entity: Option<&str>) -> Result<Vec<Event>> {
        let c = self.0.lock().unwrap();
        let mut s=c.prepare("SELECT id,at,entity_id,kind,message FROM events WHERE (?1 IS NULL OR entity_id=?1) ORDER BY id DESC LIMIT 200")?;
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
        self.0.lock().unwrap().execute(
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
        let mut c = self.0.lock().unwrap();
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
        let changed=tx.execute("INSERT INTO usage(day,sessions) VALUES (?1,1) ON CONFLICT(day) DO UPDATE SET sessions=sessions+1 WHERE sessions < ?2",params![day,limit.min(i64::MAX as u64) as i64])?;
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
        let c = self.0.lock().unwrap();
        Ok(c.query_row(
            "SELECT sessions FROM usage WHERE day=?1",
            [chrono::Utc::now().format("%F").to_string()],
            |r| r.get::<_, i64>(0),
        )
        .unwrap_or(0) as u64)
    }
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
                    && ["TOKEN", "SECRET", "PASSWORD", "API_KEY"]
                        .iter()
                        .any(|p| key.contains(p))
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
    fn redacts_tokens() {
        assert!(!redact("Bearer secretkey123 ghp_abcdefghijklmnop").contains("secretkey"));
    }
}
