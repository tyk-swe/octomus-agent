//! Local, read-only reporting. Never open through Store::open (which migrates state).
use crate::{
    config::TIERS,
    model::{Cycle, Task, completed_sessions, decision, now},
    store::{Admission, Store, redact_json},
};
use anyhow::Result;
use rusqlite::Connection;
use serde::de::DeserializeOwned;
use serde_json::{Value, json};
use std::{collections::BTreeMap, path::Path};

fn records<T: DeserializeOwned>(c: &Connection, kind: &str) -> Result<Vec<T>> {
    let mut statement = c.prepare("SELECT data FROM records WHERE kind=?1 ORDER BY id")?;
    let rows = statement.query_map([kind], |r| r.get::<_, String>(0))?;
    rows.map(|r| Ok(serde_json::from_str(&r?)?)).collect()
}

pub fn usage_report(path: &Path) -> Result<Value> {
    let mut c = Store::open_readonly(path, "usage reporting")?;
    // One consistent snapshot even while the service is admitting work.
    let tx = c.transaction()?;
    let has_ledger: bool = tx.query_row(
        "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='admissions')",
        [],
        |r| r.get(0),
    )?;
    let admissions: Vec<Admission> = if has_ledger {
        let mut s = tx.prepare("SELECT data FROM admissions ORDER BY at,id")?;
        let rows = s.query_map([], |r| r.get::<_, String>(0))?;
        rows.map(|r| Ok(serde_json::from_str(&r?)?))
            .collect::<Result<_>>()?
    } else {
        vec![]
    };
    let cycles: Vec<Cycle> = records(&tx, "cycle")?;
    let tasks: Vec<Task> = records(&tx, "task")?;
    let mut daily_counts = BTreeMap::<String, u64>::new();
    let mut cycle_counts = BTreeMap::<String, (u64, u64)>::new();
    let mut task_counts = BTreeMap::<String, u64>::new();
    for admission in &admissions {
        let day = crate::model::utc_day(chrono::DateTime::parse_from_rfc3339(&admission.at)?);
        *daily_counts.entry(day).or_default() += 1;
        let counts = cycle_counts.entry(admission.cycle_id.clone()).or_default();
        if let Some(task) = &admission.task_id {
            counts.1 += 1;
            *task_counts.entry(task.clone()).or_default() += 1;
        } else {
            counts.0 += 1;
        }
    }
    let mut s = tx.prepare("SELECT day,sessions FROM usage ORDER BY day")?;
    let daily = s
        .query_map([], |r| {
            Ok((r.get::<_, String>(0)?, r.get::<_, i64>(1)? as u64))
        })?
        .map(|r| {
            let (day, total) = r?;
            let recorded = daily_counts.get(&day).copied().unwrap_or(0);
            Ok(
                json!({"day":day,"admissions":total,"attributed_admissions":recorded,
                "unattributed_admissions":total.saturating_sub(recorded)}),
            )
        })
        .collect::<rusqlite::Result<Vec<_>>>()?;
    let cycle_rows = cycles.iter().map(|cycle| {
        let (planning, task) = cycle_counts.get(&cycle.id).copied().unwrap_or_default();
        let wall_seconds = cycle.completed_at.as_ref().and_then(|end| {
            let start = chrono::DateTime::parse_from_rfc3339(&cycle.started_at).ok()?;
            let end = chrono::DateTime::parse_from_rfc3339(end).ok()?;
            Some((end - start).num_milliseconds() as f64 / 1000.0)
        });
        let decisions = decision::ALL.map(|decision| {
            (decision, cycle.proposals.iter().filter(|p| p.decision == decision).count())
        }).into_iter().collect::<BTreeMap<_, _>>();
        json!({"id":cycle.id,"mode":cycle.mode,"number":cycle.number,"status":cycle.status,
            "started_at":cycle.started_at,"completed_at":cycle.completed_at,"wall_seconds":wall_seconds,
            "planning_admissions":planning,"task_admissions":task,
            "recorded_completed_sessions":completed_sessions(&cycle.sessions),
            "decisions":decisions,"error":cycle.error})
    }).collect::<Vec<_>>();
    let task_rows = tasks
        .iter()
        .map(|task| {
            json!({
                "id":task.id,"cycle_id":task.cycle_id,"tier":task.proposal.tier,
                "status":task.status,"route":task.route,"repair_route":task.config.repair_route,
                "admissions":task_counts.get(&task.id).copied().unwrap_or(0),
                "recorded_completed_sessions":completed_sessions(&task.sessions),
                "created_at":task.created_at,"updated_at":task.updated_at,
                "pr_url":task.pr_url,"error":task.error
            })
        })
        .collect::<Vec<_>>();
    let tiers = TIERS.map(|tier| {
        let observed = tasks
            .iter()
            .filter(|t| t.proposal.tier == tier && task_counts.contains_key(&t.id))
            .collect::<Vec<_>>();
        json!({"tier":tier,"observed_tasks":observed.len(),
            "admissions":observed.iter().map(|t| task_counts[&t.id]).sum::<u64>()})
    });
    let mut report = json!({"schema_version":1,"generated_at":now(),"has_admission_ledger":has_ledger,
        "measurement":"Admissions reserve budget before work starts. They include failed starts and retries; they are not completed turns or billed usage. Completed session counts describe persisted thread records; a repair thread can contain multiple turns. Historical admissions without a ledger remain unattributed. Cycle wall time excludes subsequent task execution. No provider charges or merge status are inferred.",
        "daily":daily,"cycles":cycle_rows,"tasks":task_rows,"tiers":tiers,"admissions":admissions});
    redact_json(&mut report);
    Ok(report)
}
