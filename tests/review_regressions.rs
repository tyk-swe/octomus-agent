use octomus_agent::{
    config::{Config, Route},
    engine::App,
    model::{Cycle, Status, Task, id, now},
    store::{HistoryQuery, Store},
};
use serde_json::{Value, json};
use tower::ServiceExt;

fn task() -> Task {
    serde_json::from_value(json!({
        "id":id(),"cycle_id":"original-cycle",
        "proposal":{"id":"proposal","title":"Concrete improvement","problem":"Missing behavior","benefit":"Useful behavior","scope":"one file","evidence":["README.md"],"category":"features","target":"main","tier":"M","dependencies":[],"prompt":"Implement the documented behavior","decision":"accepted","reason":"Grounded","problem_key":"stable-problem"},
        "status":"queued","route":Route::new("fixture","low"),"config":Config {github_repo:"Fixture/Project".into(),..Config::default()},
        "source_revision":"source","comparison_base":"source","default_revision":"source","branch":"octomus/work","workspace":"","execution_session":null,"repair_session":null,"sessions":[],"reviews":[],"verification":[],"output_commit":null,"pr_number":null,"pr_url":null,"attempts":0,"error":null,"created_at":now(),"updated_at":now()
    })).unwrap()
}

fn cycle(t: &Task) -> Cycle {
    serde_json::from_value(json!({
        "id":t.cycle_id,"number":1,"status":"running","started_at":now(),"completed_at":null,
        "grounding":null,"proposals":[t.proposal],"assessments":[],"sessions":[],"error":null,
        "repository":t.config.github_repo
    }))
    .unwrap()
}

