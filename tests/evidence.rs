//! Run-evidence read model. Every database here is an explicitly synthetic temporary
//! fixture; no live `.octomus/` state, credentials or runner accounts are touched.
use axum::{
    body::{Body, to_bytes},
    http::{Request, StatusCode},
};
use octomus_agent::{
    api,
    config::{Config, Route},
    engine::App,
    evidence,
    model::{Cycle, Task, id, now},
    store::Store,
};
use serde_json::{Value, json};
use tower::ServiceExt;

const TOKEN: &str = "run-evidence-fixture-token-at-least-32-characters";

fn cycle(id: &str, mode: &str, proposals: Value, assessments: Value, sessions: Value) -> Cycle {
    serde_json::from_value(json!({
        "id": id, "mode": mode, "number": 7, "status": "completed",
        "started_at": "2026-09-12T00:00:00Z", "completed_at": "2026-09-12T01:00:00Z",
        "grounding": {"revision": "base0000", "prs": [], "history": [], "maintenance_due": false, "maintenance_targets": []},
        "proposals": proposals, "assessments": assessments, "sessions": sessions,
        "error": null, "repository": "fixture/project"
    }))
    .unwrap()
}

fn proposal(id: &str, decision: &str) -> Value {
    json!({
        "id": id, "title": "Concrete improvement", "problem": "Missing behavior",
        "benefit": "Useful behavior", "scope": "one file", "evidence": ["README.md"],
        "category": "features", "target": "main", "tier": "M", "dependencies": [],
        "prompt": "SECRET-PROMPT-TEXT", "decision": decision, "reason": "Grounded reason"
    })
}

fn reviewer_session(role: &str, status: &str) -> Value {
    json!({
        "id": format!("{role}-session"), "role": role, "route": Route::new("fixture", "low"),
        "status": status, "started_at": "2026-09-12T00:10:00Z", "summary": "SECRET-TRANSCRIPT"
    })
}

fn batch(entries: Value) -> Value {
    json!({"assessments": entries})
}

/// A task carrying a private workspace path, prompt, transcript, command output and
/// diagnostic text, so omission of private fields is observable.
fn task(cycle_id: &str, proposal_id: &str) -> Task {
    serde_json::from_value(json!({
        "id": id(), "cycle_id": cycle_id, "proposal": proposal(proposal_id, "accepted"),
        "status": "queued", "route": Route::new("fixture", "low"),
        "config": Config {
            github_repo: "fixture/project".into(),
            codex_binary: "/private/bin/codex".into(),
            verification_commands: vec!["make check".into()],
            ..Config::default()
        },
        "source_revision": "source00", "comparison_base": "source00",
        "default_revision": "base0000", "branch": "octomus/work",
        "workspace": "/private/workspace/path",
        "execution_session": null, "repair_session": null,
        "sessions": [{"id": "exec-1", "role": "executor", "route": Route::new("fixture", "low"),
                      "status": "completed", "started_at": now(), "summary": "SECRET-TRANSCRIPT"}],
        "reviews": [], "verification": [],
        "output_commit": null, "pr_number": null, "pr_url": null,
        "attempts": 0, "error": "SECRET-ERROR-DETAIL",
        "created_at": now(), "updated_at": now()
    }))
    .unwrap()
}

fn review(revision: &str, completed: bool, summary: &str, findings: Value) -> Value {
    json!({
        "session_id": "review-1", "revision": revision, "comparison_base": "source00",
        "created_at": now(),
        "result": {"completed": completed, "summary": summary, "findings": findings}
    })
}

fn check(command: &str, success: bool, revision: &str) -> Value {
    json!({
        "command": command, "success": success, "output": "SECRET-COMMAND-OUTPUT",
        "revision": revision, "created_at": now()
    })
}

fn fixture(cycles: &[Cycle], tasks: &[Task]) -> (tempfile::TempDir, Store, std::path::PathBuf) {
    let temp = tempfile::tempdir().unwrap();
    let path = temp.path().join("state.db");
    let store = Store::open(&path).unwrap();
    for c in cycles {
        store.put("cycle", &c.id, c).unwrap();
    }
    for t in tasks {
        store.put("task", &t.id, t).unwrap();
    }
    (temp, store, path)
}

