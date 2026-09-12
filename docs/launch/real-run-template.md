# Real rehearsal record — NOT OBSERVED

Copy for an actual owner-operated run. This template is not live evidence.
Replace placeholders only from observations; leave unobserved items explicit.
Use **UNAVAILABLE** with a reason when evidence cannot be obtained. Never infer
success from fixture tests, configuration, catalog availability or doctor success.

## Identity and prerequisites

| Field | Observation / protected evidence reference |
| --- | --- |
| Owner, operator, dedicated VM/service identity (redacted) | NOT OBSERVED |
| Owner authorization and run scope | NOT OBSERVED |
| Application full revision, build provenance, local diff | NOT OBSERVED |
| Target repository/path, ownership, non-production scope | NOT OBSERVED |
| Separation from application launch checkout | NOT OBSERVED |
| Target baseline SHA, branch, remote SHA, clean/dirty state | NOT OBSERVED |
| Baseline verification commands, exits, times, evidence | NOT OBSERVED |
| Actual service-user client path/version/environment | NOT OBSERVED |
| Catalog/authentication/doctor results, warnings, timestamp | NOT OBSERVED |
| Initial paused/idle state, queued count, pending work resolution | NOT OBSERVED |
| Saved concurrency, tasks/cycle, interval, admission/time limits | NOT OBSERVED |
| Owner-confirmed allowance/overage policy (no credentials) | NOT OBSERVED |

## Routes and inference — separate observations

Record exact backend/model/effort and any provider/variant fields. Configured and
catalog/doctor-validated routes do not establish runtime-reported identity.

| Route | Saved route | Validated route / time / evidence |
| --- | --- | --- |
| Orchestrator | NOT OBSERVED | NOT OBSERVED |
| Discovery | NOT OBSERVED | NOT OBSERVED |
| Proposal reviewers | NOT OBSERVED | NOT OBSERVED |
| Code reviewer | NOT OBSERVED | NOT OBSERVED |
| XS execution | NOT OBSERVED | NOT OBSERVED |
| S execution | NOT OBSERVED | NOT OBSERVED |
| M execution | NOT OBSERVED | NOT OBSERVED |
| L execution | NOT OBSERVED | NOT OBSERVED |
| XL execution | NOT OBSERVED | NOT OBSERVED |
| Repair | NOT OBSERVED | NOT OBSERVED |

| Runtime field (repeat per session/turn where available) | Observation |
| --- | --- |
| Cycle/task/session ID and role | NOT OBSERVED |
| Runtime-reported model/effort, reporting source | NOT OBSERVED |
| Inference completion/error and evidence reference | NOT OBSERVED |
| Identity attribution gaps (use UNAVAILABLE with reason) | NOT OBSERVED |

## Cycle, decisions and delivery

| Field | Observation / protected evidence reference |
| --- | --- |
| Owner Run once action, UTC start and control/batch state | NOT OBSERVED |
| Actual cycle ID/number/mode and grounding revision | NOT OBSERVED |
| Proposal IDs, scope and evidence | NOT OBSERVED |
| Reviewer 1 actual assessments, decision/reasons, session ID | NOT OBSERVED |
| Reviewer 2 actual assessments, decision/reasons, session ID | NOT OBSERVED |
| Orchestrator actual accepted/rejected/deferred decisions/reasons | NOT OBSERVED |
| Task IDs, dependencies, original route/config snapshots | NOT OBSERVED |
| Task source/default revision and full comparison base | NOT OBSERVED |
| Executor and persistent repair session IDs, repair rounds | NOT OBSERVED |
| Each fresh code reviewer session, actual findings/decision | NOT OBSERVED |
| Final review completed/clean result, revision, comparison base | NOT OBSERVED |
| Output commit SHA | NOT OBSERVED |
| Every configured check: command, success/exit, revision, evidence | NOT OBSERVED |
| Output = clean review = successful check revisions | NOT OBSERVED |
| PR URL, branch/base, actual head SHA and observation time | NOT OBSERVED |
| PR head = output commit; external head movement | NOT OBSERVED |
| Final cycle/task outcome (include idle, blocked or failed) | NOT OBSERVED |
| Return to paused/idle and remaining queued count | NOT OBSERVED |
| UTC end and total planning-through-delivery elapsed time | NOT OBSERVED |

## Regression, usage and operator judgment

| Field | Observation / protected evidence reference |
| --- | --- |
| Bug reproducer/regression patch hash and exact command | NOT OBSERVED |
| Isolated baseline checkout SHA, expected failure and actual result | NOT OBSERVED |
| Isolated patched checkout SHA, identical regression and actual result | NOT OBSERVED |
| Patched meaningful suite results / limitations | NOT OBSERVED |
| Usage-report time and private reference | NOT OBSERVED |
| Admissions before/after by UTC day; attribution gaps | NOT OBSERVED |
| Planning admissions for actual cycle ID | NOT OBSERVED |
| Task/review/repair/retry admissions and task IDs | NOT OBSERVED |
| Attributable cost (amount/currency or UNAVAILABLE with reason) | NOT OBSERVED |
| Cost source, time window, attribution method; fees vs incremental cost | NOT OBSERVED |
| Interventions: operator, UTC time, reason, exact action, effect | NOT OBSERVED |
| Cancellation/stop/restart/reconciliation and remaining work | NOT OBSERVED |
| Human usefulness assessment and acceptance/rejection rationale | NOT OBSERVED |
| Follow-up defects, limitations and owner next decision | NOT OBSERVED |
| Explicit owner public-safety review, date and approved scope | NOT OBSERVED — not approved for publication |

Do not include secrets, raw runner transcripts or private billing screenshots.
Redacted references still require owner review; this record is not automatically
public-safe. No live validation or successful PR is asserted by this template.
