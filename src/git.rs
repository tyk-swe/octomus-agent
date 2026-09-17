use crate::{
    config::Config,
    model::{BlockedReason, OpenPrInventory, PullRequest, Task, now},
    process,
};
use anyhow::{Context, Result, ensure};
use serde_json::Value;
use std::{
    collections::{HashMap, hash_map::Entry},
    path::Path,
};
use tokio_util::sync::CancellationToken;
/// Attaches a typed blocked reason as the innermost cause while keeping the
/// detailed message outermost, so `BlockedReason::from_error` picks the reason.
fn blocked(reason: BlockedReason, message: &'static str) -> anyhow::Error {
    anyhow::Error::new(reason).context(message)
}

pub async fn git(
    c: &Config,
    cwd: &Path,
    args: &[&str],
    cancel: &CancellationToken,
) -> Result<String> {
    Ok(
        process::run_machine("git", args, cwd, c.command_timeout_seconds, cancel)
            .await?
            .trim()
            .to_owned(),
    )
}
async fn gh(c: &Config, args: &[&str], cancel: &CancellationToken) -> Result<String> {
    process::run_machine("gh", args, &c.repository, c.command_timeout_seconds, cancel).await
}
/// The origin URL of a clone of the configured repository.
async fn origin_url(c: &Config, repo: &Path, cancel: &CancellationToken) -> Result<String> {
    git(c, repo, &["remote", "get-url", "origin"], cancel).await
}
/// The commit a checkout currently has checked out.
async fn head(c: &Config, path: &Path, cancel: &CancellationToken) -> Result<String> {
    git(c, path, &["rev-parse", "HEAD"], cancel).await
}
/// Decodes a `gh api --paginate` response: one document per page, streamed rather
/// than buffered, so the caller can count pages and decide what an empty reply means.
fn gh_pages(out: &str) -> impl Iterator<Item = Result<Vec<Value>>> + '_ {
    serde_json::Deserializer::from_str(out)
        .into_iter::<Vec<Value>>()
        .map(|page| page.map_err(Into::into))
}
pub async fn validate_remote(c: &Config, cancel: &CancellationToken) -> Result<()> {
    let remote = origin_url(c, &c.repository, cancel).await?;
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
    let remote = origin_url(c, &c.repository, cancel).await?;
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
    head(c, path, cancel).await
}
async fn clean(c: &Config, path: &Path, cancel: &CancellationToken) -> Result<bool> {
    Ok(git(c, path, &["status", "--porcelain"], cancel)
        .await?
        .is_empty())
}
/// Whether `ancestor` is an ancestor of `descendant`. `merge-base --is-ancestor`
/// reports a false predicate as exit 1; every other nonzero status is a real
/// command failure and propagates instead of reading as false.
pub async fn is_ancestor(
    c: &Config,
    cwd: &Path,
    ancestor: &str,
    descendant: &str,
    cancel: &CancellationToken,
) -> Result<bool> {
    process::run_predicate(
        "git",
        &["merge-base", "--is-ancestor", ancestor, descendant],
        cwd,
        c.command_timeout_seconds,
        cancel,
        &[1],
    )
    .await
}
/// True when the worktree is clean and HEAD is exactly `revision`.
pub async fn at(
    c: &Config,
    path: &Path,
    revision: &str,
    cancel: &CancellationToken,
) -> Result<bool> {
    Ok(clean(c, path, cancel).await? && head(c, path, cancel).await? == revision)
}
pub async fn open_pr_inventory(c: &Config, cancel: &CancellationToken) -> Result<OpenPrInventory> {
    // Paginate the API: never quietly omit older open work.
    let observed_at = now();
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
    let mut inventory = parse_inventory(&out, c)?;
    inventory.observed_at = observed_at;
    Ok(inventory)
}
pub fn parse_inventory(out: &str, c: &Config) -> Result<OpenPrInventory> {
    let mut prs: HashMap<u64, PullRequest> = HashMap::new();
    let mut pages = 0usize;
    for page in gh_pages(out) {
        pages += 1;
        for p in page? {
            ensure!(
                matches!(p["state"].as_str(), Some("open" | "closed")),
                "Open PR entry has an unrecognized state"
            );
            let pr = parse_pr(&p, c)?;
            ensure!(
                !pr.branch.is_empty()
                    && !pr.head.is_empty()
                    && !pr.base.is_empty()
                    && !pr.base_repository.is_empty()
                    && !pr.url.is_empty(),
                "Open PR entry is missing required identity"
            );
            ensure!(
                pr.base_repository.eq_ignore_ascii_case(&c.github_repo),
                "Open PR entry reports a different base repository"
            );
            if pr.state != "open" {
                continue;
            }
            match prs.entry(pr.number) {
                Entry::Occupied(existing) => {
                    ensure!(
                        existing.get() == &pr,
                        "Conflicting open PR inventory entries"
                    );
                }
                Entry::Vacant(entry) => {
                    entry.insert(pr);
                }
            }
        }
    }
    ensure!(pages >= 1, "Open PR inventory response is empty");
    let mut prs: Vec<PullRequest> = prs.into_values().collect();
    prs.sort_by_key(|p| p.number);
    Ok(OpenPrInventory {
        repository: c.github_repo.clone(),
        observed_at: now(),
        prs,
    })
}
pub async fn owned_pr_details(
    c: &Config,
    inventory: &OpenPrInventory,
    cancel: &CancellationToken,
) -> Result<Vec<PullRequest>> {
    let mut prs = vec![];
    for observed in inventory.prs.iter().filter(|p| p.owned) {
        let detail = pr(c, observed.number, cancel).await?;
        ensure!(
            detail.owned_open() && detail.branch == observed.branch && detail.base == observed.base,
            "Owned PR changed while the open inventory was being read"
        );
        prs.push(detail);
    }
    Ok(prs)
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
/// Bodies of a pull request's comments, paginated like every other list read.
pub(crate) async fn pr_comments(
    c: &Config,
    number: u64,
    cancel: &CancellationToken,
) -> Result<Vec<String>> {
    let out = gh(
        c,
        &[
            "api",
            "--paginate",
            &format!(
                "repos/{}/issues/{number}/comments?per_page=100",
                c.github_repo
            ),
        ],
        cancel,
    )
    .await?;
    let mut bodies = vec![];
    for page in gh_pages(&out) {
        for value in page? {
            bodies.push(value["body"].as_str().unwrap_or("").to_owned());
        }
    }
    Ok(bodies)
}
/// Whether the task's publication marker is attached to the pull request: in the
/// description when this task originated the request, or in an append-only
/// comment when it delivered a follow-up.
pub(crate) async fn task_marker(
    c: &Config,
    task_id: &str,
    p: &PullRequest,
    cancel: &CancellationToken,
) -> Result<bool> {
    let marker = format!("<!-- octomus:task:{task_id} -->");
    if p.body.contains(&marker) {
        return Ok(true);
    }
    Ok(pr_comments(c, p.number, cancel)
        .await?
        .iter()
        .any(|body| body.contains(&marker)))
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
        head_repository: text(&p["head"]["repo"]["full_name"]),
        base_repository: text(&p["base"]["repo"]["full_name"]),
        owned: branch.starts_with(&c.branch_prefix)
            && p["head"]["repo"]["full_name"]
                .as_str()
                .is_some_and(|r| r.eq_ignore_ascii_case(&c.github_repo))
            && p["base"]["repo"]["full_name"]
                .as_str()
                .is_some_and(|r| r.eq_ignore_ascii_case(&c.github_repo))
            && body.contains("<!-- octomus:task:"),
    })
}
pub(crate) async fn publication_pr(
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
    let mut matches = vec![];
    for page in gh_pages(&output) {
        for value in page? {
            let candidate = parse_pr(&value, c)?;
            if candidate.branch == branch {
                matches.push(candidate);
            }
        }
    }
    ensure!(
        matches.len() <= 1,
        blocked(
            BlockedReason::RemoteConflict,
            "Ambiguous PR association; reconcile before publication"
        )
    );
    Ok(matches.pop())
}