async fn api_evidence(store: &Store, dir: &std::path::Path, cycle: &str) -> (StatusCode, Value) {
    let app = App::new(store.clone(), dir.to_path_buf());
    let response = api::router(app, TOKEN, None)
        .oneshot(
            Request::builder()
                .uri(format!("/api/cycles/{cycle}/evidence"))
                .header("authorization", format!("Bearer {TOKEN}"))
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    let status = response.status();
    let bytes = to_bytes(response.into_body(), 4 * 1024 * 1024)
        .await
        .unwrap();
    (
        status,
        serde_json::from_slice(&bytes).unwrap_or(Value::Null),
    )
}

fn find_proposal<'a>(value: &'a Value, id: &str) -> &'a Value {
    value["proposals"]
        .as_array()
        .unwrap()
        .iter()
        .find(|p| p["id"] == id)
        .unwrap_or_else(|| panic!("proposal {id} missing from export"))
}

#[test]
fn complete_cycle_reports_reviewers_tasks_revisions_and_checks() {
    let mut delivered = task("cycle-a", "p1");
    delivered.status = octomus_agent::model::Status::Published;
    delivered.output_commit = Some("out00001".into());
    delivered.pr_number = Some(3);
    delivered.pr_url = Some("https://github.com/fixture/project/pull/3".into());
    delivered.reviews = serde_json::from_value(json!([review(
        "out00001",
        true,
        "Reviewed the complete change set",
        json!([])
    )]))
    .unwrap();
    delivered.verification =
        serde_json::from_value(json!([check("make check", true, "out00001")])).unwrap();
    let c = cycle(
        "cycle-a",
        "execution",
        json!([proposal("p1", "accepted"), proposal("p2", "deferred")]),
        json!([
            batch(json!([
                {"id": "p1", "decision": "accepted", "reason": "a accepts"},
                {"id": "p2", "decision": "deferred", "reason": "a defers"}
            ])),
            batch(json!([
                {"id": "p1", "decision": "accepted", "reason": "b accepts"},
                {"id": "p2", "decision": "rejected", "reason": "b rejects"}
            ]))
        ]),
        json!([
            reviewer_session("adversary-a", "completed"),
            reviewer_session("adversary-b", "completed")
        ]),
    );
    let (_temp, store, _path) = fixture(&[c], &[delivered.clone()]);
    let value = store.run_evidence("cycle-a").unwrap().unwrap();

    assert_eq!(value["schema_version"], evidence::SCHEMA_VERSION);
    assert_eq!(value["review_required_before_sharing"], true);
    assert_eq!(value["kind"], "recorded_review_check_evidence");
    assert!(value["generated_at"].as_str().unwrap().len() > 10);
    assert_eq!(value["cycle"]["planning"]["creates_execution_queue"], true);
    assert_eq!(value["cycle"]["planning"]["planning_finished"], true);
    assert_eq!(value["cycle"]["planning"]["decisions"]["accepted"], 1);
    assert_eq!(value["cycle"]["planning"]["decisions"]["deferred"], 1);
    assert_eq!(value["cycle"]["grounding_revision"], "base0000");

    let p1 = find_proposal(&value, "p1");
    let verdicts = p1["reviewer_verdicts"].as_array().unwrap();
    assert_eq!(verdicts[0]["reviewer"], "adversary-a");
    assert_eq!(verdicts[0]["state"], "recorded");
    assert_eq!(verdicts[0]["decision"], "accepted");
    assert_eq!(verdicts[1]["reviewer"], "adversary-b");
    assert_eq!(verdicts[1]["reason"], "b accepts");

    // Deferred stays deferred and is never folded into rejected.
    let p2 = find_proposal(&value, "p2");
    assert_eq!(p2["final_decision"], "deferred");
    assert_eq!(p2["reviewer_verdicts"][0]["decision"], "deferred");
    assert_eq!(p2["reviewer_verdicts"][1]["decision"], "rejected");
    assert!(p2["linked_tasks"].as_array().unwrap().is_empty());

    let t = &p1["linked_tasks"][0];
    assert_eq!(t["id"], delivered.id);
    assert_eq!(t["proposal_id"], "p1");
    assert_eq!(t["cycle_id"], "cycle-a");
    assert_eq!(t["status"], "published");
    assert_eq!(t["revisions"]["output"], "out00001");
    assert_eq!(t["revisions"]["comparison_base"], "source00");
    assert_eq!(t["latest_review"]["clean"], true);
    assert_eq!(t["latest_review"]["clean_at_output_revision"], true);
    assert_eq!(t["latest_review"]["latest"]["summary_present"], true);
    assert_eq!(t["required_commands"]["state"], "recorded");
    assert_eq!(t["required_commands"]["commands"][0]["state"], "passed");
    assert_eq!(
        t["required_commands"]["all_passed_at_output_revision"],
        true
    );
    assert_eq!(t["pull_request"]["number"], 3);
    assert_eq!(t["pull_request"]["source"], "recorded_task_reference");
    assert_eq!(t["sessions"][0]["role"], "executor");
    assert_eq!(t["sessions"][0]["requested_route"]["model"], "fixture");
    assert!(t["gaps"].as_array().unwrap().is_empty(), "{:?}", t["gaps"]);
    assert!(
        value["limitations"]
            .as_array()
            .unwrap()
            .iter()
            .any(|l| l.as_str().unwrap().contains("Published is not merged"))
    );
}

