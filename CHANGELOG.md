# Changelog

## Unreleased

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

- Enforce live admission policy for queued and recovered tasks; preserve immutable contracts and explicit attempt limits.
- Validate exact PR publication results, complete same-branch dependency orders, and bounded machine output.
- Persist one-shot operation, typed recovery and supersession; run safe housekeeping while paused.
- Add indexed history summaries, authoritative counts, paginated evidence, decision memory, idle backoff and PR outcome observation.
- Exercise pinned real clients with isolated synthetic providers, and add operational, browser and scale regressions.

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

### Changed

- New workspace directories use Octomus task IDs; existing saved paths and native
  session identities remain valid for recovery.

- Dashboard builds precede Rust builds; `--assets` is an explicit override.
- Operator checklist moved to `docs/operations.md`; README starts with installation
  and first-run guidance.

Publication remains pending; this is not a released version.
