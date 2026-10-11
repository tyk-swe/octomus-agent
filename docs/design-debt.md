# Design debt inventory

This pass preserves the single-service operating model and the existing trust
boundaries. A record remains durable authority; working task evidence may be
pending a checkpoint, and external operations require a committed authorization.
Each subsystem owns its resource lifecycle and its query or policy invariants.

## Confirmed debt resolved

| Evidence | Concrete cost | Implemented ownership | Verification |
| --- | --- | --- | --- |
| `engine/execution.go`: `transition`, `saveTask`, `publishReviewed`, `published` | A failed save changes caller state; a separate activity failure reports a committed status as failure; publication restores selected fields manually. | Commit lifecycle copies before replacing caller state, use typed task writes, and separate ancillary activity from essential transaction evidence. | Refused task/status-event/publication writes, cancellation/checkpoint ordering, restart and publication fixtures. |
| `engine/actions.go`: retry, supersede, reconcile and archive | Cancellation clearing, attempt-policy evidence, task status and merge revocation can partially commit. | Small store transactions for each coupled invariant. | Failure injection checks that task, marker, policy and merge authority remain coherent. |
| `engine/scheduler.go`: `runTask`; `actions.go`: `reconcileLocked`; `recovery.go` | Repeated registration/settlement/release logic differs; ordinary exited active tasks can remain stranded after refused settlement. | Shared task ownership and settlement; recovery covers every workerless active task and keeps admission blocked on failed settlement. | Worker panic/exit, refused settlement and retry recovery under race detector. |
| `engine/scheduler.go`: `tick` calls `sandboxReady` under `gate` | A stale broker HTTP read blocks pause and other controls for its timeout. | Read readiness outside the admission gate, then revalidate configuration, controls and runtime ownership. | Blocked broker read with concurrent pause/configuration changes. |
| `export/{export,evidence,maintenance_evidence}.go` | Exports know SQLite tables and indexes despite store ownership, and public connection/query primitives allow layout coupling. | Concrete snapshot reader owning typed indexed queries, with interpretation retained in export. | One-snapshot exports, malformed assessment preservation, indexed query plans, goldens and scale. |
| `model/maintenance.go`, `git/publish.go`, `export/evidence.go` | Independent newest-command selection can diverge and resurrect superseded success. | One recorded-evidence selector; keep eligibility and fresh authorization separate. | Newer failure, missing/wrong revision and incomplete evidence cases. |
| Engine/API fixed response maps and `web/api.ts` generic assertions | Endpoint/result mismatches appear type-safe; composed API shapes escape current property-name checks. | Named response types and endpoint operations with structural Go/TypeScript verification. | Contract drift, nested types, optionality/nullability and endpoint tests. |
| `Settings.svelte` reload regex and inline draft rules | Human wording drives recovery, and display transformations/canonical multiline values have implicit ownership. | Stable actionable error codes and a cohesive canonical/display/edited draft model. | Revision/active/unresolved conflicts and lossless untouched-field tests. |
| Dashboard components' generation/cancellation/mutation flags | Session and resource ownership is spread across components, including child writes to parent flags. | Focused resource/session owners and explicit action events/results. | Old responses, confirming-read failure, coalescing, navigation and disposal regressions. |
| `egress/lease.go`: `Revoke` and broker/login callers | Filesystem failure is discarded although callers assume authorization revoked. | Explicit revocation outcome; independently attempt removal, retain cleanup retry ownership and safe evidence. | Revocation failure, uncertain start, reaper, capacity and login rotation. |
| Sandbox remote wait and broker teardown constants | Timeout classification depends on presentation text; coordinated cleanup budgets rely on comments. | Typed termination cause and one wire-owned teardown budget contract. | Old/unknown cause conservatism, timeout/cleanup bounds and stream tests. |

## Intentional boundaries retained

- Maintenance merge holds the admission gate through its bounded external mutation
  (`engine/automerge.go:mergeAttempt`). This serializes irreversible merging with
  pause; moving every I/O operation outside every lock would break that contract.
- Codex and OpenCode adapters retain different native session/checkpoint semantics.
  The shared invocation pipeline handles admission and evidence, not a fictional
  identical backend lifecycle.
- SQLite projections and bounded indexed summaries are retained. Reading complete
  task records to reconstruct dashboard or export summaries would increase cost.
- Generic redacted configuration patches and historical raw assessments remain
  available where stronger fixed types would destroy information or transformation
  metadata. Stable response envelopes are typed separately.