#[test]
fn private_fields_are_omitted_from_the_export() {
    let mut t = task("cycle-a", "p1");
    t.output_commit = Some("out00001".into());
    t.reviews = serde_json::from_value(json!([review(
        "out00001",
        true,
        "SECRET-REVIEW-SUMMARY",
        json!([{"title": "Finding title", "file": "src/x.rs",
                "detail": "Finding rationale", "priority": "high"}])
    )]))
    .unwrap();
    t.verification =
        serde_json::from_value(json!([check("make check", true, "out00001")])).unwrap();
    let c = cycle(
        "cycle-a",
        "execution",
        json!([proposal("p1", "accepted")]),
        json!([]),
        json!([]),
    );
    let (_temp, store, _path) = fixture(&[c], &[t]);
    let value = store.run_evidence("cycle-a").unwrap().unwrap();
    let text = serde_json::to_string(&value).unwrap();
    for private in [
        "/private/workspace/path",
        "/private/bin/codex",
        "SECRET-PROMPT-TEXT",
        "SECRET-TRANSCRIPT",
        "SECRET-COMMAND-OUTPUT",
        "SECRET-ERROR-DETAIL",
        "SECRET-REVIEW-SUMMARY",
    ] {
        assert!(!text.contains(private), "export leaked {private}");
    }
    // The facts about those records survive without the private text.
    let t = &find_proposal(&value, "p1")["linked_tasks"][0];
    assert_eq!(t["error_recorded"], true);
    assert_eq!(t["latest_review"]["latest"]["summary_present"], true);
    let finding = &t["latest_review"]["latest"]["findings"][0];
    assert_eq!(finding["title"], "Finding title");
    assert_eq!(finding["file"], "src/x.rs");
    assert_eq!(finding["priority"], "high");
    assert!(text.contains("requires manual review before sharing"));
    assert!(!text.contains("safe to publish"));
}

#[test]
fn partial_cycle_reports_gaps_without_inventing_outcomes() {
    let mut c = cycle(
        "cycle-a",
        "execution",
        json!([proposal("p1", "accepted")]),
        json!([]),
        json!([]),
    );
    c.status = "running".into();
    c.completed_at = None;
    c.grounding = None;
    let (_temp, store, _path) = fixture(&[c], &[]);
    let value = store.run_evidence("cycle-a").unwrap().unwrap();
    assert_eq!(value["cycle"]["planning"]["planning_finished"], false);
    assert_eq!(value["cycle"]["grounding_revision"], Value::Null);
    let gaps = value["gaps"].as_array().unwrap();
    let joined = gaps
        .iter()
        .map(|g| g.as_str().unwrap())
        .collect::<Vec<_>>()
        .join(" | ");
    assert!(joined.contains("still recorded as running"), "{joined}");
    assert!(joined.contains("no saved grounding revision"), "{joined}");
    // Both reviewers are explicitly missing rather than inferred from the decision.
    let p1 = find_proposal(&value, "p1");
    for slot in 0..2 {
        assert_eq!(p1["reviewer_verdicts"][slot]["state"], "missing");
        assert_eq!(p1["reviewer_verdicts"][slot]["decision"], Value::Null);
    }
    // Accepted planning without a task is a gap, never a claim of completed work.
    assert!(
        p1["gaps"]
            .as_array()
            .unwrap()
            .iter()
            .any(|g| g.as_str().unwrap().contains("acceptance is not execution"))
    );
}

