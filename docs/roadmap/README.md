# Octomus Agent roadmap: Rust → Go

**Status: implementation underway.** Individual milestone progress records are
authoritative. Rust remains the default build and release implementation.

Replace Octomus Agent’s Rust backend with an idiomatic, maintainable Go service
while preserving supported behavior, durable state, the dashboard, both runner
integrations, and publication safeguards. Keep the existing product and operating
contract throughout the rewrite.

The complete workflow remains:

> Repository grounding → discovery → independent proposal review → consolidation
> → isolated execution → fresh full-diff code review → repair when necessary
> → revision-bound verification → orchestrator-owned GitHub publication.

The rewrite is complete only when Go passes the behavioral and operational gates,
existing state upgrades safely, release packages work outside the source tree,
authorized cutover has real evidence, and Rust is no longer needed to build or run
the Octomus backend.

## Reading and updating this roadmap

This directory is the single migration roadmap and progress ledger. This README
owns shared scope, invariants, architecture, and verification policy. Each linked
milestone owns its deliverable, acceptance criteria, and progress record. Do not
create parallel `ROADMAP.md`, `PARITY.md`, `MIGRATION.md`, audit reports, or additional
milestone plans. Test fixtures and normal implementation files belong in the
repository; raw logs and large generated evidence belong outside the roadmap.

Read this README, the assigned milestone and its dependencies, then the relevant
reference implementation and tests. Update product documentation when behavior or
commands actually change; proposed Go commands are not current installation
instructions.

**Planning baseline:** the supplied plan identifies `octomus-agent.zip`, with
Cargo package version `0.1.0`. [M0](m0-reference-and-contracts.md) must locate and
freeze the exact source, record its revision or source/archive hash, and resolve
any difference from the working checkout. A package version alone does not
identify the reference. No archive verification is claimed by this roadmap.

**Execution model:** GPT-6 Astra. Select and record the actual model identifier and
supported reasoning setting in the execution environment. This does not authorize
changes to Octomus’s configured runtime model routes.

## Milestones and dependencies

| Milestone | Deliverable | Depends on |
| --- | --- | --- |
| [M0 — Reference and contracts](m0-reference-and-contracts.md) | Frozen Rust reference, compatibility fixtures, test inventory, and qualification budgets | — |
| [M1 — Go foundations](m1-go-foundations.md) | Executable, configuration, wire formats, and embedded dashboard build | M0 |
| [M2 — Storage and exports](m2-storage-and-exports.md) | SQLite compatibility, durable transactions, and read-only CLI exports | M1 |
| [M3 — Processes and Git](m3-processes-and-git.md) | Owned process lifecycle and Git/GitHub primitives | M1 |
| [M4 — Runner adapters](m4-runner-adapters.md) | Codex and OpenCode adapters with exact route and protocol validation | M2, M3 |
| [M5 — Planning and scheduling](m5-planning-and-scheduling.md) | Planning, scheduling, admission control, and supporting controls | M2, M3, M4 |
| [M6 — Execution and publication](m6-execution-and-publication.md) | Task lifecycle, review, repair, verification, publication, and recovery | M5 |
| [M7 — Operator experience](m7-operator-experience.md) | Complete API, operational features, and existing frontend integration | M6 |
| [M8 — Qualification](m8-qualification.md) | Differential, failure-path, concurrency, scale, and performance evidence | M7 |
| [M9 — Release and migration rehearsal](m9-release-and-migration.md) | Native release candidates and verified upgrade/rollback procedures | M8 |
| [M10 — Cutover and Rust retirement](m10-cutover-and-retirement.md) | Authorized deployment, live canary evidence, and removal of obsolete Rust code | M9 |

M2 and M3 can proceed independently after M1. Within M4, the adapters can proceed
independently once their shared contract is fixed. Do not parallelize competing
edits to canonical models, migrations, the scheduler, or publication state.

Rust remains the default build and release implementation through M8. M9 switches
the defaults only after qualification. Retain the frozen reference for comparisons
and rollback rehearsal until M10 permits its retirement.

