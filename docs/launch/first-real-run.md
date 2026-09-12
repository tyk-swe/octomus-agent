# Validation on this VM and first real run

## Current VM: local testing

Use this section from the owner's approved development checkout, as its login
user. Keep its absolute path private. On 2026-09-12 this environment
has Ubuntu 26.04.1, Rust/Cargo 1.98.0, Node 24.20.0, npm 11.19.0, Python 3.14.4,
and working headless Chromium. These satisfy the local test requirements, though
they differ from CI's Ubuntu 24.04 and Node 22. Recheck tools when repeating this
procedure. The installed user-level Codex is 0.154.0; it is not the pinned 0.153.4
client needed for live acceptance. No dedicated `octomus` account or installed
systemd service was found during preflight.

The owner subsequently authorized a temporary foreground rehearsal on this
machine using its existing authenticated accounts and a privately installed
pinned client. See [the draft result](day1-result.md) for the actual failures,
development fix, publication and cleanup. That observation does not authorize
future live attempts or establish dedicated-deployment acceptance.

Run the existing fixture suites here. They start temporary Octomus processes and
exercise authenticated API controls, planning, execution, review, verification,
and publication against synthetic peers and local Git repositories. Browser tests
also use synthetic data. They require no model login, GitHub login, allowance, or
live Run once. Their simulated PRs and admissions are not live evidence.

Set `octomus_checkout` to that checkout's absolute path in your local shell.
Record the application SHA and local diff, then run the commands below in Bash.
Keep output private and capture both Make results even if one fails. The build
must precede integration tests; each Make target handles that dependency.

```bash
cd "$octomus_checkout"
umask 077
octomus_test_run="$(mktemp -d /tmp/octomus-test.XXXXXX)"
mkdir "$octomus_test_run/tmp"
git rev-parse HEAD > "$octomus_test_run/revision.txt"
git status --short > "$octomus_test_run/status.txt"
npm ci --prefix web > "$octomus_test_run/npm-ci.log" 2>&1 || {
  printf 'Dependency installation failed; preserve the log and resolve it before testing.\n' >&2
  exit 1
}
if env -u OCTOMUS_TOKEN -u OCTOMUS_DATA_DIR -u OCTOMUS_LISTEN \
  -u OCTOMUS_ASSETS -u OCTOMUS_TEST_BINARY -u CARGO_TARGET_DIR \
  TMPDIR="$octomus_test_run/tmp" make check > "$octomus_test_run/check.log" 2>&1; then
  octomus_check_exit=0
else
  octomus_check_exit=$?
fi
if env -u OCTOMUS_TOKEN -u OCTOMUS_DATA_DIR -u OCTOMUS_LISTEN \
  -u OCTOMUS_ASSETS -u OCTOMUS_TEST_BINARY -u CARGO_TARGET_DIR \
  TMPDIR="$octomus_test_run/tmp" make test > "$octomus_test_run/test.log" 2>&1; then
  octomus_test_exit=0
else
  octomus_test_exit=$?
fi
printf 'make check: %s\nmake test: %s\n' "$octomus_check_exit" "$octomus_test_exit"
```

If browser prerequisites are missing, use the installation commands in
[AGENTS.md](../../AGENTS.md); record unavailable prerequisites rather than
omitting browser tests. For any failed command, create or append to repository
root `FAIL.md` with the revision/diff, environment, UTC times, exact command and
exit, redacted failure excerpt, reproduction, and checks not reached. Preserve
the first failure and label any deliberate rerun. Do not create a failure record
claiming a failure when all commands passed. Report intentionally ignored Rust
tests separately; these targets do not cover all standalone CI/release jobs.

If a requested live rehearsal cannot pass preflight, record its unmet prerequisites
in a separate **Live preflight — BLOCKED** section of `FAIL.md`. An unstarted
cycle is not a failed inference, and successful fixture tests do not clear those
blockers. Record local command results separately.

After recording results, stop any surviving processes belonging to this test run
using their verified PIDs/process groups. Do not kill by a broad process name.
Preserve failure logs/traces before removing this run's temporary fixture state
under `$octomus_test_run/tmp` and generated browser results. Record existing
artifacts before testing and preserve earlier evidence. Keep the private logs
outside Git; remove only attributable test artifacts, retaining pre-existing
build/dependency caches. Never clean the checkout with `git clean -fdx`, remove
the checkout's `.octomus/`, or delete credentials or unrelated workspace contents.
Verify no test processes remain and inspect `git status --short` afterward.

## Live rehearsal prerequisites

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
