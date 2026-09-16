//! Regressions for the September 2026 source review findings F1–F6.
use octomus_agent::{
    config::{Config, Route},
    engine::{App, resolve_target, validate_proposals},
    git,
    model::*,
    process,
    store::Store,
};
use serde_json::json;
use tokio_util::sync::CancellationToken;

fn task() -> Task {
    serde_json::from_value(json!({
        "id":id(),"cycle_id":"cycle","proposal":{"id":"a","title":"Concrete improvement","problem":"Missing behavior","benefit":"Useful behavior","scope":"one file","evidence":["README.md"],"category":"features","target":"main","tier":"M","dependencies":[],"prompt":"Implement the documented behavior","decision":"accepted","reason":"Grounded"},
        "status":"queued","route":Route::new("fixture","low"),"config":Config {github_repo:"fixture/project".into(),..Config::default()},
        "source_revision":"source","comparison_base":"source","default_revision":"source","branch":"octomus/work","workspace":"","execution_session":null,"repair_session":null,"sessions":[],"reviews":[],"verification":[],"output_commit":null,"pr_number":null,"pr_url":null,"attempts":0,"error":null,"created_at":now(),"updated_at":now()
    })).unwrap()
}

fn proposal(id: &str, title: &str, key: &str) -> Proposal {
    Proposal {
        id: id.into(),
        title: title.into(),
        problem: "Truncated frames are accepted".into(),
        evidence: vec!["src/parser.rs".into()],
        benefit: "Rejects malformed input".into(),
        category: "features".into(),
        target: "main".into(),
        tier: "M".into(),
        scope: "parser".into(),
        dependencies: vec![],
        prompt: "Fix the parser".into(),
        decision: "accepted".into(),
        reason: "Grounded".into(),
        problem_key: key.into(),
        relevant_paths: vec![],
        reconsiders: vec![],
    }
}

fn pr(number: u64, branch: &str, head: &str, owned: bool) -> PullRequest {
    serde_json::from_value(json!({
        "number": number, "title": "PR", "branch": branch, "head": head, "base": "main",
        "url": format!("https://example.invalid/pr/{number}"), "body": "", "state": "open",
        "changed_lines": 1, "created_at": now(), "owned": owned,
        "head_repository": if owned { "fixture/project" } else { "fork/project" },
        "base_repository": "fixture/project"
    }))
    .unwrap()
}

/// A real Git workspace at one commit with a tracked `impl.txt` containing `0`.
async fn workspace(config: &Config, cancel: &CancellationToken) -> (tempfile::TempDir, String) {
    let temp = tempfile::tempdir().unwrap();
    let repo = temp.path().join("workspace");
    std::fs::create_dir(&repo).unwrap();
    std::fs::write(repo.join("impl.txt"), "0\n").unwrap();
    for args in [
        vec!["init", "-b", "main"],
        vec!["add", "impl.txt"],
        vec![
            "-c",
            "user.name=Fixture",
            "-c",
            "user.email=fixture@example.com",
            "commit",
            "-m",
            "Fixture",
        ],
    ] {
        git::git(config, &repo, &args, cancel).await.unwrap();
    }
    let revision = git::git(config, &repo, &["rev-parse", "HEAD"], cancel)
        .await
        .unwrap();
    (temp, revision)
}

async fn verify(commands: &[&str]) -> (Task, anyhow::Result<Vec<String>>) {
    let cancel = CancellationToken::new();
    let config = Config {
        verification_commands: commands.iter().map(|c| c.to_string()).collect(),
        command_timeout_seconds: 10,
        ..Config::default()
    };
    let (temp, revision) = workspace(&config, &cancel).await;
    let store = Store::open(&temp.path().join("state.db")).unwrap();
    let app = App::new(store, temp.path().into());
    let mut t = task();
    t.config = config;
    t.attempt_policy = None;
    t.workspace = temp.path().join("workspace").to_string_lossy().into_owned();
    t.status = Status::Reviewing;
    let result = app.verify_revision(&mut t, &revision, &cancel).await;
    (t, result)
}

fn blocked_by(result: &anyhow::Result<Vec<String>>, reason: BlockedReason) -> bool {
    result
        .as_ref()
        .err()
        .is_some_and(|e| BlockedReason::from_error(e) == reason)
}

// F1
#[tokio::test]
async fn verification_command_that_mutates_the_worktree_is_failed_evidence_and_stops_the_run() {
    let (t, result) = verify(&[
        "printf 1 > impl.txt",
        "test \"$(cat impl.txt)\" = 1",
        "git checkout -- impl.txt",
    ])
    .await;
    assert!(
        blocked_by(&result, BlockedReason::WorkspaceInvalid),
        "{result:?}"
    );
    assert_eq!(t.verification.len(), 1, "later commands must not run");
    let first = &t.verification[0];
    assert!(!first.success && first.command == "printf 1 > impl.txt");
    assert!(
        first
            .output
            .contains("changed during this verification command")
    );
}

