use octomus_agent::{
    config::{Config, Route},
    engine::{App, PrIdentity, PrRefresh},
    model::{OpenPrInventory, PullRequest, Status, Task, now},
    store::{Store, pr_union},
};
use serde_json::json;
use std::path::Path;
use tokio_util::sync::CancellationToken;

mod common;
use common::*;

fn owned_pr(number: u64, branch: &str) -> PullRequest {
    serde_json::from_value(json!({
        "number": number, "title": "Owned", "branch": branch, "head": "b".repeat(40),
        "base": "main", "url": format!("https://example.invalid/pull/{number}"),
        "body": format!("<!-- octomus:task:t{number} -->"), "state": "open",
        "changed_lines": 1, "created_at": now(), "owned": true,
        "head_repository": "fixture/project", "base_repository": "fixture/project"
    }))
    .unwrap()
}

fn inventory(prs: Vec<PullRequest>) -> OpenPrInventory {
    OpenPrInventory {
        repository: "fixture/project".into(),
        observed_at: now(),
        prs,
    }
}

fn store_with_limit(limit: usize) -> (tempfile::TempDir, Store) {
    let tmp = tempfile::tempdir().unwrap();
    let store = Store::open(&tmp.path().join("state.db")).unwrap();
    store
        .put(
            "settings",
            "config",
            &Config {
                github_repo: "fixture/project".into(),
                max_open_prs: limit,
                ..Default::default()
            },
        )
        .unwrap();
    (tmp, store)
}

fn refresh_job(id: &str, c: &Config, cancel: &CancellationToken) -> PrRefresh {
    PrRefresh {
        id: id.into(),
        identity: PrIdentity::of(c),
        handle: tokio::spawn(async {}),
        cancel: cancel.clone(),
        result: None,
        error: None,
        last_attempt: chrono::Utc::now().timestamp(),
    }
}

#[test]
fn new_pr_admission_is_atomic_and_bounded_by_the_live_limit() {
    let (_tmp, store) = store_with_limit(5);
    let inv = inventory(
        (1..=4)
            .map(|n| owned_pr(n, &format!("octomus/open-{n}")))
            .collect(),
    );
    assert!(store.persist_pr_inventory(&inv, &[]).unwrap());
    let mut first = task();
    first.branch = "octomus/one".into();
    store.put("task", &first.id, &first).unwrap();
    let mut second = task();
    second.branch = "octomus/two".into();
    store.put("task", &second.id, &second).unwrap();
    assert!(store.admit_new_pr_task(&mut first, &inv).unwrap());
    assert_eq!(first.status, Status::Executing);
    assert!(!store.admit_new_pr_task(&mut second, &inv).unwrap());
    assert_eq!(second.status, Status::Queued);
    assert_eq!(
        store
            .get::<Task>("task", &second.id)
            .unwrap()
            .unwrap()
            .status,
        Status::Queued
    );
    let reservations = store.pr_reservations("fixture/project").unwrap();
    assert_eq!(reservations.len(), 1);
    assert_eq!(reservations[0].branch, "octomus/one");
    assert!(store.has_pr_reservation(&first.id).unwrap());
    assert!(!store.has_pr_reservation(&second.id).unwrap());
}

#[test]
fn shared_branch_pull_requests_each_consume_capacity() {
    let (_tmp, store) = store_with_limit(2);
    let mut second_base = owned_pr(8, "octomus/shared");
    second_base.base = "release".into();
    let inv = inventory(vec![owned_pr(7, "octomus/shared"), second_base]);
    assert!(store.persist_pr_inventory(&inv, &[]).unwrap());
    let (observed, _, remaining) =
        pr_union(&inv, &store.pr_reservations("fixture/project").unwrap(), 2);
    assert_eq!((observed, remaining), (2, 0));
    let mut queued = task();
    queued.branch = "octomus/next".into();
    store.put("task", &queued.id, &queued).unwrap();
    assert!(!store.admit_new_pr_task(&mut queued, &inv).unwrap());
    assert_eq!(queued.status, Status::Queued);
}