- Sandbox broker authorization and filesystem checks remain independent from any
  shared owned-root layout; construction is not authorization.

## Sandbox, runner and release trace outcomes

| Confirmed evidence | Cost and resulting ownership | Behavior coverage |
| --- | --- | --- |
| `egress/lease.go`, `broker/{lifecycle,teardown}.go`, CLI `sandbox.go` discarded lease deletion errors; login rotation trusted a proxy file that may never record an interrupted grant. | Revocation now returns an explicit outcome. One prepared owner attempts lease deletion and container removal independently, retains admission until confirmed removal, and retries either remaining resource. Shutdown hands unresolved cleanup to the final/startup sweep. Typed bounded lease metadata owns login identity; scoped revocation recovers interrupted grants, leaves other sandboxes intact and blocks replacement on unreadable/unclassifiable ownership. | Filesystem obstruction, lease-only and combined failures, uncertain start, shutdown handoff, login refusal/retry and interrupted grants without a proxy file. |
| Failed/uncertain create released admission and used a separate time-limited late-create goroutine. | Failed preparation now uses the same resource owner. An unanswered create must first be observed by reserved name and then removed by ID; a 404 or elapsed time cannot prove future absence. Observed IDs survive lost removal answers. Rejected 400/409 creates own no container and cannot delete a conflicting request's container. | Delayed creation beyond the old two-minute lookup, lost removal answer, retained capacity, rejected create and final sweep. |
| Remote wait classified time limits by `TimeLimitReason`; raw exit/native wait/inspection decoding defaulted missing status to success or known OOM=false. | A typed wire outcome separates policy from diagnostic text; shared teardown/report budgets have one owner. Required exit/native status fields use strict typed decoding. Missing/unknown/malformed reports become infrastructure failures or incomplete evidence. Historical complete reports without an outcome remain readable; unclassified historical error reports fail conservatively. | Wording changes, unknown/malformed outcomes, raw null/empty/duplicate/missing frames, missing Docker wait and OOM fields, real-daemon containment/streams. |
| `runner.Runner.Client` replacement closed an adapter differently from `Release`. | Both now use one settlement path: detach ownership, close, normalize sandbox failures and retain evidence exactly once before any replacement can start. | Failed close prevents replacement; original evidence and runner-unavailable classification survive. |
| Broker retry logs and native Docker refusals bypassed the shared scrubber. | Daemon diagnostic text and broker log rendering now redact at their output boundaries, including proxy URL credentials. Tokens remain absent from lease filenames and cleanup diagnostics. | Credential-bearing native refusal and broker retry message. |
| `package.sh`, `release.py`, `release-images.py` independently accepted different VERSION alphabets/lengths. | The existing release-image validator now owns the OCI-compatible 1–128-character version policy; an offline release command supplies package preflight before artifacts are written. Release identities and valid prereleases remain unchanged. | All existing offline release cases; invalid inputs produce no dist; valid prerelease/boundary-length archive identity. |

The wider pass explicitly retained these source-backed boundaries:

- `sandbox/wire` already owns programs, stream vocabulary and workspace/home
  layout; `broker/validate.go` independently checks canonical paths, UID ownership,
  symlinks, allowed owned roots and the trusted metadata pointer. A generic root
  abstraction would conflate construction with authorization, so that candidate
  was rejected. Golden specs remain the isolation boundary.
- `runner/owned.go` already joins child and reader settlement with bounded,
  fragment-redacted stderr context. Codex NDJSON and OpenCode HTTP/SSE readiness,
  session/checkpoint and close mechanics serve different native protocols;
  `stream.go` provides their bounded framing without pretending identical lifecycle
  semantics. Further generic lifecycle layers were rejected.
- `process/{command,capture,deadline,status}.go` owns process-group cancellation,
  bounded stream drain/capture and typed intentional-kill classification. Trusted
  host Git/process operations and explicit `--sandbox off` are deliberate;
  every runner, verification and probe still starts through sandbox. The
  hypothetical nil-state `Status.Code` candidate has no current reachable caller:
  `RunPredicate` calls it only after successful capture/wait, so no speculative
  guard or changed result semantics was added.
- `workspace` cleanup and measurement use descriptor-relative no-follow traversal,
  entry/depth/reopen/deadline bounds and bounded mutation retries. Missing or
  unreadable ownership remains unknown. Replacing this machinery with a shared
  recursive walker or `RemoveAll` would weaken authorization or measurement.
