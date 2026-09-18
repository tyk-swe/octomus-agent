# Changelog

Notable changes are recorded here using [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Nothing has been published yet; this section describes what the first release contains.

### Added

- Continuous and one-shot cycles that discover work, challenge it with two independent
  reviewers and deliver verified pull requests, plus audits that record recommendations
  and dispatch no tasks.
- Per-role Codex and OpenCode routing, with locally managed OpenCode servers and guided
  provider, model, effort and variant configuration.
- An embedded single-operator dashboard with a guided setup checklist, saved
  configuration drafts, connection and clean-baseline checks, and an Inspect run panel.
- Read-only run evidence (`RunEvidenceV1`) through `GET /api/cycles/{id}/evidence` and
  `--export-run`, with a documented consistent SQLite snapshot procedure.
- A standalone static showcase build (`web/showcase`) of recorded evidence, with
  allowlist and duplicate-key gates, hash-bound approval and network isolation.
- A public preview site with a guided synthetic sample, reproducible Product Hunt
  gallery assets, launch copy, and a manually triggered GitHub Pages workflow.
- Durable admission accounting, owned pull-request capacity limits, decision memory,
  attention notifications and read-only usage reports.
- Linux x86_64 and aarch64 release packaging, a checksum-verifying installer and Cargo
  package metadata.
- A security policy, threat model, hardened systemd unit, failed-authentication backoff
  and warnings for non-loopback listeners.

### Changed

- Refreshed dashboard and login styling, clearer setup-state explanations, and audit-first
  guidance for installations without a recorded cycle.
- Verification runs on the reviewed revision, and a command that changes tracked state
  is recorded as failed evidence rather than passing silently.
- Repair rounds are budgeted per attempt: an explicit retry starts a fresh budget and
  keeps earlier review evidence.
- Planning attaches every started role to its cycle before a partial batch failure ends
  the run, so incomplete planning is still inspectable.
- Pull-request targets resolve once through a shared owned-PR resolver, and ambiguous
  matches are rejected rather than guessed.
- Follow-up deliveries comment on the owned pull request instead of rewriting its
  description, so maintainer edits are never replaced; a cancelled task's reserved
  pull-request capacity is released once the remote settles its admitted branch.
- The dashboard build precedes the Rust build; `--assets` is an explicit override.

### Removed

- The Codex-only `GET /api/models` endpoint, superseded by `POST /api/model-catalog`,
  which reports both runners.