/// `marker` carries the caller's check of `task_marker`: the marker may live in
/// the description or in a follow-up comment, which this synchronous check
/// cannot fetch for itself.
pub fn validate_publication(
    task: &Task,
    p: &PullRequest,
    marker: bool,
    reconcile: bool,
) -> Result<()> {
    let c = &task.config;
    ensure!(
        p.head_repository.eq_ignore_ascii_case(&c.github_repo)
            && p.base_repository.eq_ignore_ascii_case(&c.github_repo)
            && p.owned
            && p.branch == task.branch
            && p.base == c.default_branch
            && Some(p.head.as_str()) == task.output_commit.as_deref()
            && marker
            && (p.state == "open"
                || (reconcile && ["closed", "merged"].contains(&p.state.as_str()))),
        "PR publication result does not match repository, ownership, branch, base, reviewed head, task marker or state"
    );
    Ok(())
}

pub async fn publish(task: &Task, cancel: &CancellationToken) -> Result<PullRequest> {
    publish_inner(task, cancel)
        .await
        .context(BlockedReason::PublicationUncertain)
}
/// The pull request text for a reviewed commit.
///
/// A task that already owns a pull request posts a follow-up comment rather
/// than rewriting the description, so earlier delivery notes and any maintainer
/// conversation are never replaced. The task marker makes that append
/// idempotent: a republication of the same task adds nothing.
fn pr_body(task: &Task, existing: Option<&PullRequest>, commit: &str) -> String {
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
    let marker = format!("<!-- octomus:task:{} -->", task.id);
    let update = format!(
        "{}\n\n{}\n\nScope: {}\n\nVerification\n{}\n\n{}\n\nReviewed commit: `{commit}`. {} review round(s).\n\n{marker}",
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
        task.reviews.len()
    );
    match existing {
        Some(_) => format!("Octomus follow-up: {}\n\n{update}", task.proposal.title),
        None => update,
    }
}
/// Attaches follow-up evidence to a pull request this task already owns.
///
/// The remote is re-read immediately before the comment: another writer moving
/// the head or closing the request between reconciliation and here means the
/// delivery no longer matches what was reviewed, so it is refused and the retry
/// reconciles against whatever is now there. The evidence itself is posted as a
/// comment rather than rewritten into the description — the body is shared,
/// maintainer-editable text with no conditional-replace API, so a check-then-
/// edit could silently drop a concurrent maintainer edit while a comment can
/// only ever append.
async fn update_pr(
    c: &Config,
    task: &Task,
    p: &PullRequest,
    commit: &str,
    body_path: &str,
    cancel: &CancellationToken,
) -> Result<PullRequest> {
    let latest = pr(c, p.number, cancel).await?;
    ensure!(
        latest.owned_open() && latest.base == c.default_branch && latest.head == commit,
        blocked(
            BlockedReason::RemoteConflict,
            "PR changed around publication; retry will reconcile the current remote state"
        )
    );
    let marker = task_marker(c, &task.id, &latest, cancel).await?;
    if !marker {
        gh(
            c,
            &[
                "pr",
                "comment",
                &p.number.to_string(),
                "--repo",
                &c.github_repo,
                "--body-file",
                body_path,
            ],
            cancel,
        )
        .await?;
    }
    let published = pr(c, p.number, cancel).await?;
    // The marker is attached by construction: it was either already present or
    // posted by the comment above.
    validate_publication(task, &published, true, false)?;
    Ok(published)
}