#[test]
fn stale_inventory_snapshots_cannot_admit_or_overwrite() {
    let (_tmp, store) = store_with_limit(2);
    let mut older = inventory(vec![]);
    older.observed_at = "2026-01-01T00:00:00Z".into();
    assert!(store.persist_pr_inventory(&older, &[]).unwrap());
    let mut admitted = task();
    admitted.branch = "octomus/one".into();
    store.put("task", &admitted.id, &admitted).unwrap();
    assert!(store.admit_new_pr_task(&mut admitted, &older).unwrap());
    assert!(store.has_pr_reservation(&admitted.id).unwrap());
    let mut delivered = store.get::<Task>("task", &admitted.id).unwrap().unwrap();
    delivered.status = Status::Published;
    delivered.output_commit = Some("e".repeat(40));
    store.put("task", &delivered.id, &delivered).unwrap();
    let mut newer = inventory(vec![owned_pr(10, "octomus/one")]);
    newer.observed_at = "2026-01-02T00:00:00Z".into();
    assert!(store.persist_pr_inventory(&newer, &[]).unwrap());
    assert!(!store.has_pr_reservation(&admitted.id).unwrap());
    let mut next = task();
    next.branch = "octomus/two".into();
    store.put("task", &next.id, &next).unwrap();
    assert!(!store.admit_new_pr_task(&mut next, &older).unwrap());
    assert_eq!(next.status, Status::Queued);
    assert!(store.admit_new_pr_task(&mut next, &newer).unwrap());
    let mut last = task();
    last.branch = "octomus/three".into();
    store.put("task", &last.id, &last).unwrap();
    assert!(!store.admit_new_pr_task(&mut last, &newer).unwrap());
    assert!(!store.persist_pr_inventory(&older, &[]).unwrap());
    assert!(store.has_pr_reservation(&next.id).unwrap());
    let saved: OpenPrInventory = store.get("settings", "pr_inventory").unwrap().unwrap();
    assert_eq!(saved.observed_at, "2026-01-02T00:00:00Z");
}

#[test]
fn admission_rechecks_the_canonical_queued_record() {
    let (_tmp, store) = store_with_limit(5);
    let inv = inventory(vec![]);
    assert!(store.persist_pr_inventory(&inv, &[]).unwrap());
    let mut queued = task();
    queued.branch = "octomus/one".into();
    store.put("task", &queued.id, &queued).unwrap();
    let mut stale = queued.clone();
    stale.error = Some("Stale copy".into());
    assert!(!store.admit_new_pr_task(&mut stale, &inv).unwrap());
    let mut canonical = store.get::<Task>("task", &queued.id).unwrap().unwrap();
    canonical.status = Status::Cancelled;
    store.put("task", &canonical.id, &canonical).unwrap();
    assert!(!store.admit_new_pr_task(&mut queued, &inv).unwrap());
    assert!(store.pr_reservations("fixture/project").unwrap().is_empty());
}

#[test]
fn admission_requires_task_identity_to_match_live_config() {
    let (_tmp, store) = store_with_limit(2);
    let inv = inventory(vec![]);
    assert!(store.persist_pr_inventory(&inv, &[]).unwrap());
    let mut queued = task();
    queued.branch = "octomus/one".into();
    store.put("task", &queued.id, &queued).unwrap();
    store
        .put(
            "settings",
            "config",
            &Config {
                github_repo: "fixture/project".into(),
                max_open_prs: 2,
                branch_prefix: "other/".into(),
                ..Default::default()
            },
        )
        .unwrap();
    assert!(!store.admit_new_pr_task(&mut queued.clone(), &inv).unwrap());
    store
        .put(
            "settings",
            "config",
            &Config {
                github_repo: "fixture/project".into(),
                max_open_prs: 2,
                default_branch: "trunk".into(),
                ..Default::default()
            },
        )
        .unwrap();
    assert!(!store.admit_new_pr_task(&mut queued.clone(), &inv).unwrap());
    store
        .put(
            "settings",
            "config",
            &Config {
                github_repo: "fixture/project".into(),
                max_open_prs: 2,
                ..Default::default()
            },
        )
        .unwrap();
    let mut off_target = task();
    off_target.branch = "octomus/two".into();
    off_target.proposal.target = "octomus/shared".into();
    store.put("task", &off_target.id, &off_target).unwrap();
    assert!(!store.admit_new_pr_task(&mut off_target, &inv).unwrap());
    assert!(store.admit_new_pr_task(&mut queued, &inv).unwrap());
}