#[test]
fn malformed_and_missing_reviewer_batches_never_shift_identities() {
    let c = cycle(
        "cycle-a",
        "execution",
        json!([proposal("p1", "accepted")]),
        // Reviewer A's batch is unusable; reviewer B's is valid and must stay B's.
        json!([
            json!({"unexpected": "not an assessment list"}),
            batch(json!([
                {"id": "p1", "decision": "rejected", "reason": "b rejects"},
                {"id": "", "decision": "accepted", "reason": "unreadable identity"}
            ]))
        ]),
        json!([
            reviewer_session("adversary-a", "completed"),
            reviewer_session("adversary-b", "completed")
        ]),
    );
    let (_temp, store, _path) = fixture(&[c], &[]);
    let value = store.run_evidence("cycle-a").unwrap().unwrap();
    let verdicts = &find_proposal(&value, "p1")["reviewer_verdicts"];
    assert_eq!(verdicts[0]["reviewer"], "adversary-a");
    assert_eq!(verdicts[0]["state"], "malformed");
    assert_eq!(verdicts[0]["decision"], Value::Null);
    assert_eq!(verdicts[1]["reviewer"], "adversary-b");
    assert_eq!(verdicts[1]["state"], "recorded");
    assert_eq!(verdicts[1]["decision"], "rejected");
    let joined = value["gaps"]
        .as_array()
        .unwrap()
        .iter()
        .map(|g| g.as_str().unwrap())
        .collect::<Vec<_>>()
        .join(" | ");
    assert!(joined.contains("is malformed"), "{joined}");
    assert!(joined.contains("not reassigned"), "{joined}");
    assert!(joined.contains("unreadable entries"), "{joined}");
}

#[test]
fn unconfirmed_and_duplicate_reviewer_evidence_stays_explicit() {
    let c = cycle(
        "cycle-a",
        "execution",
        json!([proposal("p1", "accepted")]),
        json!([batch(json!([
            {"id": "p1", "decision": "accepted", "reason": "first"},
            {"id": "p1", "decision": "rejected", "reason": "second"}
        ]))]),
        // A recorded but failed reviewer session cannot confirm the batch's identity.
        json!([reviewer_session("adversary-a", "failed")]),
    );
    let (_temp, store, _path) = fixture(&[c], &[]);
    let value = store.run_evidence("cycle-a").unwrap().unwrap();
    let verdicts = &find_proposal(&value, "p1")["reviewer_verdicts"];
    assert_eq!(verdicts[0]["state"], "duplicate");
    assert_eq!(verdicts[0]["decision"], Value::Null);
    let note = verdicts[0]["note"].as_str().unwrap();
    assert!(note.contains("inconsistent"), "{note}");
    assert!(note.contains("unconfirmed"), "{note}");
    assert_eq!(verdicts[1]["state"], "missing");
}

#[test]
fn repeated_proposal_ids_across_cycles_do_not_cross_runs() {
    let first = cycle(
        "cycle-a",
        "execution",
        json!([proposal("p1", "accepted")]),
        json!([]),
        json!([]),
    );
    let second = cycle(
        "cycle-b",
        "execution",
        json!([proposal("p1", "accepted")]),
        json!([]),
        json!([]),
    );
    let a = task("cycle-a", "p1");
    let b = task("cycle-b", "p1");
    let (_temp, store, _path) = fixture(&[first, second], &[a.clone(), b.clone()]);
    for (id, expected) in [("cycle-a", &a), ("cycle-b", &b)] {
        let value = store.run_evidence(id).unwrap().unwrap();
        let linked = find_proposal(&value, "p1")["linked_tasks"]
            .as_array()
            .unwrap();
        assert_eq!(linked.len(), 1, "{id} joined across cycles");
        assert_eq!(linked[0]["id"], expected.id);
        assert_eq!(linked[0]["cycle_id"], id);
    }
}