// F1
#[tokio::test]
async fn verification_command_that_moves_head_is_failed_evidence() {
    let (t, result) = verify(&[
        "git -c user.name=x -c user.email=x@example.com commit --allow-empty -m moved",
        "true",
    ])
    .await;
    assert!(
        blocked_by(&result, BlockedReason::WorkspaceInvalid),
        "{result:?}"
    );
    assert_eq!(t.verification.len(), 1);
    assert!(!t.verification[0].success);
}

// F1 + F6
#[tokio::test]
async fn intact_verification_records_every_success_with_both_streams() {
    let (t, result) = verify(&["echo out; echo warn >&2", "true"]).await;
    assert_eq!(result.unwrap(), Vec::<String>::new());
    assert_eq!(t.verification.len(), 2);
    assert!(t.verification.iter().all(|v| v.success));
    assert!(t.verification[0].output.contains("out") && t.verification[0].output.contains("warn"));
}

// F6
#[tokio::test]
async fn successful_runs_keep_stderr() {
    let tmp = tempfile::tempdir().unwrap();
    let cancel = CancellationToken::new();
    let only_stderr = process::run("bash", &["-c", "echo warn >&2"], tmp.path(), 10, &cancel)
        .await
        .unwrap();
    assert!(only_stderr.contains("warn"), "{only_stderr}");
    let both = process::run(
        "bash",
        &["-c", "echo out; echo err >&2"],
        tmp.path(),
        10,
        &cancel,
    )
    .await
    .unwrap();
    assert!(
        both.starts_with("out") && both.contains("[stderr]") && both.ends_with("err"),
        "{both}"
    );
    let quiet = process::run("bash", &["-c", "echo out"], tmp.path(), 10, &cancel)
        .await
        .unwrap();
    assert_eq!(quiet, "out");
    assert!(
        process::run(
            "bash",
            &["-c", "echo boom >&2; exit 3"],
            tmp.path(),
            10,
            &cancel
        )
        .await
        .unwrap_err()
        .to_string()
        .contains("boom")
    );
}

// F2
#[test]
fn repair_rounds_count_from_the_attempt_baseline() {
    let mut t = task();
    let round = ReviewRound {
        session_id: "s".into(),
        revision: "r".into(),
        comparison_base: "c".into(),
        result: Review {
            completed: true,
            summary: "clean".into(),
            findings: vec![],
        },
        created_at: now(),
    };
    t.reviews = vec![round.clone(), round];
    let max_repair_rounds = 1;
    assert!(
        t.attempt_reviews() > max_repair_rounds,
        "the exhausted first attempt is still exhausted"
    );
    t.review_baseline = t.reviews.len();
    assert_eq!(t.attempt_reviews(), 0);
    assert!(t.attempt_reviews() < max_repair_rounds + 1);
}

// F3
#[test]
fn same_cycle_proposals_sharing_a_problem_key_are_duplicates() {
    let c = Config::default();
    let g = Grounding {
        revision: "rev".into(),
        prs: vec![],
        external_prs: vec![],
        pr_coverage: PrCoverage::default(),
        history: json!({}),
        maintenance_due: false,
        maintenance_targets: vec![],
    };
    let a = proposal("a", "Reject truncated frames", "parser:truncated-frame");
    let b = proposal(
        "b",
        "Validate the complete frame length",
        "parser:truncated-frame",
    );
    let err = validate_proposals(&c, &[a.clone(), b.clone()], &g, &[]).unwrap_err();
    assert!(err.to_string().contains("Duplicate accepted"), "{err}");
    let mut other = b;
    other.problem_key = "parser:length-header".into();
    validate_proposals(&c, &[a, other], &g, &[]).unwrap();
}

// F4
#[test]
fn target_resolution_binds_the_owned_pr_regardless_of_order() {
    let c = Config {
        github_repo: "fixture/project".into(),
        ..Default::default()
    };
    let prs = vec![
        pr(202, "octomus/fix", "fork-head", false),
        pr(101, "octomus/fix", "repo-head", true),
    ];
    let bound = resolve_target(&c, &prs, "octomus/fix").unwrap().unwrap();
    assert_eq!((bound.number, bound.head.as_str()), (101, "repo-head"));
    assert!(resolve_target(&c, &prs, "main").unwrap().is_none());
    assert!(resolve_target(&c, &prs[..1], "octomus/fix").is_err());
    let ambiguous = vec![prs[1].clone(), prs[1].clone()];
    assert!(resolve_target(&c, &ambiguous, "octomus/fix").is_err());
    let g = Grounding {
        revision: "rev".into(),
        prs,
        external_prs: vec![],
        pr_coverage: PrCoverage::default(),
        history: json!({}),
        maintenance_due: false,
        maintenance_targets: vec![],
    };
    let mut p = proposal("a", "Fix", "");
    p.target = "octomus/fix".into();
    validate_proposals(&c, &[p], &g, &[]).unwrap();
}
