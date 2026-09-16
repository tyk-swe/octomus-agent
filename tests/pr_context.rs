use octomus_agent::{
    engine::{external_context, resolve_target},
    git,
    model::{Grounding, PullRequest},
};
use serde_json::{Value, json};

mod common;
use common::*;

fn entry(
    number: u64,
    branch: &str,
    head_repo: Value,
    base_repo: Value,
    body: &str,
    state: &str,
) -> Value {
    json!({
        "number": number,
        "title": format!("Pull request {number}"),
        "body": body,
        "state": state,
        "merged_at": Value::Null,
        "head": {"ref": branch, "sha": format!("{:040x}", number), "repo": head_repo},
        "base": {"ref": "main", "repo": base_repo},
        "html_url": format!("https://example.invalid/pull/{number}"),
        "additions": 3,
        "deletions": 1,
        "created_at": "2026-08-01T00:00:00Z"
    })
}

fn owned_entry(number: u64, branch: &str) -> Value {
    entry(
        number,
        branch,
        json!({"full_name": "fixture/project"}),
        json!({"full_name": "fixture/project"}),
        &format!("Owned work.\n<!-- octomus:task:task-{number} -->"),
        "open",
    )
}

fn external_entry(number: u64) -> Value {
    entry(
        number,
        "contributor/work",
        json!({"full_name": "contributor/project"}),
        json!({"full_name": "fixture/project"}),
        "External work without a task marker.",
        "open",
    )
}

#[test]
fn inventory_reads_every_page_dedupes_and_sorts() {
    let c = config();
    let pages: Vec<String> = [
        (1..=60).rev().map(external_entry).collect::<Vec<_>>(),
        (61..=120)
            .rev()
            .map(|n| owned_entry(n, &format!("octomus/work-{n}")))
            .collect::<Vec<_>>(),
        (121..=150)
            .rev()
            .map(|n| {
                if n % 2 == 0 {
                    owned_entry(n, &format!("octomus/work-{n}"))
                } else {
                    external_entry(n)
                }
            })
            .collect::<Vec<_>>(),
    ]
    .iter()
    .map(|page| json!(page).to_string())
    .collect();
    let inventory = git::parse_inventory(&pages.concat(), &c).unwrap();
    assert_eq!(inventory.repository, "fixture/project");
    assert!(!inventory.observed_at.is_empty());
    assert_eq!(inventory.prs.len(), 150);
    assert!(inventory.prs.windows(2).all(|w| w[0].number < w[1].number));
    assert_eq!(inventory.prs.iter().filter(|p| p.owned).count(), 75);
    let duplicate = format!(
        "{}{}",
        json!(vec![external_entry(7)]),
        json!(vec![external_entry(7)])
    );
    let inventory = git::parse_inventory(&duplicate, &c).unwrap();
    assert_eq!(inventory.prs.len(), 1);
}

#[test]
fn ownership_requires_prefix_head_repository_base_repository_and_marker() {
    let c = config();
    let cases = [
        (owned_entry(1, "octomus/owned"), true),
        (
            entry(
                2,
                "octomus/fork",
                json!({"full_name": "contributor/project"}),
                json!({"full_name": "fixture/project"}),
                "Fork work.\n<!-- octomus:task:fork -->",
                "open",
            ),
            false,
        ),
        (
            entry(
                3,
                "feature/unprefixed",
                json!({"full_name": "fixture/project"}),
                json!({"full_name": "fixture/project"}),
                "Same repository but not an Octomus branch.\n<!-- octomus:task:u -->",
                "open",
            ),
            false,
        ),
        (
            entry(
                4,
                "octomus/unmarked",
                json!({"full_name": "fixture/project"}),
                json!({"full_name": "fixture/project"}),
                "Prefixed branch without the task marker.",
                "open",
            ),
            false,
        ),
        (
            entry(
                5,
                "octomus/deleted-head",
                Value::Null,
                json!({"full_name": "fixture/project"}),
                "Source repository was deleted.\n<!-- octomus:task:gone -->",
                "open",
            ),
            false,
        ),
    ];
    let prs: Vec<Value> = cases.iter().map(|(p, _)| p.clone()).collect();
    let inventory = git::parse_inventory(&json!(prs).to_string(), &c).unwrap();
    for (index, (_, expected)) in cases.iter().enumerate() {
        assert_eq!(inventory.prs[index].owned, *expected, "case {}", index + 1);
    }
    let deleted = &inventory.prs[4];
    assert_eq!(deleted.head_repository, "");
    assert_eq!(deleted.branch, "octomus/deleted-head");
    let wrong_base = entry(
        6,
        "octomus/cross-repo",
        json!({"full_name": "fixture/project"}),
        json!({"full_name": "upstream/project"}),
        "Targets a different base repository.\n<!-- octomus:task:x -->",
        "open",
    );
    assert!(git::parse_inventory(&json!(vec![wrong_base]).to_string(), &c).is_err());
}