#[tokio::test]
async fn cancellation_requires_explicit_rediscovery_and_preserves_the_route() {
    let temp = tempfile::tempdir().unwrap();
    let store = Store::open(&temp.path().join("state.db")).unwrap();
    let original = task();
    store.put("task", &original.id, &original).unwrap();
    let app = App::new(store.clone(), temp.path().into());
    let token = "rediscovery-fixture-token-at-least-32-characters";
    let router = octomus_agent::api::router(app, token, None);
    for (action, expected) in [("cancel", 200), ("supersede", 200), ("supersede", 409)] {
        let response = router
            .clone()
            .oneshot(
                axum::http::Request::builder()
                    .uri(format!("/api/tasks/{}/{action}", original.id))
                    .method("POST")
                    .header("authorization", format!("Bearer {token}"))
                    .header("content-type", "application/json")
                    .body(axum::body::Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status().as_u16(), expected, "{action}");
        let saved: Task = store.get("task", &original.id).unwrap().unwrap();
        assert_eq!(saved.status, Status::Cancelled);
        assert_eq!(saved.rediscovery_requested, action != "cancel");
        assert_eq!(json!(saved.route), json!(original.route));
        assert_eq!(json!(saved.config), json!(original.config));
    }
    let mut cancelled = original;
    cancelled.status = Status::Cancelled;
    assert!(cancelled.allowed_actions().contains(&"supersede"));
    for field in [
        "output_commit",
        "superseded_by",
        "archived_at",
        "discarded_at",
    ] {
        let mut ineligible = cancelled.clone();
        match field {
            "output_commit" => ineligible.output_commit = Some("delivered".into()),
            "superseded_by" => ineligible.superseded_by.push("replacement".into()),
            "archived_at" => ineligible.lifecycle.archived_at = Some(now()),
            _ => ineligible.lifecycle.discarded_at = Some(now()),
        }
        assert!(
            !ineligible.allowed_actions().contains(&"supersede"),
            "{field}"
        );
    }
}

#[test]
fn repository_history_and_rediscovery_lineage_ignore_repository_casing() {
    let temp = tempfile::tempdir().unwrap();
    let path = temp.path().join("state.db");
    let store = Store::open(&path).unwrap();
    let mut delivered = task();
    delivered.status = Status::Published;
    delivered.pr_number = Some(42);
    delivered.output_commit = Some("delivered-head".into());
    store.put("task", &delivered.id, &delivered).unwrap();
    let decision = json!({"id":"decision","repository":delivered.config.github_repo});
    store.put("decision", "decision", &decision).unwrap();
    let observation = json!({
        "repository": delivered.config.github_repo,
        "pr": {"number":42,"title":delivered.proposal.title,"branch":delivered.branch,"head":"delivered-head","base":"main","url":"https://github.com/Fixture/Project/pull/42","body":"Fixture","state":"open","changed_lines":1,"created_at":now(),"owned":true},
        "observed_at":now(),"delivered_head":"delivered-head","external_head_movement":false
    });
    store.put("pr", "Fixture/Project:42", &observation).unwrap();
    drop(store);
    let store = Store::open(&path).unwrap();
    for repository in ["fixture/project", "Fixture/Project", "FIXTURE/PROJECT"] {
        for match_title in [true, false] {
            let mut proposed = delivered.proposal.clone();
            if match_title {
                proposed.problem_key = "another-key".into();
            } else {
                proposed.title = "A different title for the same problem".into();
            }
            let matches = store
                .duplicate_tasks(repository, std::slice::from_ref(&proposed))
                .unwrap();
            assert_eq!(matches.len(), 1);
            assert_eq!(matches[0].id, delivered.id);
            proposed.target = "Main".into();
            assert!(
                store
                    .duplicate_tasks(repository, &[proposed])
                    .unwrap()
                    .is_empty()
            );
        }
        assert_eq!(
            store.decision_memory(repository).unwrap(),
            std::slice::from_ref(&decision)
        );
        assert_eq!(
            store.latest_pr_output(repository, 42).unwrap().as_deref(),
            Some("delivered-head")
        );
        let (record_id, observed) = store.pr_observation(repository, 42).unwrap().unwrap();
        assert_eq!(record_id, "Fixture/Project:42");
        assert_eq!(observed.delivered_head.as_deref(), Some("delivered-head"));
    }
    assert!(
        store
            .duplicate_tasks("different/project", &[delivered.proposal])
            .unwrap()
            .is_empty()
    );
    assert!(
        store
            .decision_memory("different/project")
            .unwrap()
            .is_empty()
    );
    assert!(
        store
            .latest_pr_output("different/project", 42)
            .unwrap()
            .is_none()
    );
    assert!(
        store
            .pr_observation("different/project", 42)
            .unwrap()
            .is_none()
    );

    let mut old = task();
    old.status = Status::Cancelled;
    old.rediscovery_requested = true;
    store.put("task", &old.id, &old).unwrap();
    assert!(
        store
            .rediscovery_requests("different/project")
            .unwrap()
            .is_empty()
    );
    assert_eq!(
        store.rediscovery_requests("fixture/project").unwrap()[0]["id"],
        old.id
    );
    let mut replacement = task();
    replacement.cycle_id = "replacement-cycle".into();
    replacement.config.github_repo = "fixture/project".into();
    replacement.supersedes = vec![old.id.clone()];
    replacement.proposal.reconsiders = vec![old.id.clone()];
    store
        .commit_plan(&cycle(&replacement), std::slice::from_ref(&replacement))
        .unwrap();
    let saved: Task = store.get("task", &old.id).unwrap().unwrap();
    assert_eq!(saved.superseded_by, [replacement.id]);
    assert!(!saved.rediscovery_requested);
    assert_eq!(saved.config.github_repo, "Fixture/Project");
    assert!(
        store
            .rediscovery_requests("FIXTURE/PROJECT")
            .unwrap()
            .is_empty()
    );
}

#[test]
fn duplicate_titles_trim_saved_and_proposed_whitespace_after_upgrade() {
    for legacy in [false, true] {
        let temp = tempfile::tempdir().unwrap();
        let path = temp.path().join("state.db");
        let store = Store::open(&path).unwrap();
        let mut delivered = task();
        delivered.status = Status::Published;
        delivered.proposal.title = "  Concrete improvement  ".into();
        store.put("task", &delivered.id, &delivered).unwrap();
        drop(store);
        if legacy {
            let connection = rusqlite::Connection::open(&path).unwrap();
            connection.execute_batch(
                "BEGIN;
                DROP INDEX meta_duplicate;
                CREATE INDEX meta_duplicate ON record_meta(kind,repository COLLATE NOCASE,target,title COLLATE NOCASE,status);
                PRAGMA user_version=3;
                COMMIT;",
            ).unwrap();
        }
        let store = Store::open(&path).unwrap();
        let saved: Value = store.get("task", &delivered.id).unwrap().unwrap();
        assert_eq!(saved, json!(delivered), "Migration changed saved evidence");
        let mut proposed = delivered.proposal.clone();
        proposed.title = "concrete IMPROVEMENT".into();
        proposed.problem_key = "different-problem-key".into();
        let matches = store
            .duplicate_tasks("fixture/project", &[proposed.clone()])
            .unwrap();
        assert_eq!(
            matches.len(),
            1,
            "Existing titles must match after reopening"
        );
        assert_eq!(
            matches[0].id, delivered.id,
            "Existing titles must match immediately after reopening"
        );
        for whitespace in ["", " ", "\t\r\n", "\u{85}\u{a0}\u{2003}\u{202f}\u{3000}"] {
            delivered.proposal.title = format!("{whitespace}Concrete improvement{whitespace}");
            store.put("task", &delivered.id, &delivered).unwrap();
            for title in ["concrete IMPROVEMENT", "\tConcrete improvement\u{a0}"] {
                proposed.title = title.into();
                let matches = store
                    .duplicate_tasks("fixture/project", &[proposed.clone()])
                    .unwrap();
                assert_eq!(matches.len(), 1, "Saved whitespace: {whitespace:?}");
                assert_eq!(json!(matches[0]), json!(delivered));
            }
        }
        for title in ["Concrete  improvement", "\u{200b}Concrete improvement"] {
            proposed.title = title.into();
            assert!(
                store
                    .duplicate_tasks("fixture/project", &[proposed.clone()])
                    .unwrap()
                    .is_empty()
            );
        }
        proposed.title = "Concrete improvement".into();
        delivered.lifecycle.archived_at = Some(now());
        store.put("task", &delivered.id, &delivered).unwrap();
        assert!(
            store
                .duplicate_tasks("fixture/project", &[proposed.clone()])
                .unwrap()
                .is_empty()
        );
        delivered.lifecycle.archived_at = None;
        delivered.status = Status::Cancelled;
        store.put("task", &delivered.id, &delivered).unwrap();
        assert!(
            store
                .duplicate_tasks("fixture/project", &[proposed])
                .unwrap()
                .is_empty()
        );
    }
}

#[test]
fn proposal_content_revisions_cover_omitted_and_truncated_evidence_after_upgrade() {
    for legacy in [false, true] {
        let temp = tempfile::tempdir().unwrap();
        let path = temp.path().join("state.db");
        let store = Store::open(&path).unwrap();
        let mut cycle = cycle(&task());
        cycle.proposals[0].reason = "r".repeat(2100);
        store.put("cycle", &cycle.id, &cycle).unwrap();
        drop(store);
        if legacy {
            // Recreate the prior projection schema with its saved evidence intact.
            let connection = rusqlite::Connection::open(&path).unwrap();
            connection
                .execute_batch(
                    "BEGIN;
                DROP TRIGGER project_proposals_insert;
                DROP TRIGGER project_proposals_update;
                ALTER TABLE proposal_records DROP COLUMN content_revision;
                PRAGMA user_version=2;
                COMMIT;",
                )
                .unwrap();
        }
        let store = Store::open(&path).unwrap();
        let original: Value = store.get("cycle", &cycle.id).unwrap().unwrap();
        assert_eq!(original, json!(cycle));
        let summary = store
            .proposal_page(&HistoryQuery::default())
            .unwrap()
            .items
            .remove(0);
        assert_eq!(summary["content_revision"], 1);
        assert_eq!(summary["prompt"], "");
        assert_eq!(summary["evidence"], json!([]));
        // Changes outside the proposal must not invalidate cached detail.
        cycle.status = "completed".into();
        store.put("cycle", &cycle.id, &cycle).unwrap();
        assert_eq!(
            store.proposal_page(&HistoryQuery::default()).unwrap().items[0],
            summary
        );
        for revision in 2..=4 {
            match revision {
                2 => cycle.proposals[0].prompt = "Consolidated execution instructions".into(),
                3 => cycle.proposals[0]
                    .evidence
                    .push("Fresh file evidence".into()),
                _ => cycle.proposals[0]
                    .reason
                    .push_str("Updated rationale after the truncated prefix"),
            }
            store.put("cycle", &cycle.id, &cycle).unwrap();
            let mut refreshed = store
                .proposal_page(&HistoryQuery::default())
                .unwrap()
                .items
                .remove(0);
            assert_eq!(refreshed["content_revision"], revision);
            refreshed["content_revision"] = json!(1);
            assert_eq!(
                refreshed, summary,
                "Only the revision changes in the summary"
            );
            let mut detail = store
                .proposal_detail(&cycle.id, &cycle.proposals[0].id)
                .unwrap()
                .unwrap();
            assert_eq!(
                detail.as_object_mut().unwrap().remove("content_revision"),
                Some(json!(revision))
            );
            assert_eq!(detail, json!(cycle.proposals[0]));
        }
        drop(store);
        let store = Store::open(&path).unwrap();
        store.put("cycle", &cycle.id, &cycle).unwrap();
        assert_eq!(
            store.proposal_page(&HistoryQuery::default()).unwrap().items[0]["content_revision"],
            4
        );
        let saved: Value = store.get("cycle", &cycle.id).unwrap().unwrap();
        assert_eq!(saved, json!(cycle));
    }
}
