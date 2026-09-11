//! Explicit scale check: cargo test --test history_scale -- --ignored --nocapture
use octomus_agent::store::Store;
use std::{
    alloc::{GlobalAlloc, Layout, System},
    sync::atomic::{AtomicUsize, Ordering},
    time::Instant,
};
struct Meter;
static LIVE: AtomicUsize = AtomicUsize::new(0);
static PEAK: AtomicUsize = AtomicUsize::new(0);
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
