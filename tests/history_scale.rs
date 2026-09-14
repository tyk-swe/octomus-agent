//! Explicit scale check: cargo test --test history_scale -- --ignored --nocapture
use octomus_agent::{
    config::{Config, Route},
    model::{Proposal, Task},
    store::Store,
};
use std::{
    alloc::{GlobalAlloc, Layout, System},
    sync::atomic::{AtomicUsize, Ordering},
    time::Instant,
};
struct Meter;
static LIVE: AtomicUsize = AtomicUsize::new(0);
static PEAK: AtomicUsize = AtomicUsize::new(0);
static MEASUREMENT: std::sync::Mutex<()> = std::sync::Mutex::new(());
unsafe impl GlobalAlloc for Meter {
    unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
        let p = unsafe { System.alloc(layout) };
        if !p.is_null() {
            let live = LIVE.fetch_add(layout.size(), Ordering::Relaxed) + layout.size();
            PEAK.fetch_max(live, Ordering::Relaxed);
        }
        p
    }
    unsafe fn dealloc(&self, p: *mut u8, layout: Layout) {
        LIVE.fetch_sub(layout.size(), Ordering::Relaxed);
        unsafe { System.dealloc(p, layout) };
    }
}
#[global_allocator]
static ALLOCATOR: Meter = Meter;
#[test]
#[ignore = "explicit 100,000-record allocation and latency measurement"]
fn bounded_history_scale() {
    let _measurement = MEASUREMENT.lock().unwrap();
    let tmp = tempfile::tempdir().unwrap();
    let path = tmp.path().join("state.db");
    let store = Store::open(&path).unwrap();
    let mut writer = rusqlite::Connection::open(&path).unwrap();
    let data=serde_json::json!({"id":"fixture","status":"published","proposal":{"title":"Historical task","target":"main","prompt":"x".repeat(4096)},"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}).to_string();
    let mut inserted = 0;
    let mut baseline = 0;
    for count in [1000, 10000, 100000] {
        {
            let tx = writer.transaction().unwrap();
            {
                let mut stmt = tx
                    .prepare("INSERT INTO records VALUES ('task',?1,?2)")
                    .unwrap();
                for i in inserted..count {
                    stmt.execute(rusqlite::params![format!("historical-{i}"), data])
                        .unwrap();
                }
            }
            tx.commit().unwrap();
        }
        inserted = count;
        let mut elapsed = vec![];
        let mut peak = 0;
        let mut bytes = 0;
        for _ in 0..20 {
            let start_live = LIVE.load(Ordering::Relaxed);
            PEAK.store(start_live, Ordering::Relaxed);
            let start = Instant::now();
            let snapshot = store.dashboard().unwrap();
            elapsed.push(start.elapsed().as_micros());
            assert_eq!(snapshot["tasks"].as_array().unwrap().len(), 300);
            assert_eq!(snapshot["counts"]["published"], count);
            bytes = serde_json::to_vec(&snapshot).unwrap().len();
            peak = peak.max(PEAK.load(Ordering::Relaxed).saturating_sub(start_live));
        }
        elapsed.sort();
        if baseline == 0 {
            baseline = peak;
        }
        assert!(
            peak <= baseline * 2 + 65536,
            "Rust allocations grew with full history"
        );
        assert!(bytes < 1024 * 1024);
        println!(
            "history={count} state_bytes={bytes} p50_us={} p95_us={} peak_rust_bytes={peak}",
            elapsed[10], elapsed[19]
        );
    }
}

#[test]
#[ignore = "explicit 2,000-task duplicate lookup with 64 KiB evidence per task"]
fn duplicate_history_scale() {
    let _measurement = MEASUREMENT.lock().unwrap();
    let tmp = tempfile::tempdir().unwrap();
    let path = tmp.path().join("state.db");
    let store = Store::open(&path).unwrap();
    let mut data = serde_json::json!({
        "id":"historical", "cycle_id":"cycle", "status":"published",
        "proposal":{"id":"proposal", "title":"Historical task", "problem_key":"historical-key", "target":"main", "problem":"Missing behavior", "benefit":"Useful behavior", "scope":"one file", "evidence":["README.md"], "category":"features", "tier":"M", "dependencies":[], "prompt":"Implement behavior", "decision":"accepted", "reason":"Grounded"},
        "route":Route::new("fixture", "low"), "config":Config { github_repo:"fixture/project".into(), ..Config::default() },
        "source_revision":"source", "comparison_base":"source", "default_revision":"source", "branch":"tyk/history", "workspace":"", "execution_session":null, "repair_session":null, "sessions":[], "reviews":[],
        "verification":[{"command":"fixture", "success":true, "output":"x".repeat(64 * 1024), "revision":"source", "created_at":"2026-01-01T00:00:00Z"}],
        "output_commit":null, "pr_number":null, "pr_url":null, "attempts":0, "error":null, "created_at":"2026-01-01T00:00:00Z", "updated_at":"2026-01-01T00:00:00Z"
    });
    // Keep the probe valid for implementations that accidentally load all tasks.
    serde_json::from_value::<Task>(data.clone()).unwrap();
    let proposals: Vec<Proposal> = (0..20)
        .map(|i| {
            let mut proposal: Proposal = serde_json::from_value(data["proposal"].clone()).unwrap();
            proposal.title = format!("New task {i}");
            proposal.problem_key = format!("new-key-{i}");
            proposal
        })
        .collect();
    let mut writer = rusqlite::Connection::open(&path).unwrap();
    let tx = writer.transaction().unwrap();
    {
        let mut insert = tx
            .prepare("INSERT INTO records VALUES ('task',?1,?2)")
            .unwrap();
        for i in 0..2000 {
            data["id"] = serde_json::json!(format!("historical-{i}"));
            data["proposal"]["title"] = serde_json::json!(format!("Historical task {i}"));
            data["proposal"]["problem_key"] = serde_json::json!(format!("historical-key-{i}"));
            insert
                .execute(rusqlite::params![
                    data["id"].as_str().unwrap(),
                    data.to_string()
                ])
                .unwrap();
        }
    }
    tx.commit().unwrap();
    let start_live = LIVE.load(Ordering::Relaxed);
    PEAK.store(start_live, Ordering::Relaxed);
    let start = Instant::now();
    assert!(
        store
            .duplicate_tasks("FIXTURE/PROJECT", &proposals)
            .unwrap()
            .is_empty()
    );
    let elapsed = start.elapsed();
    let peak = PEAK.load(Ordering::Relaxed).saturating_sub(start_live);
    assert!(
        peak < 1024 * 1024,
        "Duplicate lookup allocated historical evidence: {peak} bytes"
    );
    println!(
        "history=2000 evidence_bytes=65536 proposals=20 elapsed_us={} peak_rust_bytes={peak}",
        elapsed.as_micros()
    );
}