#[test]
fn live_limit_changes_reclassify_capacity() {
    let (_tmp, store) = store_with_limit(5);
    let inv = inventory(
        (1..=5)
            .map(|n| owned_pr(n, &format!("octomus/open-{n}")))
            .collect(),
    );
    assert!(store.persist_pr_inventory(&inv, &[]).unwrap());
    let mut queued = task();
    queued.branch = "octomus/next".into();
    store.put("task", &queued.id, &queued).unwrap();
    assert!(!store.admit_new_pr_task(&mut queued.clone(), &inv).unwrap());
    store
        .put(
            "settings",
            "config",
            &Config {
                github_repo: "fixture/project".into(),
                max_open_prs: 6,
                ..Default::default()
            },
        )
        .unwrap();
    let mut queued = store.get::<Task>("task", &queued.id).unwrap().unwrap();
    assert!(store.admit_new_pr_task(&mut queued, &inv).unwrap());
}

#[test]
fn inventory_and_reservations_union_without_double_counting() {
    let (_tmp, store) = store_with_limit(5);
    let inv = inventory(vec![
        owned_pr(1, "octomus/observed"),
        owned_pr(2, "octomus/other"),
    ]);
    let mut represented = task();
    represented.branch = "octomus/observed".into();
    represented.status = Status::Published;
    represented.output_commit = Some("b".repeat(40));
    store.put("task", &represented.id, &represented).unwrap();
    store.seed_pr_reservation(&represented).unwrap();
    let mut unrepresented = task();
    unrepresented.branch = "octomus/pending".into();
    store.seed_pr_reservation(&unrepresented).unwrap();
    let (observed, unrepresented_count, remaining) =
        pr_union(&inv, &store.pr_reservations("fixture/project").unwrap(), 5);
    assert_eq!((observed, unrepresented_count, remaining), (2, 1, 2));
    store.persist_pr_inventory(&inv, &[]).unwrap();
    let reservations = store.pr_reservations("fixture/project").unwrap();
    assert_eq!(reservations.len(), 1);
    assert_eq!(reservations[0].task_id, unrepresented.id);
    let (observed, unrepresented_count, remaining) = pr_union(&inv, &reservations, 5);
    assert_eq!((observed, unrepresented_count, remaining), (2, 1, 2));
}

#[test]
fn terminal_states_release_reservations_but_checkpoints_and_delivery_do_not() {
    let (_tmp, store) = store_with_limit(5);
    let mut blocked = task();
    blocked.branch = "octomus/blocked".into();
    store.put("task", &blocked.id, &blocked).unwrap();
    store.seed_pr_reservation(&blocked).unwrap();
    blocked.status = Status::Blocked;
    store.put("task", &blocked.id, &blocked).unwrap();
    assert!(!store.has_pr_reservation(&blocked.id).unwrap());
    for (status, output) in [
        (Status::Failed, false),
        (Status::Cancelled, false),
        (Status::Blocked, true),
        (Status::Published, true),
    ] {
        let mut t = task();
        t.branch = format!("octomus/{status:?}");
        store.put("task", &t.id, &t).unwrap();
        store.seed_pr_reservation(&t).unwrap();
        t.status = status.clone();
        if output {
            t.output_commit = Some("c".repeat(40));
        }
        store.put("task", &t.id, &t).unwrap();
        assert_eq!(
            store.has_pr_reservation(&t.id).unwrap(),
            output,
            "{status:?}"
        );
    }
}

#[test]
fn persisted_inventory_releases_only_confirmed_reservations() {
    let (_tmp, store) = store_with_limit(5);
    let mut open = task();
    open.branch = "octomus/open".into();
    open.status = Status::Published;
    open.output_commit = Some("c".repeat(40));
    store.put("task", &open.id, &open).unwrap();
    store.seed_pr_reservation(&open).unwrap();
    let mut uncertain = task();
    uncertain.branch = "octomus/uncertain".into();
    store.seed_pr_reservation(&uncertain).unwrap();
    let mut confirmed = task();
    confirmed.branch = "octomus/merged".into();
    store.seed_pr_reservation(&confirmed).unwrap();
    let inv = inventory(vec![owned_pr(9, "octomus/open")]);
    store
        .persist_pr_inventory(&inv, &[confirmed.id.clone()])
        .unwrap();
    let reservations = store.pr_reservations("fixture/project").unwrap();
    assert_eq!(reservations.len(), 1);
    assert_eq!(reservations[0].task_id, uncertain.id);
}

