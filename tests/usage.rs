use octomus_agent::{
    config::Route,
    report::usage_report,
    store::{Admission, Store},
};
use rusqlite::Connection;

fn admission(at: &str) -> Admission {
    let mut a = Admission::new(
        "cycle",
        Some("task"),
        "repair",
        &Route::new("fixture", "medium"),
    );
    a.at = at.into();
    a
}

#[test]
fn admission_and_counter_commit_together_across_days_and_restarts() {
    let temp = tempfile::tempdir().unwrap();
    let path = temp.path().join("state.db");
    let store = Store::open(&path).unwrap();
    let first = admission("2026-09-09T23:59:59Z");
    store.reserve_session(2, &first).unwrap();
    // A failed ledger insert must roll back the counter increment too.
    assert!(store.reserve_session(2, &first).is_err());
    store
        .reserve_session(2, &admission("2026-09-09T23:59:59.500Z"))
        .unwrap();
    assert!(
        store
            .reserve_session(2, &admission("2026-09-09T23:59:59.900Z"))
            .is_err()
    );
    store
        .reserve_session(2, &admission("2026-09-10T00:00:00Z"))
        .unwrap();
    drop(store);
    let reopened = Store::open(&path).unwrap();
    // Offset timestamps still belong to the correct UTC day.
    reopened
        .reserve_session(2, &admission("2026-09-10T09:00:01+09:00"))
        .unwrap();
    let report = usage_report(&path).unwrap();
    assert_eq!(report["admissions"].as_array().unwrap().len(), 4);
    let daily = report["daily"].as_array().unwrap();
    assert_eq!(daily.len(), 2);
    assert_eq!(daily[0]["day"], "2026-09-09");
    assert_eq!(daily[1]["day"], "2026-09-10");
    for day in daily {
        assert_eq!(day["admissions"], 2);
        assert_eq!(day["attributed_admissions"], 2);
        assert_eq!(day["unattributed_admissions"], 0);
    }
}

#[test]
fn legacy_reporting_is_read_only_and_upgrade_preserves_unattributed_usage() {
    let temp = tempfile::tempdir().unwrap();
    let path = temp.path().join("state.db");
    let legacy = Connection::open(&path).unwrap();
    legacy.execute_batch("CREATE TABLE records(kind TEXT,id TEXT,data TEXT,PRIMARY KEY(kind,id)); CREATE TABLE usage(day TEXT PRIMARY KEY,sessions INTEGER); INSERT INTO usage VALUES ('2026-09-09',7);").unwrap();
    drop(legacy);
    let before = std::fs::read(&path).unwrap();
    let report = usage_report(&path).unwrap();
    assert_eq!(report["has_admission_ledger"], false);
    assert_eq!(report["daily"][0]["unattributed_admissions"], 7);
    assert_eq!(std::fs::read(&path).unwrap(), before);
    let store = Store::open(&path).unwrap();
    store
        .reserve_session(10, &admission("2026-09-09T12:00:00Z"))
        .unwrap();
    let report = usage_report(&path).unwrap();
    assert_eq!(report["daily"][0]["admissions"], 8);
    assert_eq!(report["daily"][0]["attributed_admissions"], 1);
    assert_eq!(report["daily"][0]["unattributed_admissions"], 7);
}

#[test]
fn report_never_creates_missing_state() {
    let temp = tempfile::tempdir().unwrap();
    let path = temp.path().join("missing/state.db");
    assert!(usage_report(&path).is_err());
    assert!(!path.parent().unwrap().exists());
}