/// Opens a new pull request and confirms what was actually created.
///
/// `gh` reports success as a URL, which is parsed rather than trusted: a URL on
/// another host, or naming another repository, means the request was not created
/// where this task believes it was.
async fn create_pr(
    c: &Config,
    task: &Task,
    body_path: &str,
    cancel: &CancellationToken,
) -> Result<PullRequest> {
    let created = gh(
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
            body_path,
        ],
        cancel,
    )
    .await?;
    let url = reqwest::Url::parse(created.trim())
        .context(BlockedReason::RemoteConflict)
        .context("PR creation returned no unambiguous URL; reconcile before retrying")?;
    ensure!(
        url.scheme() == "https"
            && url.host_str() == Some("github.com")
            && url.query().is_none()
            && url.fragment().is_none(),
        blocked(BlockedReason::RemoteConflict, "Invalid PR creation URL")
    );
    let parts: Vec<_> = url
        .path_segments()
        .ok_or_else(|| blocked(BlockedReason::RemoteConflict, "Missing PR URL path"))?
        .collect();
    ensure!(
        parts.len() == 4
            && parts[2] == "pull"
            && format!("{}/{}", parts[0], parts[1]).eq_ignore_ascii_case(&c.github_repo),
        blocked(
            BlockedReason::RemoteConflict,
            "Created PR belongs to a different repository"
        )
    );
    let number: u64 = parts[3]
        .parse()
        .context(BlockedReason::RemoteConflict)
        .context("Missing created PR number")?;
    let published = pr(c, number, cancel).await?;
    let marker = format!("<!-- octomus:task:{} -->", task.id);
    validate_publication(task, &published, published.body.contains(&marker), false)?;
    Ok(published)
}

