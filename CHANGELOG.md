# Changelog

Notable changes are recorded here using [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Per-route Codex/OpenCode selection, locally managed OpenCode servers, and guided
  provider/model/effort/variant configuration with compatible saved task loading.
- One-shot audits with durable proposal decisions and no task dispatch.
- Embedded dashboard, Linux x86_64/aarch64 release packaging, checksum-verifying
  installer and Cargo package metadata/assets.
- Security policy, threat model, hardened systemd unit, failed-authentication
  backoff and warnings for non-loopback listeners.
- Dependency audits, distribution/audit coverage and community contribution files.
- Configurable repair routes, CLI version diagnostics, durable admission accounting
  and read-only usage reports for live commissioning preparation.
- Indexed history summaries, authoritative counts, paginated evidence, decision
  memory, idle backoff and PR outcome observation.
- Pinned real-client contract coverage with isolated synthetic providers, and
  operational, browser and scale regressions.
- Read-only run evidence (`RunEvidenceV1`) for one cycle through
  `GET /api/cycles/{id}/evidence`, `--export-run` and the dashboard's Inspect run panel,
  with a documented consistent SQLite snapshot procedure exercised by
  `tests/evidence_snapshot.py`.
- A standalone public showcase build (`web/showcase`) of an owner-approved evidence
  wrapper with explicit fixture/recorded modes, allowlist and duplicate-key gates,
  hash-bound approval and network isolation.

### Changed

- Preserve configuration drafts and model catalogs across dashboard navigation,
  show unsaved changes with local discard, and require saved values for connection checks.
- Add a compact setup checklist to Configuration that labels repository details,
  routes, verification policy, the connection check and the Audit/Run once choice as
  entered, saved, checked or ran, links to the existing controls, invalidates a check
  result when the saved configuration changes, and never starts work. Tighten the
  README first-run path and link the public showcase's installation call to action to it.
- Return the checked configuration snapshot from the connection check and bind
  dashboard check results to it; keep failed task-evidence requests unavailable until
  an explicit retry or a changed saved revision, and label overview counts as
  recent-window observations.
- Improve dashboard typography, mobile touch targets and keyboard navigation; add
  distinct list loading, empty and retry states with retained results after refresh failures.
- Share dashboard action eligibility and pending feedback; distinguish delivered tasks
  from merged PRs and explain control conflicts for the requested operation.
- Lead installation guidance with the available source build, document both runners
  for contributors, and refresh the synthetic dashboard screenshot.
- Check worktree and HEAD before and after every verification command; a command that
  changes tracked state is recorded as failed evidence and blocks the task.
- Budget repair rounds per attempt: explicit retry starts a fresh round budget while
  retaining earlier review evidence.
- Deduplicate same-cycle accepted proposals by stable problem identity as well as title.
- Resolve PR targets once through a single owned-PR resolver shared by validation, task
  construction and decision memory; reject ambiguous matches.
- Record a terminal status for every started planning role, marking roles complete only
  after schema and source-integrity validation, and attach all of them to the cycle
  before a partial batch failure ends planning.
- Keep bounded stderr in successful verification evidence.
- Enforce live admission policy for queued and recovered tasks; preserve immutable
  contracts and explicit attempt limits.
- Validate exact PR publication results, complete same-branch dependency orders, and
  bounded machine output.
- Persist one-shot operation, typed recovery and supersession; run safe housekeeping
  while paused.
- New workspace directories use Octomus task IDs; existing saved paths and native
  session identities remain valid for recovery.
- Dashboard builds precede Rust builds; `--assets` is an explicit override.
- Operator checklist moved to `docs/operations.md`; README starts with installation
  and first-run guidance.

### Fixed

- Export `blocked_reason` with the task API's vocabulary (`verification_failed`)
  instead of a lower-cased Rust type name.

### Removed

- The superseded `GET /api/models` Codex-only catalog endpoint; `POST /api/model-catalog`
  reports both runners.
- The Astra rehearsal preset on the Configuration dashboard.
- Launch rehearsal draft reports and the PRD, acceptance and hardening-validation
  documents; their history remains in Git.
- The screenshot-capture spec and community boilerplate files (code of conduct,
  CODEOWNERS, label definitions and issue forms).

Publication remains pending; this is not a released version.
