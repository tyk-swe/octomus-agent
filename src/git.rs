use crate::{
    config::Config,
    model::{PrComment, PullRequest, Task},
    process,
    store::redact,
};
use anyhow::{Context, Result, ensure};
use serde_json::Value;
use std::{collections::BTreeMap, path::Path};
use tokio_util::sync::CancellationToken;

/// Recorded PR comments are bounded evidence: newest first, per-comment and per-PR caps.
pub const MAX_COMMENTS: usize = 30;
pub const MAX_COMMENT_CHARS: usize = 2000;
pub const MAX_COMMENT_BYTES_PER_PR: usize = 24 * 1024;

pub async fn git(
    c: &Config,
    cwd: &Path,
    args: &[&str],
    cancel: &CancellationToken,
) -> Result<String> {
    process::run("git", args, cwd, c.command_timeout_seconds, cancel).await
}
async fn gh(c: &Config, args: &[&str], cancel: &CancellationToken) -> Result<String> {
    process::run("gh", args, &c.repository, c.command_timeout_seconds, cancel).await
}
pub async fn validate_remote(c: &Config, cancel: &CancellationToken) -> Result<()> {
    let remote = git(c, &c.repository, &["remote", "get-url", "origin"], cancel).await?;
    let repo = remote
        .strip_prefix("git@github.com:")
        .or_else(|| remote.strip_prefix("https://github.com/"))
        .or_else(|| remote.strip_prefix("ssh://git@github.com/"))
        .context("Origin must use github.com via SSH or credential-free HTTPS")?;
    ensure!(
        repo.trim_end_matches(".git")
            .eq_ignore_ascii_case(&c.github_repo),
        "Origin does not match configured GitHub repository"
    );
    gh(c, &["auth", "status", "--hostname", "github.com"], cancel).await?;
    Ok(())
}
pub async fn fetch(c: &Config, cancel: &CancellationToken) -> Result<()> {
    git(c, &c.repository, &["fetch", "--prune", "origin"], cancel).await?;
    Ok(())
}
pub async fn remote_revision(
    c: &Config,
    branch: &str,
    cancel: &CancellationToken,
) -> Result<Option<String>> {
    ensure!(crate::config::valid_branch(branch), "Invalid branch");
    let out = git(
        c,
        &c.repository,
        &[
            "ls-remote",
            "--heads",
            "origin",
            &format!("refs/heads/{branch}"),
        ],
        cancel,
    )
    .await?;
    Ok(out.split_whitespace().next().map(String::from))
}
pub async fn clone_at(
    c: &Config,
    path: &Path,
    revision: &str,
    cancel: &CancellationToken,
) -> Result<()> {
    ensure!(
        !path.exists(),
        "Workspace already exists; recovery must inspect it"
    );
    tokio::fs::create_dir_all(path.parent().context("Invalid workspace path")?).await?;
    git(
        c,
        &c.repository,
        &[
            "clone",
            "--no-hardlinks",
            "--no-checkout",
            "--",
            c.repository.to_str().context("Non UTF-8 repository path")?,
            path.to_str().context("Non UTF-8 workspace")?,
        ],
        cancel,
    )
    .await?;
    git(c, path, &["checkout", "--detach", revision], cancel).await?;
    let remote = git(c, &c.repository, &["remote", "get-url", "origin"], cancel).await?;
    git(c, path, &["remote", "set-url", "origin", &remote], cancel).await?;
    git(c, path, &["config", "user.name", "Octomus Agent"], cancel).await?;
    git(
        c,
        path,
        &[
            "config",
            "user.email",
            "octomus-agent@users.noreply.github.com",
        ],
        cancel,
    )
    .await?;
    // Task clones must never include application state in generated commits.
    let exclude = path.join(".git/info/exclude");
    tokio::fs::write(exclude, "/.octomus/\n").await?;
    Ok(())
}
pub async fn snapshot(
    c: &Config,
    path: &Path,
    message: &str,
    cancel: &CancellationToken,
) -> Result<String> {
    git(c, path, &["add", "--all"], cancel).await?;
    let changed = git(c, path, &["diff", "--cached", "--name-only"], cancel).await?;
    if !changed.is_empty() {
        git(
            c,
            path,
            &["-c", "core.hooksPath=/dev/null", "commit", "-m", message],
            cancel,
        )
        .await?;
    }
    git(c, path, &["rev-parse", "HEAD"], cancel).await
}
/// Rebases the detached workspace HEAD onto a moved default-branch revision. A conflict
/// aborts the rebase, restores the previous HEAD and fails so the workspace stays inspectable.
pub async fn rebase_onto(
    c: &Config,
    workspace: &Path,
    revision: &str,
    cancel: &CancellationToken,
) -> Result<String> {
    ensure!(
        clean(c, workspace, cancel).await?,
        "Workspace must be clean before reconciliation"
    );
    let before = git(c, workspace, &["rev-parse", "HEAD"], cancel).await?;
    // The trusted configured checkout already fetched the remote; take the revision from it.
    git(
        c,
        workspace,
        &[
            "fetch",
            "--no-tags",
            c.repository.to_str().context("Non UTF-8 repository path")?,
            &format!("refs/remotes/origin/{}", c.default_branch),
        ],
        cancel,
    )
    .await?;
    ensure!(
        git(c, workspace, &["rev-parse", "FETCH_HEAD"], cancel).await? == revision,
        "Default branch moved again during reconciliation; retry"
    );
    let rebase = git(
        c,
        workspace,
        &[
            "-c",
            "core.hooksPath=/dev/null",
            "-c",
            "rebase.autoStash=false",
            "rebase",
            revision,
        ],
        cancel,
    )
    .await;
    if let Err(error) = rebase {
        let _ = git(c, workspace, &["rebase", "--abort"], cancel).await;
        ensure!(
            git(c, workspace, &["rev-parse", "HEAD"], cancel).await? == before
                && clean(c, workspace, cancel).await?,
            "Rebase abort left the workspace inconsistent; inspect before retrying"
        );
        anyhow::bail!(
            "Rebase onto the moved default branch {revision} conflicted; workspace preserved at {before}: {error:#}"
        );
    }
    ensure!(
        clean(c, workspace, cancel).await?,
        "Rebase left uncommitted changes"
    );
    git(
        c,
        workspace,
        &["merge-base", "--is-ancestor", revision, "HEAD"],
        cancel,
    )
    .await?;
    git(c, workspace, &["rev-parse", "HEAD"], cancel).await
}
pub async fn clean(c: &Config, path: &Path, cancel: &CancellationToken) -> Result<bool> {
    Ok(git(c, path, &["status", "--porcelain"], cancel)
        .await?
        .is_empty())
}
pub async fn prs(c: &Config, cancel: &CancellationToken) -> Result<Vec<PullRequest>> {
    // Paginate the API: never quietly omit older open work.
    let out = gh(
        c,
        &[
            "api",
            "--paginate",
            &format!("repos/{}/pulls?state=open&per_page=100", c.github_repo),
        ],
        cancel,
    )
    .await?;
    let mut prs = vec![];
    for page in serde_json::Deserializer::from_str(&out).into_iter::<Vec<Value>>() {
        for p in page? {
            if p["head"]["ref"]
                .as_str()
                .is_some_and(|b| b.starts_with(&c.branch_prefix))
            {
                let detail = gh(
                    c,
                    &[
                        "api",
                        &format!("repos/{}/pulls/{}", c.github_repo, p["number"]),
                    ],
                    cancel,
                )
                .await?;
                let mut pr = parse_pr(&serde_json::from_str(&detail)?, c)?;
                if pr.owned {
                    feedback(c, &mut pr, cancel).await?;
                }
                prs.push(pr);
            }
        }
    }
    Ok(prs)
}
async fn paginated(c: &Config, route: &str, cancel: &CancellationToken) -> Result<Vec<Value>> {
    let out = gh(c, &["api", "--paginate", route], cancel).await?;
    let mut items = vec![];
    for page in serde_json::Deserializer::from_str(&out).into_iter::<Value>() {
        match page? {
            Value::Array(values) => items.extend(values),
            // check-runs pages are objects wrapping the array.
            Value::Object(mut object) => {
                if let Some(Value::Array(values)) = object.remove("check_runs") {
                    items.extend(values);
                }
            }
            _ => {}
        }
    }
    Ok(items)
}
/// Records reviews, check runs and comments for an owned PR so planning can act on them.
async fn feedback(c: &Config, pr: &mut PullRequest, cancel: &CancellationToken) -> Result<()> {
    let repo = &c.github_repo;
    let n = pr.number;
    let reviews = paginated(c, &format!("repos/{repo}/pulls/{n}/reviews"), cancel).await?;
    pr.review_decision = review_decision(&reviews);
    let runs = paginated(
        c,
        &format!("repos/{repo}/commits/{}/check-runs", pr.head),
        cancel,
    )
    .await?;
    (pr.ci, pr.failing_checks) = ci_status(&runs);
    let review_comments = paginated(c, &format!("repos/{repo}/pulls/{n}/comments"), cancel).await?;
    let issue_comments = paginated(c, &format!("repos/{repo}/issues/{n}/comments"), cancel).await?;
    pr.comments = comments(&review_comments, &issue_comments);
    Ok(())
}
/// Latest non-pending, non-dismissed state per reviewer; requested changes outrank approval.
pub fn review_decision(reviews: &[Value]) -> String {
    let mut latest: BTreeMap<&str, &str> = BTreeMap::new();
    for review in reviews {
        let (Some(login), Some(state)) =
            (review["user"]["login"].as_str(), review["state"].as_str())
        else {
            continue;
        };
        if !matches!(state, "PENDING" | "DISMISSED") {
            latest.insert(login, state);
        }
    }
    for (state, decision) in [
        ("CHANGES_REQUESTED", "changes_requested"),
        ("APPROVED", "approved"),
        ("COMMENTED", "commented"),
    ] {
        if latest.values().any(|s| *s == state) {
            return decision.into();
        }
    }
    "none".into()
}
/// Aggregates check runs on the head commit; any incomplete run is pending, any failed run fails.
pub fn ci_status(check_runs: &[Value]) -> (String, Vec<String>) {
    if check_runs.is_empty() {
        return ("none".into(), vec![]);
    }
    let failing: Vec<String> = check_runs
        .iter()
        .filter(|run| {
            run["status"] == "completed"
                && run["conclusion"].as_str().is_some_and(|c| {
                    matches!(
                        c,
                        "failure"
                            | "timed_out"
                            | "cancelled"
                            | "action_required"
                            | "startup_failure"
                    )
                })
        })
        .map(|run| run["name"].as_str().unwrap_or("unnamed check").to_owned())
        .collect();
    if !failing.is_empty() {
        return ("failure".into(), failing);
    }
    if check_runs.iter().any(|run| run["status"] != "completed") {
        return ("pending".into(), vec![]);
    }
    ("success".into(), vec![])
}
/// Merges review and issue comments, newest last, bounded and redacted.
pub fn comments(review: &[Value], issue: &[Value]) -> Vec<PrComment> {
    let mut all: Vec<PrComment> = review
        .iter()
        .chain(issue)
        .map(|c| PrComment {
            author: c["user"]["login"].as_str().unwrap_or("").to_owned(),
            at: c["created_at"].as_str().unwrap_or("").to_owned(),
            path: c["path"].as_str().map(str::to_owned),
            body: redact(c["body"].as_str().unwrap_or(""))
                .chars()
                .take(MAX_COMMENT_CHARS)
                .collect(),
        })
        .collect();
    all.sort_by(|a, b| a.at.cmp(&b.at));
    let mut kept: Vec<PrComment> = vec![];
    let mut bytes = 0;
    for comment in all.into_iter().rev().take(MAX_COMMENTS) {
        bytes += comment.body.len();
        if bytes > MAX_COMMENT_BYTES_PER_PR {
            break;
        }
        kept.push(comment);
    }
    kept.reverse();
    kept
}
pub async fn pr(c: &Config, number: u64, cancel: &CancellationToken) -> Result<PullRequest> {
    let out = gh(
        c,
        &["api", &format!("repos/{}/pulls/{number}", c.github_repo)],
        cancel,
    )
    .await?;
    parse_pr(&serde_json::from_str(&out)?, c)
}
fn parse_pr(p: &Value, c: &Config) -> Result<PullRequest> {
    let text = |v: &Value| v.as_str().unwrap_or("").to_owned();
    let branch = text(&p["head"]["ref"]);
    let body = text(&p["body"]);
    Ok(PullRequest {
        number: p["number"].as_u64().context("Missing PR number")?,
        title: text(&p["title"]),
        branch: branch.clone(),
        head: text(&p["head"]["sha"]),
        base: text(&p["base"]["ref"]),
        url: text(&p["html_url"]),
        body: body.clone(),
        state: if p["merged_at"].is_string() {
            "merged".into()
        } else {
            text(&p["state"])
        },
        changed_lines: p["additions"].as_u64().unwrap_or(0) + p["deletions"].as_u64().unwrap_or(0),
        created_at: text(&p["created_at"]),
        owned: branch.starts_with(&c.branch_prefix)
            && p["head"]["repo"]["full_name"]
                .as_str()
                .is_some_and(|r| r.eq_ignore_ascii_case(&c.github_repo))
            && body.contains("<!-- octomus:task:"),
        mergeable: match (&p["mergeable"], p["mergeable_state"].as_str()) {
            (Value::Bool(false), _) | (_, Some("dirty")) => "conflicts",
            (Value::Bool(true), _) => "clean",
            _ => "unknown",
        }
        .into(),
        ..PullRequest::default()
    })
}
async fn publication_pr(
    c: &Config,
    branch: &str,
    cancel: &CancellationToken,
) -> Result<Option<PullRequest>> {
    let owner = c
        .github_repo
        .split('/')
        .next()
        .context("Missing repository owner")?;
    let output = gh(
        c,
        &[
            "api",
            "--paginate",
            &format!(
                "repos/{}/pulls?state=all&head={owner}:{branch}&per_page=100",
                c.github_repo
            ),
        ],
        cancel,
    )
    .await?;
    for page in serde_json::Deserializer::from_str(&output).into_iter::<Vec<Value>>() {
        for value in page? {
            let candidate = parse_pr(&value, c)?;
            if candidate.branch == branch {
                return Ok(Some(candidate));
            }
        }
    }
    Ok(None)
}
pub async fn publish(task: &Task, cancel: &CancellationToken) -> Result<PullRequest> {
    let c = &task.config;
    validate_remote(c, cancel).await?;
    let trusted_remote = git(c, &c.repository, &["remote", "get-url", "origin"], cancel).await?;
    let path = Path::new(&task.workspace);
    let commit = task
        .output_commit
        .as_deref()
        .context("No reviewed commit")?;
    ensure!(
        task.reviews
            .last()
            .is_some_and(|r| r.revision == commit && r.result.clean()),
        "Publication requires a clean review at the output revision"
    );
    ensure!(
        c.verification_commands.iter().all(|cmd| task
            .verification
            .iter()
            .rev()
            .find(|v| v.command == *cmd)
            .is_some_and(|v| v.success && v.revision == commit)),
        "Publication requires successful verification at the reviewed revision"
    );
    ensure!(
        task.branch.starts_with(&c.branch_prefix) && task.branch != c.default_branch,
        "Cannot publish outside the owned branch namespace"
    );
    ensure!(
        clean(c, path, cancel).await?,
        "Workspace changed after review"
    );
    ensure!(
        git(c, path, &["rev-parse", "HEAD"], cancel).await? == commit,
        "Workspace HEAD changed after review"
    );
    let existing = if let Some(number) = task.pr_number {
        Some(pr(c, number, cancel).await?)
    } else {
        publication_pr(c, &task.branch, cancel).await?
    };
    if let Some(p) = &existing {
        if p.owned
            && p.head == commit
            && p.body
                .contains(&format!("<!-- octomus:task:{} -->", task.id))
        {
            // Delivery already happened, even if a maintainer has since closed or merged the PR.
            return Ok(p.clone());
        }
        ensure!(
            p.owned && p.state == "open" && p.branch == task.branch && p.base == c.default_branch,
            "PR ownership, base, or open state changed; reconcile before retrying"
        );
        if task.pr_number.is_none() {
            ensure!(
                p.body
                    .contains(&format!("<!-- octomus:task:{} -->", task.id)),
                "Branch is already associated with another task"
            );
        }
    }
    let remote = remote_revision(c, &task.branch, cancel).await?;
    ensure!(
        remote_revision(c, &c.default_branch, cancel)
            .await?
            .as_deref()
            == Some(&task.default_revision),
        "Default branch moved since the recorded review context; inspect and reconcile before publication"
    );
    if remote.as_deref() != Some(commit) {
        if task.pr_number.is_some() {
            ensure!(
                remote.as_deref() == Some(&task.source_revision),
                "Remote branch changed during task; preserve workspace and reconcile"
            );
        } else {
            ensure!(
                remote.is_none(),
                "New branch collision; refusing to overwrite remote work"
            );
        }
        // An exact lease protects the check/push race. The local ancestry must also be preserved.
        git(
            c,
            path,
            &["merge-base", "--is-ancestor", &task.source_revision, commit],
            cancel,
        )
        .await?;
        let expected = remote.as_deref().unwrap_or("");
        git(
            c,
            path,
            &[
                "-c",
                "core.hooksPath=/dev/null",
                "-c",
                "push.followTags=false",
                "push",
                &format!("--force-with-lease=refs/heads/{}:{expected}", task.branch),
                &trusted_remote,
                &format!("{commit}:refs/heads/{}", task.branch),
            ],
            cancel,
        )
        .await?;
    }
    let verification = task
        .verification
        .iter()
        .filter(|v| v.revision == commit)
        .map(|v| {
            format!(
                "- `{}`: {}",
                v.command,
                if v.success { "passed" } else { "failed" }
            )
        })
        .collect::<Vec<_>>()
        .join("\n");
    let update = format!(
        "{}\n\n{}\n\nScope: {}\n\nVerification\n{}\n\n{}\n\nReviewed commit: `{commit}`. {} review round(s).\n\n<!-- octomus:task:{} -->",
        task.proposal.problem,
        task.proposal.benefit,
        task.proposal.scope,
        verification,
        task.sessions
            .iter()
            .rev()
            .find(|s| s.role == "executor" || s.role == "repair")
            .map(|s| s.summary.as_str())
            .unwrap_or(""),
        task.reviews.len(),
        task.id
    );
    let body = if let Some(p) = &existing {
        if p.body
            .contains(&format!("<!-- octomus:task:{} -->", task.id))
        {
            p.body.clone()
        } else {
            format!(
                "{}\n\n---\n\nOctomus follow-up: {}\n\n{update}",
                p.body, task.proposal.title
            )
        }
    } else {
        update
    };
    let body_path = path.parent().unwrap().join("pr-body.md");
    tokio::fs::write(&body_path, body).await?;
    if let Some(p) = existing {
        let latest = pr(c, p.number, cancel).await?;
        ensure!(
            latest.owned
                && latest.state == "open"
                && latest.base == c.default_branch
                && latest.head == commit
                && latest.body == p.body,
            "PR changed around publication; retry will reconcile the current remote state"
        );
        gh(
            c,
            &[
                "pr",
                "edit",
                &p.number.to_string(),
                "--repo",
                &c.github_repo,
                "--body-file",
                body_path.to_str().unwrap(),
            ],
            cancel,
        )
        .await?;
        let published = pr(c, p.number, cancel).await?;
        ensure!(
            published.state == "open" && published.head == commit,
            "PR changed while its publication record was being updated"
        );
        return Ok(published);
    }
    gh(
        c,
        &[
            "pr",
            "create",
            "--repo",
            &c.github_repo,
            "--head",
            &task.branch,
            "--base",
            &c.default_branch,
            "--title",
            &task.proposal.title,
            "--body-file",
            body_path.to_str().unwrap(),
        ],
        cancel,
    )
    .await?;
    prs(c, cancel)
        .await?
        .into_iter()
        .find(|p| p.branch == task.branch)
        .context("PR creation returned but PR is not visible; retry will reconcile")
}