async fn publish_inner(task: &Task, cancel: &CancellationToken) -> Result<PullRequest> {
    let config = task.execution_config();
    let c = &config;
    validate_remote(c, cancel).await?;
    let trusted_remote = origin_url(c, &c.repository, cancel).await?;
    let path = Path::new(&task.workspace);
    let commit = task
        .output_commit
        .as_deref()
        .ok_or_else(|| blocked(BlockedReason::WorkspaceInvalid, "No reviewed commit"))?;
    ensure!(
        task.reviews
            .last()
            .is_some_and(|r| r.revision == commit && r.result.clean()),
        blocked(
            BlockedReason::WorkspaceInvalid,
            "Publication requires a clean review at the output revision"
        )
    );
    ensure!(
        c.verification_commands.iter().all(|cmd| task
            .verification
            .iter()
            .rev()
            .find(|v| v.command == *cmd)
            .is_some_and(|v| v.success && v.revision == commit)),
        blocked(
            BlockedReason::WorkspaceInvalid,
            "Publication requires successful verification at the reviewed revision"
        )
    );
    ensure!(
        task.branch.starts_with(&c.branch_prefix) && task.branch != c.default_branch,
        blocked(
            BlockedReason::WorkspaceInvalid,
            "Cannot publish outside the owned branch namespace"
        )
    );
    ensure!(
        clean(c, path, cancel).await?,
        blocked(
            BlockedReason::WorkspaceInvalid,
            "Workspace changed after review"
        )
    );
    ensure!(
        head(c, path, cancel).await? == commit,
        blocked(
            BlockedReason::WorkspaceInvalid,
            "Workspace HEAD changed after review"
        )
    );
    let existing = if let Some(number) = task.pr_number {
        Some(pr(c, number, cancel).await?)
    } else {
        publication_pr(c, &task.branch, cancel).await?
    };
    if let Some(p) = &existing {
        let marker = task_marker(c, &task.id, p, cancel).await?;
        if validate_publication(task, p, marker, true).is_ok() {
            // Delivery already happened, even if a maintainer has since closed or merged the PR.
            return Ok(p.clone());
        }
        ensure!(
            p.owned_open() && p.branch == task.branch && p.base == c.default_branch,
            blocked(
                BlockedReason::RemoteConflict,
                "PR ownership, base, or open state changed; reconcile before retrying"
            )
        );
        if task.pr_number.is_none() {
            ensure!(
                marker,
                blocked(
                    BlockedReason::RemoteConflict,
                    "Branch is already associated with another task"
                )
            );
        }
    }
    let remote = remote_revision(c, &task.branch, cancel).await?;
    ensure!(
        remote_revision(c, &c.default_branch, cancel)
            .await?
            .as_deref()
            == Some(&task.default_revision),
        BlockedReason::StaleBase
    );
    if remote.as_deref() != Some(commit) {
        if task.pr_number.is_some() {
            ensure!(
                remote.as_deref() == Some(&task.source_revision),
                BlockedReason::RemoteConflict
            );
        } else {
            ensure!(remote.is_none(), BlockedReason::RemoteConflict);
        }
        // An exact lease protects the check/push race. The local ancestry must also be preserved.
        ensure!(
            is_ancestor(c, path, &task.source_revision, commit, cancel).await?,
            blocked(
                BlockedReason::RemoteConflict,
                "Reviewed output does not contain the recorded source; reconcile the branch"
            )
        );
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
    let body = pr_body(task, existing.as_ref(), commit);
    let body_file = path.parent().unwrap().join("pr-body.md");
    tokio::fs::write(&body_file, body).await?;
    let body_path = body_file.to_str().context("Non UTF-8 PR body path")?;
    if let Some(p) = existing {
        return update_pr(c, task, &p, commit, body_path, cancel).await;
    }
    create_pr(c, task, body_path, cancel).await
}