#[test]
fn malformed_or_conflicting_inventory_fails_closed() {
    let c = config();
    let mut conflicting = external_entry(9);
    conflicting["head"]["sha"] = json!("f".repeat(40));
    let out = format!(
        "{}{}",
        json!(vec![external_entry(9)]),
        json!(vec![conflicting])
    );
    assert!(git::parse_inventory(&out, &c).is_err());
    let marked = entry(
        30,
        "octomus/shared",
        json!({"full_name": "fixture/project"}),
        json!({"full_name": "fixture/project"}),
        "Marked.\n<!-- octomus:task:m -->",
        "open",
    );
    let mut unmarked = marked.clone();
    unmarked["body"] = json!("The marker is gone.");
    let out = format!("{}{}", json!(vec![marked]), json!(vec![unmarked]));
    assert!(git::parse_inventory(&out, &c).is_err());
    assert!(git::parse_inventory("", &c).is_err());
    assert!(git::parse_inventory("  \n", &c).is_err());
    let mut missing_state = external_entry(20);
    missing_state.as_object_mut().unwrap().remove("state");
    assert!(git::parse_inventory(&json!(vec![missing_state]).to_string(), &c).is_err());
    let unknown_state = entry(
        21,
        "contributor/work",
        json!({"full_name": "contributor/project"}),
        json!({"full_name": "fixture/project"}),
        "Unknown state.",
        "draft",
    );
    assert!(git::parse_inventory(&json!(vec![unknown_state]).to_string(), &c).is_err());
    for (field, value) in [
        ("head.ref", ""),
        ("head.sha", ""),
        ("base.ref", ""),
        ("html_url", ""),
    ] {
        let mut broken = external_entry(10);
        match field {
            "head.ref" => broken["head"]["ref"] = json!(value),
            "head.sha" => broken["head"]["sha"] = json!(value),
            "base.ref" => broken["base"]["ref"] = json!(value),
            _ => broken["html_url"] = json!(value),
        }
        assert!(
            git::parse_inventory(&json!(vec![broken]).to_string(), &c).is_err(),
            "{field}"
        );
    }
    let mut no_base_repo = external_entry(11);
    no_base_repo["base"]["repo"] = Value::Null;
    assert!(git::parse_inventory(&json!(vec![no_base_repo]).to_string(), &c).is_err());
    let missing_number = json!([{"title": "No identity", "state": "open"}]);
    assert!(git::parse_inventory(&missing_number.to_string(), &c).is_err());
    let closed = entry(
        12,
        "octomus/closed",
        json!({"full_name": "fixture/project"}),
        json!({"full_name": "fixture/project"}),
        "Done.\n<!-- octomus:task:closed -->",
        "closed",
    );
    let inventory = git::parse_inventory(
        &json!(vec![closed, owned_entry(13, "octomus/open")]).to_string(),
        &c,
    )
    .unwrap();
    assert_eq!(inventory.prs.len(), 1);
    assert_eq!(inventory.prs[0].number, 13);
}