#[test]
fn multiple_task_matches_are_preserved_and_none_is_selected() {
    let c = cycle(
        "cycle-a",
        "execution",
        json!([proposal("p1", "accepted")]),
        json!([]),
        json!([]),
    );
    let mut newest = task("cycle-a", "p1");
    newest.updated_at = "2099-01-01T00:00:00Z".into();
    newest.status = octomus_agent::model::Status::Published;
    let older = task("cycle-a", "p1");
    let (_temp, store, _path) = fixture(&[c], &[newest.clone(), older.clone()]);
    let value = store.run_evidence("cycle-a").unwrap().unwrap();
    let p1 = find_proposal(&value, "p1");
    let linked = p1["linked_tasks"].as_array().unwrap();
    assert_eq!(linked.len(), 2);
    let ids: Vec<&str> = linked.iter().map(|t| t["id"].as_str().unwrap()).collect();
    assert!(ids.contains(&newest.id.as_str()) && ids.contains(&older.id.as_str()));
    assert!(
        p1["gaps"]
            .as_array()
            .unwrap()
            .iter()
            .any(|g| g.as_str().unwrap().contains("2 tasks match"))
    );
}

#[test]
fn audit_acceptance_without_tasks_is_not_execution() {
    let c = cycle(
        "cycle-a",
        "audit",
        json!([proposal("p1", "accepted")]),
        json!([batch(
            json!([{"id": "p1", "decision": "accepted", "reason": "a accepts"}])
        )]),
        json!([reviewer_session("adversary-a", "completed")]),
    );
    let (_temp, store, _path) = fixture(&[c], &[]);
    let value = store.run_evidence("cycle-a").unwrap().unwrap();
    assert_eq!(value["cycle"]["mode"], "audit");
    assert_eq!(value["cycle"]["planning"]["creates_execution_queue"], false);
    let p1 = find_proposal(&value, "p1");
    assert_eq!(p1["final_decision"], "accepted");
    assert!(p1["linked_tasks"].as_array().unwrap().is_empty());
    // An audit recommendation without a task is expected, not a gap.
    assert!(
        !p1["gaps"]
            .as_array()
            .unwrap()
            .iter()
            .any(|g| g.as_str().unwrap().contains("acceptance is not execution")),
        "{:?}",
        p1["gaps"]
    );
    assert!(
        value["limitations"]
            .as_array()
            .unwrap()
            .iter()
            .any(|l| l.as_str().unwrap().contains("Audit acceptance"))
    );
}

#[test]
fn latest_review_governs_and_incomplete_or_empty_summaries_are_not_clean() {
    let c = cycle(
        "cycle-a",
        "execution",
        json!([proposal("p1", "accepted")]),
        json!([]),
        json!([]),
    );
    // A clean round followed by a later unclean one: the latest saved review governs.
    let mut regressed = task("cycle-a", "p1");
    regressed.output_commit = Some("out00001".into());
    regressed.reviews = serde_json::from_value(json!([
        review("out00001", true, "clean earlier round", json!([])),
        review(
            "out00001",
            true,
            "later round found a problem",
            json!([{"title": "t", "file": "f", "detail": "d", "priority": "high"}])
        )
    ]))
    .unwrap();
    let (_temp, store, _path) = fixture(std::slice::from_ref(&c), &[regressed]);
    let value = store.run_evidence("cycle-a").unwrap().unwrap();
    let t = &find_proposal(&value, "p1")["linked_tasks"][0];
    assert_eq!(t["latest_review"]["rounds_recorded"], 2);
    assert_eq!(t["latest_review"]["clean"], false);
    assert_eq!(t["latest_review"]["clean_at_output_revision"], false);
    assert_eq!(
        t["latest_review"]["latest"]["findings"]
            .as_array()
            .unwrap()
            .len(),
        1
    );

    for (completed, summary) in [(false, "interrupted"), (true, "   ")] {
        let mut t = task("cycle-a", "p1");
        t.output_commit = Some("out00001".into());
        t.reviews =
            serde_json::from_value(json!([review("out00001", completed, summary, json!([]))]))
                .unwrap();
        let (_temp, store, _path) = fixture(std::slice::from_ref(&c), &[t]);
        let value = store.run_evidence("cycle-a").unwrap().unwrap();
        let t = &find_proposal(&value, "p1")["linked_tasks"][0];
        assert_eq!(
            t["latest_review"]["clean"], false,
            "{completed} {summary:?}"
        );
        assert_eq!(t["latest_review"]["latest"]["completed"], completed);
        assert_eq!(
            t["latest_review"]["latest"]["summary_present"],
            !summary.trim().is_empty()
        );
    }
}