#[test]
fn a_represented_reservation_survives_until_publication_without_double_counting() {
    let (_tmp, store) = store_with_limit(1);
    let mut active = task();
    active.branch = "octomus/live".into();
    active.status = Status::Executing;
    active.output_commit = Some("d".repeat(40));
    store.put("task", &active.id, &active).unwrap();
    store.seed_pr_reservation(&active).unwrap();
    let inv = inventory(vec![owned_pr(9, "octomus/live")]);
    assert!(store.persist_pr_inventory(&inv, &[]).unwrap());
    assert!(store.has_pr_reservation(&active.id).unwrap());
    let (observed, unrepresented, remaining) =
        pr_union(&inv, &store.pr_reservations("fixture/project").unwrap(), 1);
    assert_eq!((observed, unrepresented, remaining), (1, 0, 0));
    let mut queued = task();
    queued.branch = "octomus/next".into();
    store.put("task", &queued.id, &queued).unwrap();
    assert!(!store.admit_new_pr_task(&mut queued, &inv).unwrap());
    let mut published = store.get::<Task>("task", &active.id).unwrap().unwrap();
    published.status = Status::Published;
    store.put("task", &published.id, &published).unwrap();
    assert!(store.persist_pr_inventory(&inv, &[]).unwrap());
    assert!(!store.has_pr_reservation(&active.id).unwrap());
    let (observed, unrepresented, remaining) =
        pr_union(&inv, &store.pr_reservations("fixture/project").unwrap(), 1);
    assert_eq!((observed, unrepresented, remaining), (1, 0, 0));
}

#[test]
fn capacity_reports_only_fresh_current_process_observations() {
    let tmp = tempfile::tempdir().unwrap();
    let store = Store::open(&tmp.path().join("state.db")).unwrap();
    store.put("settings", "config", &config()).unwrap();
    let app = App::new(store.clone(), tmp.path().into());
    let inv = inventory(
        (1..=3)
            .map(|n| owned_pr(n, &format!("octomus/open-{n}")))
            .collect(),
    );
    let capacity = app.pr_capacity().unwrap();
    assert_eq!(capacity.status, "unavailable");
    assert_eq!(capacity.owned_open, None);
    assert_eq!(capacity.remaining, None);
    assert!(capacity.reason.is_some());
    store.persist_pr_inventory(&inv, &[]).unwrap();
    let capacity = app.pr_capacity().unwrap();
    assert_eq!(capacity.status, "unavailable");
    assert_eq!(capacity.owned_open, Some(3));
    assert_eq!(capacity.remaining, None);
    assert!(app.persist_pr_observation(&inv, &[]).unwrap());
    let capacity = app.pr_capacity().unwrap();
    assert_eq!(capacity.status, "ready");
    assert_eq!(
        (capacity.owned_open, capacity.remaining),
        (Some(3), Some(2))
    );
    assert_eq!(capacity.observed_at, Some(inv.observed_at.clone()));
    app.runtime().pr_refresh_error = Some("network refused".into());
    let capacity = app.pr_capacity().unwrap();
    assert_eq!(capacity.status, "unavailable");
    assert_eq!(capacity.remaining, None);
    assert!(capacity.reason.unwrap().contains("network refused"));
    app.runtime().pr_refresh_error = None;
    app.runtime().pr_observation = Some((
        PrIdentity::of(&config()),
        chrono::Utc::now().timestamp() - 301,
    ));
    let capacity = app.pr_capacity().unwrap();
    assert_eq!(capacity.status, "unavailable");
    assert_eq!(capacity.remaining, None);
    app.runtime().pr_observation = Some((
        PrIdentity {
            branch_prefix: "other/".into(),
            ..PrIdentity::of(&config())
        },
        chrono::Utc::now().timestamp(),
    ));
    let capacity = app.pr_capacity().unwrap();
    assert_eq!(capacity.status, "unavailable");
    assert!(capacity.reason.unwrap().contains("Configuration changed"));
    let wrong_repo = OpenPrInventory {
        repository: "other/project".into(),
        ..inv.clone()
    };
    assert!(!store.persist_pr_inventory(&wrong_repo, &[]).unwrap());
    let capacity = app.pr_capacity().unwrap();
    assert_eq!(capacity.owned_open, Some(3));
}