#[test]
fn external_context_bounds_counts_and_truncates_utf8() {
    let c = config();
    let mut entries: Vec<Value> = (1..=130).map(external_entry).collect();
    entries.push(owned_entry(200, "octomus/mine"));
    let inventory = git::parse_inventory(&json!(entries).to_string(), &c).unwrap();
    let (external, coverage) = external_context(&inventory);
    assert_eq!(coverage.total_open, 131);
    assert_eq!(coverage.total_external, 130);
    assert_eq!(coverage.included_external, 100);
    assert_eq!(coverage.omitted_external, 30);
    assert!(coverage.complete);
    assert!(coverage.observed_at.is_some());
    assert_eq!(external.len(), 100);
    assert!(
        external
            .iter()
            .all(|p| !p.title_truncated && !p.body_truncated)
    );

    let mut titled = external_entry(300);
    titled["title"] = json!("héllo𐐀".repeat(60));
    let mut bodied = external_entry(301);
    bodied["body"] = json!("é".repeat(2500));
    let inventory = git::parse_inventory(&json!(vec![titled, bodied]).to_string(), &c).unwrap();
    let (external, coverage) = external_context(&inventory);
    assert_eq!(external.len(), 2);
    assert_eq!(external[0].title.chars().count(), 200);
    assert!(external[0].title_truncated);
    assert_eq!(external[1].body.chars().count(), 2000);
    assert!(external[1].body_truncated);
    assert_eq!(coverage.included_external, 2);
    assert_eq!(coverage.omitted_external, 0);

    let huge: Vec<Value> = (400..=500)
        .map(|n| {
            let mut p = external_entry(n);
            p["body"] = json!("𐐀".repeat(2000));
            p
        })
        .collect();
    let inventory = git::parse_inventory(&json!(huge).to_string(), &c).unwrap();
    let (external, coverage) = external_context(&inventory);
    assert!(coverage.included_external < coverage.total_external);
    assert_eq!(
        coverage.included_external + coverage.omitted_external,
        coverage.total_external
    );
    assert!(serde_json::to_string(&external).unwrap().len() <= coverage.max_context_bytes);
    assert_eq!(coverage.max_external, 100);
    assert_eq!(coverage.max_title_chars, 200);
    assert_eq!(coverage.max_body_chars, 2000);
}

#[test]
fn external_context_sorts_unsorted_inventories() {
    let external_pr = |number: u64| -> PullRequest {
        serde_json::from_value(json!({
            "number": number, "title": "External", "branch": format!("contributor/{number}"),
            "head": "d".repeat(40), "base": "main",
            "url": format!("https://example.invalid/pull/{number}"), "body": "External work.",
            "state": "open", "changed_lines": 1, "created_at": "2026-08-01T00:00:00Z",
            "owned": false, "head_repository": "contributor/project",
            "base_repository": "fixture/project"
        }))
        .unwrap()
    };
    let inventory = octomus_agent::model::OpenPrInventory {
        repository: "fixture/project".into(),
        observed_at: "2026-08-01T00:00:00Z".into(),
        prs: vec![external_pr(9), external_pr(3), external_pr(7)],
    };
    let (external, coverage) = external_context(&inventory);
    assert_eq!(
        external.iter().map(|p| p.number).collect::<Vec<_>>(),
        vec![3, 7, 9]
    );
    assert_eq!(coverage.included_external, 3);
}

#[test]
fn target_resolution_rejects_external_and_closed_prs() {
    let c = config();
    let pr = |state: &str, owned: bool, base_repo: &str| -> PullRequest {
        serde_json::from_value(json!({
            "number": 7, "title": "PR", "branch": "octomus/fix", "head": "a".repeat(40),
            "base": "main", "url": "https://example.invalid/pull/7", "body": "",
            "state": state, "changed_lines": 1, "created_at": "2026-08-01T00:00:00Z",
            "owned": owned, "head_repository": "fixture/project", "base_repository": base_repo
        }))
        .unwrap()
    };
    let open = vec![pr("open", true, "fixture/project")];
    assert_eq!(
        resolve_target(&c, &open, "octomus/fix")
            .unwrap()
            .unwrap()
            .number,
        7
    );
    for prs in [
        vec![pr("open", false, "fixture/project")],
        vec![pr("closed", true, "fixture/project")],
        vec![pr("merged", true, "fixture/project")],
        vec![pr("open", true, "upstream/project")],
        vec![{
            let mut p = pr("open", true, "fixture/project");
            p.base = "release".into();
            p
        }],
    ] {
        assert!(resolve_target(&c, &prs, "octomus/fix").is_err());
    }
    assert!(resolve_target(&c, &open, "main").unwrap().is_none());
}

#[test]
fn legacy_grounding_loads_with_empty_external_context() {
    let g: Grounding = serde_json::from_value(json!({
        "revision": "rev",
        "prs": [],
        "history": {},
        "maintenance_due": false,
        "maintenance_targets": []
    }))
    .unwrap();
    assert!(g.external_prs.is_empty());
    assert!(!g.pr_coverage.complete);
    assert!(g.pr_coverage.observed_at.is_none());
    assert_eq!(g.pr_coverage.total_external, 0);
}