Milestone records are authoritative for status; the table above is a dependency
index, not a second progress ledger. M0–M9 may establish `IMPLEMENTATION_READY`.
That readiness label does not mean production cutover or Rust retirement is done.

## Scope

### Preserve

Paths below refer to the Rust baseline and remain useful through the frozen
reference after source retirement.

| Surface | Required outcome | Rust reference |
| --- | --- | --- |
| CLI and environment | Flags, defaults, validation, stdout/stderr separation, and read-only commands | `src/main.rs` |
| Configuration and records | Supported saved configurations, task snapshots, routes, identities, defaults, and state vocabulary | `src/config.rs`, `src/model.rs` |
| SQLite | Canonical JSON records, migrations, projections, indexes, counters, reservations, and notification outbox | `src/store.rs`, `src/store/` |
| Planning | Discovery, reviewer identities, consolidation, audits, dependency validation, decision memory, and maintenance cadence | `src/engine/planning.rs`, `src/engine/memory.rs` |
| Execution | Immutable context, fresh review sessions, persistent repair sessions, retries, cancellation, and exact-revision verification | `src/engine/execution.rs` |
| Scheduling | Paused, RunOnce, Continuous, batch membership, admission limits, branch serialization, and PR capacity | `src/engine.rs`, `src/engine/capacity.rs` |
| Runners | Codex app-server, OpenCode HTTP/SSE, mixed-backend routing, and fail-closed validation | `src/runner.rs`, `src/codex.rs`, `src/opencode.rs`, `src/schemas.rs` |
| Git and publication | Isolated clones, repository ownership checks, branch leases, publication checkpoints, and reconciliation | `src/git.rs` |
| Processes | Owned process groups, bounded captures, cancellation, deadlines, pipe draining, and secret removal | `src/process.rs` |
| Operator features | Controls, baseline checks, history, attention states, notifications, housekeeping, usage reports, and run evidence | `src/api.rs`, `src/engine/baseline.rs`, `src/engine/housekeeping.rs`, `src/notifications.rs`, `src/report.rs`, `src/evidence.rs` |
| Frontend | Existing SvelteKit dashboard, public showcase, launch site, and API/evidence contracts | `web/` |
| Distribution | Linux amd64/arm64 packages, embedded dashboard, installer verification, and systemd supervision | `install.sh`, `scripts/`, `deploy/`, `.github/workflows/` |

### Exclude

Do not add providers, multi-tenancy, distributed scheduling, a message broker, a
database server, automatic merging or deployment, a plugin architecture, or a
frontend rewrite. Keep Git and GitHub CLI integration and isolated repository
clones; changing those designs is outside this rewrite.

Do not simplify discovery counts, runtime route defaults, review requirements,
verification commands, or admission policies to make the port easier. Preserve
compatibility with formats and behavior supported by the frozen Rust baseline;
avoid speculative compatibility layers. Keep unrelated cleanup and feature work
out of migration changes.

## Execution and completion rules

An implementation assignment requires working code, relevant tests, integration,
and an updated milestone record. A scaffold or another plan does not satisfy it.

Routine local builds, fixture tests, temporary repositories, and fixes within an
assigned milestone are authorized. Live model usage, real GitHub writes,
production state changes, release publication, and service cutover require
separate authorization. Fixture and synthetic-provider results are never live
operation evidence.

Run affected tests during implementation and consolidated qualification at the
defined integration boundaries. Reuse existing scenarios and helpers; do not add
repetitive verification scripts or additional model-review loops. Read the
relevant reference code rather than rereading the entire repository each time.

A milestone is `DONE` only when every required acceptance criterion has passing
evidence. A skipped required check is not a pass. Missing prerequisites must be
named in the progress record; use `BLOCKED` when they prevent completion. Record
partial readiness explicitly without marking the milestone done.

An intentional behavior correction needs a reference reproducer, the expected
corrected behavior, a named regression, and a rationale in the owning milestone.
Unexplained differences are failures. Do not silently weaken a gate to accept a
Go result.

## Target implementation

### Architecture

Use one Go module and one service process. The suggested module path is
`github.com/tyk-swe/octomus-agent`:

