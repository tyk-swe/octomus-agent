# First real run — owner-operated rehearsal

This is an operator procedure, not evidence of a live run. Use the existing
[README](../../README.md), [deployment controls](../deployment.md), and
[Astra preset](astra-rehearsal.md). The owner supplies the dedicated VM, service
identity, repository-restricted GitHub access, target path/repository, baseline,
and meaningful verification commands. The development workspace is not that VM.
Account setup and authentication must already be owner-approved and complete.
Do not print tokens, copy credentials into evidence, or change billing here.

## 1. Establish the target and baseline

Record the installed application revision/build provenance and any local diff.
Choose one owned, non-production repository in a checkout separate from the
Octomus checkout being edited for launch. As the service user, record its absolute
path, origin identity, default branch, clean/dirty state and full baseline SHA
(`git -C TARGET rev-parse HEAD` and `git -C TARGET status --short`). Confirm the
intended remote branch is at that baseline; stop for owner resolution if it moved.
Use an owned `tyk/` branch prefix.

Install the target's required tools in advance. Run the owner's actual verification
commands on that baseline as the service user with the service's effective PATH
and permissions. Record exact commands, exit codes, UTC times and private output
references. A broken baseline or unavailable prerequisite needs owner resolution;
do not remove checks to make the rehearsal pass.

## 2. Establish an empty, paused instance

Use the owner's approved instance and loopback/SSH dashboard access. Inspect
authenticated `GET /api/state`: require `control.mode = "paused"`,
`control.paused = true`, `active_tasks = 0`, `cycle_active = false`, and
`counts.queued` absent or zero. Inspect Task queue and pending rediscovery work
too; a paused status alone does not prove idleness. Review the saved control/batch
state and any unresolved work before changing configuration.

If work exists, stop this procedure and resolve it with the owner using eligible
task controls, or use a separately approved fresh instance/data directory. Never
delete or overwrite existing state/workspaces to obtain an empty queue. Run once
captures and drains queued tasks before planning, and those tasks retain their
original route/configuration snapshots. Saving the preset does not reroute them.

## 3. Validate and save every Astra route

As the service user, check the exact configured Codex executable with `--version`.
This application pins/tests `codex-cli 0.153.4` (`src/codex.rs` and README).
Resolve a version mismatch before live work. Match the running service's executable
path, home and environment; a different user's successful catalog is insufficient.

In Configuration, set the target identity/path, branch policy, executable path and
the baseline-tested verification commands. Load Codex models for that draft path.
Require the exact available `gpt-6-astra` entry and a supported effort; reload after
path changes or failure. Apply and confirm the Astra rehearsal preset, then inspect
Orchestrator, Discovery agents, Proposal reviewers, Code reviewer, XS/S/M/L/XL
execution, and Repair: all must be Codex / `gpt-6-astra` / the chosen supported
effort, without OpenCode provider/variant fields. Confirm nine discovery agents,
one concurrent task, one task per cycle, and a 21,600-second interval. Check all
other settings, especially unsaved verification commands and timeouts, then select
**Save configuration**. The preset itself neither saves nor starts work.

Confirm the owner's subscription allowance/paid-overage policy and chosen daily
session admission limit without printing credentials or altering billing. For
subscription-only operation the owner must confirm paid overage is disabled.
Admissions include failed starts and retries; they are neither dollars nor a
subscription allowance cap. Nine-agent planning normally uses 13 admissions;
execution, reviews and repairs use more. Do not treat that estimate as a measured
run total or a guarantee of sufficient allowance.

Select **Check connection** for full execution validation of the saved config.
The running-service equivalent is authenticated `POST /api/doctor?mode=execution`;
inspect its `ok`, `warnings`, `backends`, `models`, `codex_version` and
`tested_codex_version`. Resolve errors/warnings and save/recheck any changed route.
`POST /api/model-catalog` accepts `{"backend":"codex","binary":"EXACT_PATH"}`
and checks a draft executable; it does not save configuration.

For a stopped instance only, the equivalent CLI is:

```sh
octomus-agent --data-dir APPROVED_DATA_DIR --doctor
```

CLI doctor takes the same `service.lock` as the service and fails if it is held.
Do not remove the lock or substitute another data directory to bypass it. Prefer
the running service's doctor API; stopping a service can interrupt work.
`--doctor --audit`/Check audit connection checks only planning prerequisites and
is insufficient here. Catalog/doctor success checks authentication, repository
access and route availability; it is not successful inference or PR delivery.