#[test]
fn later_failures_missing_results_and_mismatched_revisions_are_not_passing() {
    let c = cycle(
        "cycle-a",
        "execution",
        json!([proposal("p1", "accepted")]),
        json!([]),
        json!([]),
    );
    let mut t = task("cycle-a", "p1");
    t.output_commit = Some("out00001".into());
    t.config.verification_commands = vec![
        "make check".into(),
        "make test".into(),
        "make never-run".into(),
    ];
    t.reviews =
        serde_json::from_value(json!([review("out00001", true, "clean review", json!([]))]))
            .unwrap();
    t.verification = serde_json::from_value(json!([
        // A newer failure invalidates the older pass.
        check("make check", true, "out00001"),
        check("make check", false, "out00001"),
        // A pass recorded against a different revision does not transfer.
        check("make test", true, "stale000")
    ]))
    .unwrap();
    let (_temp, store, _path) = fixture(&[c], &[t]);
    let value = store.run_evidence("cycle-a").unwrap().unwrap();
    let commands = &find_proposal(&value, "p1")["linked_tasks"][0]["required_commands"];
    let by_name = |name: &str| {
        commands["commands"]
            .as_array()
            .unwrap()
            .iter()
            .find(|c| c["command"] == name)
            .unwrap()
            .clone()
    };
    let check_result = by_name("make check");
    assert_eq!(check_result["state"], "failed");
    assert_eq!(check_result["results_recorded"], 2);
    assert_eq!(check_result["latest_success"], false);
    let test_result = by_name("make test");
    assert_eq!(test_result["state"], "passed_at_other_revision");
    assert_eq!(test_result["matches_output_revision"], false);
    assert_eq!(test_result["latest_revision"], "stale000");
    let missing = by_name("make never-run");
    assert_eq!(missing["state"], "no_result");
    assert_eq!(missing["latest_success"], Value::Null);
    assert_eq!(commands["all_passed_at_output_revision"], false);
    let gaps = find_proposal(&value, "p1")["linked_tasks"][0]["gaps"].clone();
    assert!(
        gaps.as_array()
            .unwrap()
            .iter()
            .any(|g| g.as_str().unwrap().contains("every required command")),
        "{gaps:?}"
    );
}

#[test]
fn no_configured_checks_is_not_configured_rather_than_passing() {
    let c = cycle(
        "cycle-a",
        "execution",
        json!([proposal("p1", "accepted")]),
        json!([]),
        json!([]),
    );
    let mut t = task("cycle-a", "p1");
    t.config.verification_commands = vec![];
    t.output_commit = Some("out00001".into());
    let (_temp, store, _path) = fixture(&[c], &[t]);
    let value = store.run_evidence("cycle-a").unwrap().unwrap();
    let commands = &find_proposal(&value, "p1")["linked_tasks"][0]["required_commands"];
    assert_eq!(commands["state"], "not_configured");
    assert!(commands["commands"].as_array().unwrap().is_empty());
    assert_eq!(commands["all_passed_at_output_revision"], false);
}

