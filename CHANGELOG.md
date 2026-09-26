# Changelog

Notable changes are recorded here using [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Binary releases remain pending; this section describes what
the first release contains.

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
- Durable admission accounting, owned pull-request capacity limits, decision memory,
  attention notifications and read-only usage reports.
- Linux x86_64 and aarch64 release packaging and a checksum-verifying installer.
- A security policy, threat model, hardened systemd unit, failed-authentication backoff
  and warnings for non-loopback listeners.

### Changed

- Refreshed dashboard and login styling, clearer setup-state explanations, and audit-first
  guidance for installations without a recorded cycle. The public dashboard screenshot
  reflects the current interface using synthetic data.
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
- The dashboard build precedes the Go build; `--assets` is an explicit override.
- Fresh state uses SQLite schema version 7; earlier databases are refused before
  schema or journal changes.
- A graceful stop (SIGINT, SIGTERM or a terminal hangup) leaves initialized in-flight
  tasks to the same bounded restart recovery as a crash, and records a planning pass it
  cuts short as interrupted rather than failed.
- Verification evidence keeps the end of each command's stdout and stderr and its exit
  status, secret-scrubbed within 16 KiB, and marks every cut.
- Configuration errors name the failing setting and its accepted range, and the
  dashboard's operating-limit help shows each range.
- Runner connection failures include a redacted tail of the runner's stderr.
- The dashboard shows the service's plain-text rejections, marks archived and discarded
  cycles in the cycle picker, offers only the workspace action still open, counts in
  the singular where one item is meant, and shows the version from the `VERSION` file.
- `POST /api/doctor` returns version warnings in its response without writing them to
  the service log, and `405` responses name the allowed methods.

### Removed

- The public website at `octomus-agent.tyk.sh` — the Cloudflare Workers homepage, the
  generated documentation pages, the hosted showcase sample and the GitHub Pages mirror —
  retired before the first release, together with its publishing tooling. Documentation
  lives in `docs/` and the dashboard stays embedded in the service binary.
- The Codex-only `GET /api/models` endpoint, superseded by `POST /api/model-catalog`,
  which reports both runners.

### Fixed

- An interrupted publication whose retry budget is exhausted is blocked as
  `publication_uncertain`, so **Reconcile publication** can finish it without a model
  turn, and a publication recorded just before the task deadline is kept.
- Existing-PR work checks default-branch freshness before recording its output, and a
  Run once request against a stale control record answers a conflict instead of doing
  nothing.
- Archiving a superseded task withdraws its pending rediscovery request. Archiving or
  discarding a cycle a second time is a conflict, so a repeated archive no longer
  restarts its retention clock.
- Publication pushes to the exact validated origin URL without recursing into
  submodules, and the commit message Octomus generates is secret-scrubbed like PR text.
- Redaction replaces overlapping secrets whole, keeps catching `sk-` keys after terminal
  control sequences, and leaves ordinary words such as `task-` readable.
- An interrupted `--doctor` stops the runner processes it started and exits with
  status 1, and the HTTP server bounds header reads and idle connections.
- An unchanged cleanup failure is logged at most once a day instead of on every
  housekeeping pass.