#[tokio::test]
async fn capacity_reports_refresh_state_and_clears_error_after_observation() {
    let tmp = tempfile::tempdir().unwrap();
    let store = Store::open(&tmp.path().join("state.db")).unwrap();
    store.put("settings", "config", &config()).unwrap();
    let app = App::new(store.clone(), tmp.path().into());
    let c = config();
    let running = CancellationToken::new();
    let mut job = refresh_job("running", &c, &running);
    job.handle = tokio::spawn(std::future::pending::<()>());
    app.runtime().pr_refresh = Some(job);
    assert_eq!(app.pr_capacity().unwrap().status, "refreshing");
    let failed = CancellationToken::new();
    let mut job = refresh_job("failed", &c, &failed);
    job.error = Some("fixture gh failure".into());
    app.runtime().pr_refresh = Some(job);
    tokio::task::yield_now().await;
    let capacity = app.pr_capacity().unwrap();
    assert_eq!(capacity.status, "unavailable");
    assert!(capacity.reason.unwrap().contains("fixture gh failure"));
    assert!(app.persist_pr_observation(&inventory(vec![]), &[]).unwrap());
    let capacity = app.pr_capacity().unwrap();
    assert_eq!(capacity.status, "ready");
    assert_eq!(capacity.remaining, Some(5));
    running.cancel();
}

#[tokio::test]
async fn cancelled_or_obsolete_refresh_results_never_authorize_dispatch() {
    let tmp = tempfile::tempdir().unwrap();
    let store = Store::open(&tmp.path().join("state.db")).unwrap();
    store.put("settings", "config", &config()).unwrap();
    let app = App::new(store.clone(), tmp.path().into());
    let c = config();
    let inv = inventory(vec![]);
    let token = CancellationToken::new();
    app.runtime().pr_refresh = Some(refresh_job("current", &c, &token));
    app.record_pr_refresh("current", &token, Ok(inv.clone()), vec![]);
    {
        let rt = app.runtime();
        let result = rt.pr_refresh.as_ref().unwrap().result.as_ref().unwrap();
        assert_eq!(result.observed_at, inv.observed_at);
    }
    tokio::task::yield_now().await;
    assert!(app.collect_pr_refresh().is_some());
    let superseding = CancellationToken::new();
    app.runtime().pr_refresh = Some(refresh_job("next", &c, &superseding));
    let stale_token = CancellationToken::new();
    app.record_pr_refresh(
        "current",
        &stale_token,
        Ok(inventory(vec![owned_pr(9, "octomus/x")])),
        vec![],
    );
    assert!(app.runtime().pr_refresh.as_ref().unwrap().result.is_none());
    superseding.cancel();
    app.record_pr_refresh("next", &superseding, Ok(inventory(vec![])), vec![]);
    assert!(app.runtime().pr_refresh.as_ref().unwrap().result.is_none());
    tokio::task::yield_now().await;
    assert!(app.collect_pr_refresh().is_none());
    assert!(app.runtime().pr_refresh.is_none());
    let expired_token = CancellationToken::new();
    let mut job = refresh_job("old", &c, &expired_token);
    let mut stale_result = inventory(vec![]);
    stale_result.observed_at = "2020-01-01T00:00:00Z".into();
    job.result = Some(stale_result);
    app.runtime().pr_refresh = Some(job);
    tokio::task::yield_now().await;
    assert!(app.collect_pr_refresh().is_none());
    assert!(app.runtime().pr_refresh.is_none());
    let wrong_repo = Config {
        github_repo: "other/project".into(),
        ..config()
    };
    let wrong_token = CancellationToken::new();
    let mut job = refresh_job("mismatch", &wrong_repo, &wrong_token);
    job.result = Some(inventory(vec![]));
    app.runtime().pr_refresh = Some(job);
    tokio::task::yield_now().await;
    assert!(app.collect_pr_refresh().is_none());
    assert!(app.runtime().pr_refresh.is_none());
}