All `/api/*` calls require `Authorization: Bearer <operator token>` through the
owner's protected client; use JSON content type for JSON requests. Do not paste
credentials or credential-bearing commands into the record. `GET /api/config`
can confirm the saved routes; `PUT /api/config` is the existing save endpoint.
The dashboard is sufficient; no manual state database edits are needed.

## 4. Owner chooses one complete cycle

Recheck paused/idle state, empty queue, saved routes and unchanged target baseline.
Start a private copy of [the evidence template](real-run-template.md), recording
UTC start time and admissions before starting. Have the **owner select Run once**
(authenticated `POST /api/control/cycle`) exactly once. This authorizes one planning
cycle and its accepted task batch, then returns to paused. Do not run an audit just
for the recording: it spends admissions and a later execution cycle plans afresh.
Do not select Start continuous or silently repeat Run once.

Track the actual cycle ID, proposals, both proposal-reviewer assessments and the
orchestrator's decisions in Proposals / `GET /api/cycles/{id}`. Track task IDs and
their full evidence with `GET /api/tasks/{id}`. Let Octomus own publication; never
substitute operator `git push` or `gh pr create`. No accepted work, a blocked task,
or a failed cycle is a recordable outcome, not permission to weaken checks for a PR.

## 5. Intervention and stop behavior

**Pause** (`POST /api/control/pause`) stops new dispatch; active planning/tasks may
finish, including publication. It does not cancel them. Use **Cancel task**
(`POST /api/tasks/{id}/cancel`) only when eligible in `allowed_actions`; cancellation
of active workers is asynchronous, so inspect the resulting state. Active
publication returns a conflict and must reconcile; published tasks cannot be
cancelled. There is no cycle-cancel API.

If the service responds, select Pause before stopping to persist no-new-dispatch.
To stop workers, the owner can stop the foreground service with Ctrl-C/SIGTERM,
or the administrator can use `sudo systemctl stop octomus-agent` for the supplied
unit. The service cancels owned workers; the unit uses `KillMode=control-group`.
Stopping cannot undo remote effects already completed. Record why and when it was
stopped. Stopping alone preserves operating mode: restarting can queue interrupted
tasks for bounded recovery and automatically continue a saved Run once batch or
continuous operation. Do not restart casually; have the owner decide recovery.
If paused before stopping, inspect recovered state before authorizing dispatch.
Eligible Retry task, Reconcile publication,
or Supersede and rediscover are deliberate owner interventions, not automatic
instructions to obtain a successful recording. Preserve failures and workspaces.

## 6. Preserve and assess evidence

Record actual UTC start/end and total elapsed time through task completion/PR and
return to paused. Cycle completion time covers planning and can exclude task time.
Preserve cycle/task/proposal IDs, decisions, session IDs, configured and validated
routes, and separately any runtime-reported model identity with its evidence
source. Saved session routes are not independent runtime identity evidence; mark
that identity unavailable if it is not exposed in retained evidence.

For a delivered task, compare `output_commit`, the clean completed full-diff review's
`revision`/`comparison_base`, each configured successful verification's `revision`,
and the actual PR head. All output revisions must match. Preserve review findings
and repair rounds as well as the final decision, verification commands/results,
PR URL/head/base and any external head movement. Do not infer this from PR presence.

As the service user, `octomus-agent --data-dir APPROVED_DATA_DIR --usage-report`
exports read-only JSON alongside the running service without taking its lock.
Keep it private and record per-cycle/task admissions, UTC daily totals, and any
unattributed admissions. It is not provider billing. Record attributable cost only
with an owner-approved source and attribution method; otherwise use **UNAVAILABLE**.

For a bug fix, use separate isolated verification checkouts at the recorded
baseline and output SHA, outside Octomus's managed workspaces. Apply the identical
minimal regression test/reproducer to both, record its patch/hash and exact command,
and demonstrate the expected failure on baseline and pass on patched behavior.
Keep tools/inputs equivalent, distinguish setup failures from the bug, and rerun
the meaningful suite on the patch. Never reset an active task checkout to do this.

Finish with owner interventions, failure/idle outcomes and a human assessment of
usefulness. Keep raw transcripts, credentials and billing screenshots out of Git.
Use redacted observations and protected evidence references; the owner must review
the record before deciding it is public-safe. Nothing here establishes live success.