- `redact/fragment.go` deliberately joins retained stdout/stderr spans before
  projecting and shortening them; otherwise split secrets can escape. Secret
  files are loaded before normal CLI startup; runner children never inherit
  operator/GitHub/webhook/merge secrets. Dynamic redactor/environment machinery
  was not justified by this single-process startup contract.
- `cmd/{main,deploy,secrets,service,sandbox}.go` keeps trusted-checkout pinning,
  special helper/login modes, secret-file identity and the explicit host/Docker
  backend choice separate. Deployment refuses unsafe origin/account combinations;
  best-effort cleanup of its one fixed `.partial` checkout is followed by an
  authoritative clone/rename result. No fallback backend or new process start
  path was introduced. `Shutdown` owns worker joins; root removed redundant
  subsequent polling without changing service/HTTP stop ordering.
- Docker's 409 removal-in-progress diagnostic has no distinct native machine
  error code; checking that native API message remains intentional. This differs
  from the now-retired decision based on our own time-limit presentation text.
- Release archives retain an explicit file manifest and provenance. Deterministic
  Python/Git/GitHub/runner and compose fixtures exercise the shipped binary,
  trusted split checkout and real isolation; they remain separate from live-owner
  validation and from pinned-client/retained-production-OCI acceptance gates.

## Wider engine trace and retained candidates

The engine trace covered grounding/discovery/review/plan commit, task dispatch,
native invocation and replacement, full-diff review, pristine verification,
publication/reconciliation, maintenance claims, controls, startup/restart,
baselines, housekeeping and shutdown. Additional confirmed fixes are baseline
settlement barriers/retry recovery, complete paged orphan recovery, a single
persistent invocation session slot, and removal of the service's polling loop
following its existing cancellation/join boundary. `TaskAction` already joins
synchronous reconciliation through settlement; scheduled tasks have their own
worker join. Independent review verified that distinction.

These remaining candidates have no demonstrated conflicting ownership:

- `engine/invocation.go` retains preparation, judgment and completed-evidence
  callbacks. They represent actual admission/clone ordering, role-specific
  validation and completed repair accounting; removing them would duplicate the
  shared pipeline or lose refused-checkpoint evidence. Independent resume/keep
  and client-ownership flags were removed.
- `engine/automerge.go:mergeRefusal` interprets the external `gh` command's failure
  diagnostics. This remains a conservative external-adapter boundary, whereas
  internal sandbox timeout classification now uses a typed wire outcome.
- Planning, PR refresh, baseline, housekeeping and merge jobs retain distinct
  settlement owners because their durable invariants differ. Task execution and
  reconciliation share the lifecycle they actually have in common. The runtime
  cancellation list and wait group join all registered work before shutdown.
- `process`, `redact` and `workspace` retain separate process-group, scrub/bound
  and filesystem authority contracts. Shared helpers already own the relevant
  boundaries; no second untrusted-child startup or host fallback was found.

This inventory records substantiated changes and retained choices; it does not
claim that every possible future design improvement has been eliminated.

## Persistence, evidence and delivery trace outcomes

Confirmed query ownership debt is resolved by `store.Snapshot`, domain history
pages and fixed task/cycle/PR/proposal summaries. Exports assemble evidence without
SQL or connection access. Full usage histories are still intentional: usage needs
saved sessions and admissions, whereas dashboards select bounded projections
before decoding. Maintenance evidence selects at most two matching PR identities
through `meta_pr_identity`; duplicate observations remain an explicit error.
Regression tests verify snapshot consistency across a concurrent writer, index
use, exclusion of unrelated unreadable records, preserved malformed assessments,
and read-time redaction before truncation. No schema or durable format changes
were needed.

Recorded-command selection is now `model.Task.VerificationFor`: append order owns
recency, independently of timestamp, revision or outcome. Publication, maintenance
eligibility, PR-body generation and exports share it. Missing, failed and
wrong-revision evidence retain each consumer's existing distinctions. Fresh Git
and GitHub authorization remains in the delivery boundary.

The wider persistence/delivery pass retained these intentional choices:

- `store/schema.go` checks unsupported versions before journal/schema changes,
  verifies an online backup, and commits each forward migration separately. Its
  final migration event commits with the last schema step. Removing those steps
  would weaken upgrade recoverability or make a committed upgrade report failure.
  Released goldens, backup tests and fresh/migrated DDL parity cover the boundary.
- `notifications/notifications.go` owns a durable claim/send/settle outbox and
  cancellation/join; interrupted sends remain recoverable instead of being
  reported delivered. The allowlisted payload, destination identity and stable
  failure categories keep policy separate from free-text diagnostics.