```text
cmd/octomus-agent/
internal/config/
internal/model/
internal/store/
internal/process/
internal/runner/
internal/git/
internal/engine/
internal/httpapi/
internal/evidence/
internal/report/
internal/notifications/
web/embed.go
web/src/
tests/
```

Create packages when their implementation is needed. Prefer `net/http`,
`encoding/json`, `context`, `database/sql`, `os/exec`, `log/slog`, and ordinary Go
synchronization. Use concrete types by default and small interfaces at real
substitution boundaries, especially the runner adapters. Avoid empty layers,
standard-library wrappers, generic repositories, service locators,
dependency-injection frameworks, and a universal workflow engine.

### Toolchain and SQLite

Pin a supported stable Go toolchain and initial dependency versions in M0; pin
later dependencies when introduced. Record exact qualification versions. Start
with `modernc.org/sqlite` as the single driver and settle its compatibility in M2
before building orchestration on it. A replacement requires concrete
incompatibility or measurement evidence and one decision recorded in M2. Do not
maintain interchangeable drivers.

Release builds are intended to be CGO-free; race-test jobs remain CGO-enabled.
Initially limit the application database handle to one open connection, matching
serialized Rust access. Preserve WAL, `synchronous=FULL`, and the existing busy
timeout. Apply connection-local settings to every physical connection.

Within a transaction, use only that transaction or its owned connection. Do not
call back through `sql.DB` and wait for a connection already held by the
transaction. Do not hold application locks or transactions across runner,
Git/GitHub, webhook, or long filesystem operations. After asynchronous preflights,
revalidate authoritative state before committing an action.

### Ownership and cancellation

Every goroutine and subprocess needs an owner, cancellation path, and join or
cleanup path. Durable jobs belong to the application lifecycle: a browser
disconnect must not cancel accepted durable work. Request-scoped reads and
preflights should respect request cancellation.

Preserve the distinction between pausing admissions and cancelling active work.
Shutdown stops admissions, cancels owned work, terminates owned process groups,
awaits cleanup, persists recovery state, closes storage, then releases the service
lock.

### Embedded dashboard

Keep the frontend output at `web/build`. Place the embedding source inside `web/`
and include underscore-prefixed assets such as `_app`:

```go
//go:embed all:build
```

Production builds must fail when the real dashboard build is missing. Do not
ship a placeholder or silently rely on `--assets`.

## Non-negotiable invariants

### I1 — Publication authority

Only the orchestrator publishes. Workers must not push, create PRs, merge, deploy,
or change remotes in their permitted workflow. Preserve the dedicated-host trust
model: worker instructions and process groups are not a security sandbox.

### I2 — Revision identity

A publishable output needs a completed clean review and successful configured
verification for that exact output commit. Preserve the comparison base and full
accumulated diff. Reject empty net changes, incomplete reviews, stale evidence,
and verification that changes HEAD or tracked state.

### I3 — Runtime identity

Persist and validate backend, model, effort or variant, workspace, and native
session identities. A substituted route, wrong session, malformed result,
disconnect, timeout, or incomplete completion event cannot become success.

### I4 — Durable state

Preserve task, cycle, proposal, workspace, and session identities; dependencies;
batch membership; cancellation intent; and publication checkpoints. Do not
regenerate identities or relocate existing workspaces during loading.

### I5 — Atomic admission

Daily counters and admission records commit together. Current operating limits
govern admission; historical snapshots cannot override live policy. Failed starts
still consume committed reservations.

### I6 — Recovery

Interrupted planning cannot dispatch partial proposals after restart. Task
recovery respects workspace initialization, cancellation intent, retry policy,
and publication reconciliation; it cannot blindly restart every active task.

### I7 — Evidence and privacy

Reports describe saved evidence. Preserve explicit gaps, reviewer-slot
attribution, latest-review semantics, and cycle-plus-proposal joins. Keep
credentials, raw transcripts, and private operational material out of public
evidence and Git.

### I8 — Single writer

Exactly one service owns a state directory. Rust and Go may coexist as development
binaries, but cannot operate concurrently against the same state directory,
runner workspace, or publication target.

## Verification entry points