#[tokio::test]
async fn successful_observation_with_capacity_does_not_delay_the_next_batch() {
    let (_tmp, store) = store_with_limit(5);
    let app = App::new(store, std::path::PathBuf::new());
    app.persist_pr_observation(&inventory(vec![]), &[]).unwrap();
    app.runtime().pr_refresh_last_attempt = chrono::Utc::now().timestamp();
    app.schedule_pr_refresh(&config()).unwrap();
    let job = app
        .runtime()
        .pr_refresh
        .take()
        .expect("Free capacity requires a fresh batch without failure backoff");
    app.shutdown.cancel();
    job.handle.await.unwrap();
}

#[tokio::test]
async fn pause_discards_a_held_result_and_resume_requires_a_fresh_fetch() {
    let tmp = tempfile::tempdir().unwrap();
    let store = Store::open(&tmp.path().join("state.db")).unwrap();
    store.put("settings", "config", &config()).unwrap();
    let app = App::new(store.clone(), tmp.path().into());
    let c = config();
    let inv = inventory(vec![]);
    let token = CancellationToken::new();
    app.runtime().pr_refresh = Some(refresh_job("held", &c, &token));
    app.record_pr_refresh("held", &token, Ok(inv), vec![]);
    assert!(app.runtime().pr_refresh.as_ref().unwrap().result.is_some());
    app.invalidate_pr_refresh();
    {
        let rt = app.runtime();
        let job = rt.pr_refresh.as_ref().unwrap();
        assert!(job.cancel.is_cancelled());
        assert!(job.result.is_none());
    }
    tokio::task::yield_now().await;
    assert!(app.collect_pr_refresh().is_none());
    assert!(app.runtime().pr_refresh.is_none());
    assert!(app.runtime().pr_refresh_error.is_none());
    app.schedule_pr_refresh(&c).unwrap();
    {
        let rt = app.runtime();
        let job = rt.pr_refresh.as_ref().unwrap();
        assert!(job.result.is_none());
        assert!(!job.cancel.is_cancelled());
        assert_ne!(job.id, "held");
    }
    app.shutdown.cancel();
    for _ in 0..50 {
        if app.collect_pr_refresh().is_some() || app.runtime().pr_refresh.is_none() {
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(50)).await;
    }
    assert!(app.runtime().pr_refresh.is_none());
}

#[test]
fn reserved_and_existing_pr_tasks_survive_a_full_new_queue() {
    let (_tmp, store) = store_with_limit(5);
    for _ in 0..501 {
        let queued = task();
        store.put("task", &queued.id, &queued).unwrap();
    }
    let mut reserved = task();
    reserved.run_id = Some("run".into());
    store.put("task", &reserved.id, &reserved).unwrap();
    store.seed_pr_reservation(&reserved).unwrap();
    let mut existing = task();
    existing.proposal.target = "octomus/existing".into();
    existing.run_id = Some("run".into());
    store.put("task", &existing.id, &existing).unwrap();
    let tasks = store.scheduling_tasks(None).unwrap();
    assert!(tasks.iter().any(|t| t.id == reserved.id));
    assert!(tasks.iter().any(|t| t.id == existing.id));
    let tasks = store.scheduling_tasks(Some("run")).unwrap();
    assert_eq!(tasks.len(), 2);
    assert!(
        tasks
            .iter()
            .all(|t| t.id == reserved.id || t.id == existing.id)
    );
}