- `git/publish.go` keeps granular review refusal diagnostics before its shared
  eligibility check, then checks fresh trusted workspace and remote state. Those
  checks serve distinct authorization boundaries and should not be collapsed
  into recorded-evidence selection. Merge destination rules independently protect
  the configured GitHub base branch.
- `scripts/package.sh` uses a canonical explicit release-file manifest; it rejects
  symlinks and cleans temporary staging. `scripts/golden-state.py` uses each
  released ref's own harness, SQLite backup, integrity/version checks, secret/path
  scanning and provenance. These are deliberate release/compatibility evidence
  controls rather than redundant test orchestration.

## Verification and compatibility

The clean baseline production dashboard build and regular Go suite passed before
implementation. Final verification used Go 1.27.2, Node 26.11.1, npm 12.2.0 and
Docker Engine 29.9.0 in the development workspace.

| Gate | Final result |
| --- | --- |
| `make check` | Passed gofmt, Go vet, Svelte/TypeScript and Prettier. |
| `make test` | Passed the entire shuffled race suite, 66 Python behavior/release tests, executable distribution checks, all 17 service scenarios and 69 desktop/mobile dashboard tests. |
| UI logic | All 21 tests passed, including draft preservation, transformed-field protection, resource cancellation/coalescing and mutation/session generations. Also included in the full browser suite. |
| Launch assets | The normal browser suite intentionally skips this opt-in test; `npm run launch:assets --prefix web` passed separately and refreshed the visibly labeled synthetic screenshot. |
| Persistence | Released goldens, migrations/backups, fresh/migrated DDL parity, read-only refusal, typed snapshot consistency, indexed queries, malformed historical assessments and redaction regressions passed. |
| Scale | `OCTOMUS_SCALE_TEST=1` passed at 100,000 records: 104,829-byte state response and 1,327,192 peak allocated Go bytes. Under concurrent race-suite activity, p50 was 9.843 ms and p95 42.077 ms; allocation remained independent of history size. |
| Real Docker | Three daemon tests passed: verification containment, runner stdio and the containment probe. Both deterministic compose scenarios passed: self-test and planning/execution/review/verification/orchestrator delivery. Container specs remain unchanged. |
| Audit and package | Go vulnerability checking and npm audit found no vulnerabilities. Packaging passed; distribution checks also passed against the executable extracted from the generated archive. |
| Independent review | Fresh full-diff and incremental reviews covered all changed subsystems; reported implementation findings were fixed and their regressions passed. |

Intermediate verification caught response mode serialization as a number,
reactive-proxy draft cloning, stale-session callbacks, a missing child-owned
cycle-action error renderer, publication settlement losing its storage cause and
inconsistent uncertainty classification. These were corrected and rechecked.
Temporary signature/type/fixture-path mismatches during integration were fixed;
the final full checks passed. A race in new reaper-test log capture was corrected
with the existing synchronized buffer. Synthetic capability fixtures were updated
to describe their simulated server policy explicitly.

A subsequent review found that capability derivation omitted preflight ownership
and retained resource values were not marked stale when replacement reads began.
Capabilities now use the shared runtime-idle predicate, while retaining the
durable-running-cycle guard. New reads mark retained content stale immediately.
Regressions compare audit/execution preflight capabilities with handler refusals
and delay TaskDetail's replacement evidence while its updated details are visible.
Independent repair review and the repeated full checks passed before PR publication.

Native Codex/OpenCode pinned-client contracts could not run because
`OCTOMUS_CONTRACT_CODEX_BINARY` and `OCTOMUS_CONTRACT_OPENCODE_BINARY` were not
configured. Production OCI acceptance could not run because Skopeo and retained
production OCI archives were unavailable. The real Docker tests use deterministic
test images and do not establish production-image or owner-VM readiness. Service
validation used deterministic peers; no live bot/model invocation, deployment or
production migration was performed.

The database schema and durable/export formats are unchanged. API capabilities,
cycle actions and error codes are additive, while existing flattened response
fields and string mode encoding are preserved. Complete legacy sandbox exit
reports remain supported; incomplete/unknown reports stay conservative. Unknown
container creation retains its admission until removal is confirmed, and
unclassifiable login lease ownership refuses replacement. Packaging now shares
the release tooling's OCI-compatible version policy, including its length bound
and rejection of `+`, instead of accepting versions other release steps refuse.
