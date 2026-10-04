# Octomus Agent roadmap: October–November 2026

**Status: proposed.** Written on 2026-10-04 against `044e1cb`. Every milestone
starts at `TODO`; this roadmap records no completed work.

The October cleanup is merged, the issue and pull-request queues are empty, and
the service passes CI on `main`. What it lacks is a release. No tag exists, the
release workflow has never run, every installation builds from source, and the
state database refuses anything but an empty or version-7 file. This roadmap
covers the six weeks from 2026-10-05 to 2026-11-15 and aims to:

- ship the first public release, v0.1.0;
- make sure the next release can open the state v0.1.0 writes;
- stop arm64, scale and race regressions from reaching a tag;
- ship a small v0.2.0 whose upgrade runs on real v0.1.0 state.

## Milestones

| Milestone | Deliverable | Depends on | Target |
| --- | --- | --- | --- |
| [M1 — Release v0.1.0](m1-release-v0.1.0.md) | First public release: archives, signed images, docs without "pending" | Owner gates | Fri 2026-10-16 |
| [M2 — State upgrades](m2-state-upgrades.md) | Forward-only migrations from version 7, with a backup before migrating | A v7 database from M1's tag or its candidate | Fri 2026-10-30 |
| [M3 — CI coverage](m3-ci-coverage.md) | aarch64 archives, the scale check and race-instrumented e2e in CI | — | Fri 2026-11-06 |
| [M4 — v0.2.0](m4-v0.2.0.md) | Notification and grounding follow-ups, released over migrated state | M2, M3 | Fri 2026-11-13 |

M2 and M3 start alongside M1. The owner gates in M1 can move M1 and the M4
release, but not the M2 or M3 engineering: M2's golden database can be made from
the release candidate commit and replaced once the tag exists.

Targets are planning dates, not commitments. The milestone files own status; this
table is an index, not a second progress ledger.

## Rules

- **Status lives in the milestone file.** Update its progress record when work
  starts, blocks or finishes. A milestone is `DONE` only when every acceptance
  criterion has passing, attributable evidence; a skipped required check is not a
  pass.
- **Owner gates are recorded, not worked around.** When a gate holds a milestone,
  mark it `BLOCKED` and name the gate and its unblock condition.
- **Fixture results are not live evidence.** The e2e scenarios, pinned-client
  contracts and sandbox tests use fixtures and synthetic providers. Do not claim
  live validation without real evidence ([AGENTS.md](../../AGENTS.md)).
- **The operating contract does not change to meet a date.** No route
  substitution, no weakened verification, publication stays with the
  orchestrator, and a change to a golden container spec is reviewed as a change
  to the isolation boundary.
- **Saved records stay loadable.** A field added to a saved record must decode
  from records written before it existed (see [M2](m2-state-upgrades.md)).
- **Product docs change with behavior.** Update `docs/` when a milestone changes
  what an operator does; a roadmap entry is not installation guidance.

### Statuses

| Status | Meaning |
| --- | --- |
| `TODO` | Work has not started. |
| `IN_PROGRESS` | Implementation or acceptance verification is underway. |
| `BLOCKED` | A named prerequisite prevents completion. |
| `DONE` | Every acceptance criterion has passing evidence. |

### Progress record

Each milestone ends with a record in this form:

```text
Milestone:
Status: TODO | IN_PROGRESS | BLOCKED | DONE
Revision:
Delivered output:
Commands and results:
Unrun checks and blockers:
Next:
```

Record commands, results and stable references such as commit hashes, workflow
run URLs and release URLs. Keep logs, secrets and runner transcripts out of these
files.

## Not in this horizon

- **Live validation and cost.** A canary on the owner's dedicated VM and bot, and
  real per-task and daily cost figures, need owner authorization for live model
  calls and GitHub writes. [Usage and costs](../cost.md) stays "pending" until
  then.
- **Rebasing onto a moved default branch.** Tasks whose base moves keep blocking as
  `stale_base` (`internal/model/enums.go`). Rebasing touches revision identity
  and needs its own re-review and re-verification design.
- **Sandbox runtimes.** gVisor remains unvalidated ([sandbox](../sandbox.md)).
- **Release signing beyond images.** Release archives keep checksums without
  Sigstore signatures ([releasing](../releasing.md)).
- **Network exposure.** No TLS listener, multi-user roles or token expiry
  ([threat model](../threat-model.md)).
- **Scope growth.** Multiple repositories or operators, new providers, automatic
  merging or deployment.