#[test]
fn recover_seeds_reservations_for_active_recovery_and_checkpointed_work() {
    let tmp = tempfile::tempdir().unwrap();
    let path = tmp.path().join("state.db");
    let store = Store::open(&path).unwrap();
    let mut recovery = task();
    recovery.branch = "octomus/recovery".into();
    recovery.execution_session = Some("session".into());
    recovery.workspace = tmp
        .path()
        .join("recovery-ws")
        .to_string_lossy()
        .into_owned();
    std::fs::create_dir_all(Path::new(&recovery.workspace).join(".git")).unwrap();
    store.put("task", &recovery.id, &recovery).unwrap();
    let mut checkpoint = task();
    checkpoint.branch = "octomus/checkpoint".into();
    checkpoint.status = Status::Publishing;
    checkpoint.output_commit = Some("d".repeat(40));
    checkpoint.pr_number = Some(9);
    store.put("task", &checkpoint.id, &checkpoint).unwrap();
    let mut archived = task();
    archived.branch = "octomus/archived".into();
    archived.status = Status::Blocked;
    archived.output_commit = Some("e".repeat(40));
    archived.lifecycle.archived_at = Some(now());
    store.put("task", &archived.id, &archived).unwrap();
    let mut delivered = task();
    delivered.branch = "octomus/delivered".into();
    delivered.status = Status::Published;
    delivered.output_commit = Some("f".repeat(40));
    store.put("task", &delivered.id, &delivered).unwrap();
    let plain = task();
    store.put("task", &plain.id, &plain).unwrap();
    let mut doomed = task();
    doomed.branch = "octomus/doomed".into();
    doomed.status = Status::Executing;
    store.put("task", &doomed.id, &doomed).unwrap();
    let app = App::new(store.clone(), tmp.path().into());
    app.recover().unwrap();
    assert!(store.has_pr_reservation(&recovery.id).unwrap());
    assert!(store.has_pr_reservation(&checkpoint.id).unwrap());
    assert!(store.has_pr_reservation(&archived.id).unwrap());
    assert!(!store.has_pr_reservation(&delivered.id).unwrap());
    assert!(!store.has_pr_reservation(&plain.id).unwrap());
    assert!(!store.has_pr_reservation(&doomed.id).unwrap());
    assert_eq!(
        store
            .get::<Task>("task", &recovery.id)
            .unwrap()
            .unwrap()
            .status,
        Status::Queued
    );
}

#[test]
fn seed_candidates_cover_checkpoints_at_any_history_size_and_ignore_malformed_records() {
    let tmp = tempfile::tempdir().unwrap();
    let store = Store::open(&tmp.path().join("state.db")).unwrap();
    store.put("settings", "config", &config()).unwrap();
    store
        .put::<serde_json::Value>(
            "task",
            "malformed",
            &json!({"status":"published","unusable":true}),
        )
        .unwrap();
    for i in 0..505 {
        let mut checkpoint = task();
        checkpoint.branch = format!("octomus/done-{i}");
        checkpoint.status = Status::Blocked;
        checkpoint.output_commit = Some("a".repeat(40));
        store.put("task", &checkpoint.id, &checkpoint).unwrap();
    }
    let app = App::new(store.clone(), tmp.path().into());
    app.recover().unwrap();
    assert_eq!(store.pr_reservations("fixture/project").unwrap().len(), 505);
}

#[test]
fn a_stale_inventory_snapshot_still_counts_against_the_limit() {
    let (_tmp, store) = store_with_limit(1);
    let inv = inventory(vec![owned_pr(1, "octomus/open")]);
    store.persist_pr_inventory(&inv, &[]).unwrap();
    let mut queued = task();
    queued.branch = "octomus/next".into();
    store.put("task", &queued.id, &queued).unwrap();
    assert!(!store.admit_new_pr_task(&mut queued, &inv).unwrap());
    assert_eq!(
        store
            .get::<Task>("task", &queued.id)
            .unwrap()
            .unwrap()
            .status,
        Status::Queued
    );
}