#[tokio::test]
async fn evidence_route_requires_auth_and_reports_unknown_cycles() {
    let c = cycle(
        "cycle-a",
        "execution",
        json!([proposal("p1", "accepted")]),
        json!([]),
        json!([]),
    );
    let (temp, store, _path) = fixture(&[c], &[]);
    let app = App::new(store.clone(), temp.path().to_path_buf());
    let router = api::router(app, TOKEN, None);

    let unauthenticated = router
        .clone()
        .oneshot(
            Request::builder()
                .uri("/api/cycles/cycle-a/evidence")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(unauthenticated.status(), StatusCode::UNAUTHORIZED);

    let wrong_token = router
        .clone()
        .oneshot(
            Request::builder()
                .uri("/api/cycles/cycle-a/evidence")
                .header("authorization", "Bearer not-the-operator-token-000000000")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(wrong_token.status(), StatusCode::UNAUTHORIZED);

    let (status, _) = api_evidence(&store, temp.path(), "cycle-a").await;
    assert_eq!(status, StatusCode::OK);
    let (status, body) = api_evidence(&store, temp.path(), "cycle-missing").await;
    assert_eq!(status, StatusCode::NOT_FOUND);
    assert_eq!(body["error"], "Cycle not found");
}

#[tokio::test]
async fn existing_cycle_action_routes_are_preserved() {
    let c = cycle(
        "cycle-a",
        "execution",
        json!([proposal("p1", "accepted")]),
        json!([]),
        json!([]),
    );
    let (temp, store, _path) = fixture(&[c], &[]);
    let app = App::new(store.clone(), temp.path().to_path_buf());
    let router = api::router(app, TOKEN, None);
    let action = |path: &str| {
        router.clone().oneshot(
            Request::builder()
                .uri(format!("/api/cycles/cycle-a/{path}"))
                .method("POST")
                .header("authorization", format!("Bearer {TOKEN}"))
                .header("content-type", "application/json")
                .body(Body::empty())
                .unwrap(),
        )
    };
    assert_eq!(action("archive").await.unwrap().status(), StatusCode::OK);
    let archived: Cycle = store.get("cycle", "cycle-a").unwrap().unwrap();
    assert!(archived.lifecycle.archived_at.is_some());
    // Cycle detail and the unchanged action conflict path still behave as before.
    let detail = router
        .clone()
        .oneshot(
            Request::builder()
                .uri("/api/cycles/cycle-a")
                .header("authorization", format!("Bearer {TOKEN}"))
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(detail.status(), StatusCode::OK);
    assert_eq!(
        action("bogus").await.unwrap().status(),
        StatusCode::CONFLICT
    );
}

#[tokio::test]
async fn api_and_cli_facts_agree_apart_from_generation_metadata() {
    let mut t = task("cycle-a", "p1");
    t.output_commit = Some("out00001".into());
    t.reviews =
        serde_json::from_value(json!([review("out00001", true, "clean review", json!([]))]))
            .unwrap();
    t.verification =
        serde_json::from_value(json!([check("make check", true, "out00001")])).unwrap();
    let c = cycle(
        "cycle-a",
        "execution",
        json!([proposal("p1", "accepted")]),
        json!([batch(
            json!([{"id": "p1", "decision": "accepted", "reason": "a accepts"}])
        )]),
        json!([reviewer_session("adversary-a", "completed")]),
    );
    let (temp, store, path) = fixture(&[c], &[t]);
    let (status, mut from_api) = api_evidence(&store, temp.path(), "cycle-a").await;
    assert_eq!(status, StatusCode::OK);
    let mut from_cli = evidence::export_run(&path, "cycle-a").unwrap();
    for value in [&mut from_api, &mut from_cli] {
        assert!(value["generated_at"].is_string());
        value.as_object_mut().unwrap().remove("generated_at");
    }
    assert_eq!(from_api, from_cli);
}

#[test]
fn cli_export_is_read_only_and_errors_explicitly() {
    let mut t = task("cycle-a", "p1");
    t.output_commit = Some("out00001".into());
    let c = cycle(
        "cycle-a",
        "execution",
        json!([proposal("p1", "accepted")]),
        json!([]),
        json!([]),
    );
    let (temp, store, path) = fixture(&[c], &[t]);

    let listing = |dir: &std::path::Path| {
        let mut names: Vec<String> = std::fs::read_dir(dir)
            .unwrap()
            .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
            .collect();
        names.sort();
        names
    };
    let before_files = listing(temp.path());
    let before_bytes = std::fs::read(&path).unwrap();
    let before_records: i64 = {
        let c = rusqlite::Connection::open(&path).unwrap();
        c.query_row("SELECT count(*) FROM records", [], |r| r.get(0))
            .unwrap()
    };

    let value = evidence::export_run(&path, "cycle-a").unwrap();
    assert_eq!(value["cycle"]["id"], "cycle-a");

    // Unknown cycles and unknown state are explicit errors, not empty successful exports.
    let unknown = evidence::export_run(&path, "cycle-missing").unwrap_err();
    assert!(
        format!("{unknown:#}").contains("No saved cycle cycle-missing"),
        "{unknown:#}"
    );
    let absent = temp.path().join("missing-dir/state.db");
    assert!(evidence::export_run(&absent, "cycle-a").is_err());
    assert!(!absent.parent().unwrap().exists());

    assert_eq!(std::fs::read(&path).unwrap(), before_bytes);
    assert_eq!(listing(temp.path()), before_files);
    let after_records: i64 = {
        let c = rusqlite::Connection::open(&path).unwrap();
        c.query_row("SELECT count(*) FROM records", [], |r| r.get(0))
            .unwrap()
    };
    assert_eq!(after_records, before_records);
    drop(store);
}

#[test]
fn export_run_flag_returns_before_touching_application_state() {
    let temp = tempfile::tempdir().unwrap();
    let data_dir = temp.path().join("state-dir");
    let binary = env!("CARGO_BIN_EXE_octomus-agent");
    let run = |args: &[&str]| {
        std::process::Command::new(binary)
            .args(args)
            .env_remove("OCTOMUS_DATA_DIR")
            .env_remove("OCTOMUS_TOKEN")
            .env_remove("OCTOMUS_LISTEN")
            .output()
            .unwrap()
    };

    // No saved state: an explicit failure that never creates the data directory,
    // takes the service lock or starts a worker.
    let missing = run(&[
        "--data-dir",
        data_dir.to_str().unwrap(),
        "--export-run",
        "cycle-a",
    ]);
    assert!(!missing.status.success());
    assert!(!data_dir.exists(), "export created the data directory");
    assert!(String::from_utf8_lossy(&missing.stderr).contains("state database"));
    assert!(missing.stdout.is_empty());

    // Incompatible action flags are rejected.
    let conflict = run(&[
        "--data-dir",
        data_dir.to_str().unwrap(),
        "--export-run",
        "cycle-a",
        "--usage-report",
    ]);
    assert!(!conflict.status.success());
    assert!(
        String::from_utf8_lossy(&conflict.stderr).contains("cannot be used with"),
        "{}",
        String::from_utf8_lossy(&conflict.stderr)
    );
    for extra in ["--doctor", "--print-config"] {
        let conflict = run(&[
            "--data-dir",
            data_dir.to_str().unwrap(),
            "--export-run",
            "cycle-a",
            extra,
        ]);
        assert!(!conflict.status.success(), "{extra} was accepted");
    }

    // With synthetic saved state the flag prints the representation on stdout only.
    let c = cycle(
        "cycle-a",
        "execution",
        json!([proposal("p1", "accepted")]),
        json!([]),
        json!([]),
    );
    std::fs::create_dir_all(&data_dir).unwrap();
    let store = Store::open(&data_dir.join("state.db")).unwrap();
    store.put("cycle", &c.id, &c).unwrap();
    let exported = run(&[
        "--data-dir",
        data_dir.to_str().unwrap(),
        "--export-run",
        "cycle-a",
    ]);
    assert!(
        exported.status.success(),
        "{}",
        String::from_utf8_lossy(&exported.stderr)
    );
    let value: Value = serde_json::from_slice(&exported.stdout).unwrap();
    assert_eq!(value["cycle"]["id"], "cycle-a");
    assert_eq!(value["schema_version"], evidence::SCHEMA_VERSION);
    assert!(!data_dir.join("service.lock").exists());
    drop(store);
}