The commands in this section are migration deliverables, not claims that Go
targets already exist. Add only the temporary counterparts needed alongside the
existing Rust targets:

```sh
make build-go
make check-go
make test-go
```

Keep executable output predictable, for example `bin/octomus-agent-go`. Shared
executable-based tests select the implementation through one variable:

```sh
OCTOMUS_TEST_BINARY="$PWD/bin/octomus-agent-go" python3 tests/e2e.py
OCTOMUS_TEST_BINARY="$PWD/bin/octomus-agent-go" python3 tests/e2e_runners.py
OCTOMUS_TEST_BINARY="$PWD/bin/octomus-agent-go" python3 tests/e2e_hardening.py
```

Use the same selection mechanism for baseline, notification, evidence,
distribution, and browser-server helpers. Reuse scenarios without changing their
assertions to favor an implementation.

Consolidated Go qualification includes:

```sh
go vet ./...
go test ./...
CGO_ENABLED=1 go test -race ./...
```

Also build a race-instrumented service for selected subprocess-driven concurrency
scenarios. Unit-test race instrumentation does not instrument a separately built
normal executable.

Preserve frontend checks, browser tests, shell checks, dependency auditing,
package tests, and pinned-client contract jobs. Replace Cargo-specific auditing
and packaging responsibilities before retiring their implementations.

M9 makes the existing user-facing targets authoritative for Go; M10 verifies them
from a clean checkout after Rust retirement:

```sh
make check
make test
make audit
make build
make package
```

Remove temporary duplicate targets when they no longer serve migration work.

## Progress records

Update the record in the owning milestone file. Use these statuses consistently:

| Status | Meaning |
| --- | --- |
| `TODO` | Work has not started. |
| `IN_PROGRESS` | Implementation or acceptance verification is underway. |
| `BLOCKED` | A named prerequisite prevents completion; record remaining work and the unblock condition. |
| `DONE` | Every required acceptance criterion has passing, attributable evidence. |

Each record uses the same fields:

```text
Milestone:
Status: TODO | IN_PROGRESS | BLOCKED | DONE
Implementation revision:
Delivered output:
Acceptance tests and commands:
Results:
Intentional behavior differences:
Unrun required checks and blockers:
Next eligible milestone:
```

Record commands, actual results and failure signatures, and stable evidence
references. Test counts alone, “implemented,” “looks correct,” and model confidence
are insufficient. Large logs stay outside these documents; secrets and private
runner transcripts must not be committed.

### Final completion checklist

- [ ] Every in-scope Rust behavior has an implemented and tested Go equivalent.
- [ ] Supported saved configuration and durable state load correctly.
- [ ] Both runners pass fixture and pinned-client contract tests.
- [ ] Publication invariants survive cancellation, failure, and restart.
- [ ] The existing frontend works against the complete Go API.
- [ ] History, evidence, baseline checks, notifications, and housekeeping retain their contracts.
- [ ] Native amd64 and arm64 release packages pass installation and runtime tests.
- [ ] Upgrade and rollback constraints are documented and rehearsed.
- [ ] Authorized live validation is recorded separately from synthetic evidence.
- [ ] Rust is removed from Octomus’s backend build and release path.
- [ ] Every remaining compatibility helper has an identified ongoing purpose.

## Reference sources

Behavior comes from the frozen reference, its tests, and recorded corrections.
Implementation guidance does not override those contracts.

- [Go database connection ownership](https://go.dev/doc/database/manage-connections)
- [SQLite driver configuration and CGO-free implementation](https://pkg.go.dev/modernc.org/sqlite)
- [Go subprocess cancellation, waiting, and pipes](https://pkg.go.dev/os/exec)
- [Go race detector requirements](https://go.dev/doc/articles/race_detector)
- [Go embedding rules](https://pkg.go.dev/embed)
- [Codex app-server protocol and schema generation](https://developers.openai.com/codex/app-server/)
- [SQLite backup](https://sqlite.org/backup.html) and [WAL handling](https://sqlite.org/wal.html)
- [GPT-6 Astra task boundaries and repository guidance](https://learn.chatgpt.com/blog/rethinking-skills-and-prompts-for-gpt-6-astra)