#[tokio::test]
async fn admission_refresh_waits_for_an_execution_slot() {
    let tmp = tempfile::tempdir().unwrap();
    let store = Store::open(&tmp.path().join("state.db")).unwrap();
    let mut c = Config {
        repository: tmp.path().join("checkout"),
        github_repo: "fixture/project".into(),
        execution_concurrency: 1,
        verification_commands: vec!["true".into()],
        ..Default::default()
    };
    // Shipped defaults name no model; a ready fixture fills every route.
    for route in c
        .roles
        .values_mut()
        .chain(c.tiers.values_mut())
        .chain(std::iter::once(&mut c.repair_route))
    {
        *route = Route::new("fixture", "low");
    }
    std::fs::create_dir_all(c.repository.join(".git")).unwrap();
    c.validate(true).unwrap();
    store.put("settings", "config", &c).unwrap();
    let queued = task();
    store.put("task", &queued.id, &queued).unwrap();
    let app = App::new(store.clone(), tmp.path().into());
    let inv = inventory(vec![]);
    app.persist_pr_observation(&inv, &[]).unwrap();
    let mut completed = refresh_job("completed", &c, &CancellationToken::new());
    completed.result = Some(inv);
    completed.last_attempt = chrono::Utc::now().timestamp() - 10;
    let last_attempt = completed.last_attempt;
    tokio::task::yield_now().await;
    assert!(completed.handle.is_finished());
    {
        let mut rt = app.runtime();
        rt.last_retention_at = chrono::Utc::now().timestamp();
        rt.last_observation_at = rt.last_retention_at;
        rt.tasks.insert("busy".into(), CancellationToken::new());
        rt.pr_refresh = Some(completed);
    }
    let mut control = octomus_agent::model::Control::default();
    control.set_mode(octomus_agent::model::OperatingMode::Continuous);
    store.put("settings", "control", &control).unwrap();
    let service = tokio::spawn(app.clone().run());

    // Consume the completed inventory, then tick again while execution is full.
    tokio::time::sleep(std::time::Duration::from_millis(2200)).await;
    {
        let rt = app.runtime();
        assert!(rt.pr_refresh.is_none());
        assert_eq!(rt.pr_refresh_last_attempt, last_attempt);
    }
    assert_eq!(
        store
            .get::<Task>("task", &queued.id)
            .unwrap()
            .unwrap()
            .status,
        Status::Queued
    );
    assert!(!app.control().unwrap().paused);

    app.runtime().tasks.remove("busy");
    tokio::time::timeout(std::time::Duration::from_secs(5), async {
        while app.runtime().pr_refresh_last_attempt == last_attempt {
            tokio::time::sleep(std::time::Duration::from_millis(20)).await;
        }
    })
    .await
    .expect("Freeing an execution slot must resume admission refreshes");
    app.shutdown.cancel();
    service.await.unwrap();
}

#[tokio::test]
async fn refresh_failure_is_reported_without_failing_the_engine() {
    let tmp = tempfile::tempdir().unwrap();
    let store = Store::open(&tmp.path().join("state.db")).unwrap();
    let app = App::new(store.clone(), tmp.path().into());
    let mut config = Config {
        repository: tmp.path().join("checkout"),
        github_repo: "fixture/project".into(),
        verification_commands: vec!["true".into()],
        ..Default::default()
    };
    // Shipped defaults name no model; a ready fixture fills every route.
    for route in config
        .roles
        .values_mut()
        .chain(config.tiers.values_mut())
        .chain(std::iter::once(&mut config.repair_route))
    {
        *route = Route::new("fixture", "low");
    }
    std::fs::create_dir_all(config.repository.join(".git")).unwrap();
    config.validate(true).unwrap();
    store.put("settings", "config", &config).unwrap();
    let queued = task();
    store.put("task", &queued.id, &queued).unwrap();
    {
        let mut rt = app.runtime.lock().unwrap();
        rt.last_retention_at = chrono::Utc::now().timestamp();
        rt.last_observation_at = rt.last_retention_at;
    }
    let mut control = octomus_agent::model::Control::default();
    control.set_mode(octomus_agent::model::OperatingMode::Continuous);
    store.put("settings", "control", &control).unwrap();
    let service = tokio::spawn(app.clone().run());
    tokio::time::timeout(std::time::Duration::from_secs(15), async {
        loop {
            let capacity = app.pr_capacity().unwrap();
            if capacity.status == "unavailable"
                && capacity
                    .reason
                    .as_deref()
                    .is_some_and(|r| r != "No complete open-PR inventory has been observed")
            {
                break;
            }
            tokio::time::sleep(std::time::Duration::from_millis(100)).await;
        }
    })
    .await
    .expect("The refresh failure was never reported");
    let capacity = app.pr_capacity().unwrap();
    assert_eq!(capacity.status, "unavailable");
    assert_eq!(capacity.owned_open, None);
    assert_eq!(capacity.remaining, None);
    let control = app.control().unwrap();
    assert!(control.error.is_none());
    assert!(!control.paused);
    assert_eq!(
        store
            .get::<Task>("task", &queued.id)
            .unwrap()
            .unwrap()
            .status,
        Status::Queued
    );
    app.shutdown.cancel();
    service.await.unwrap();
}
