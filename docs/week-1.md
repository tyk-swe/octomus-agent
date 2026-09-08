# Week 1: live validation record

Status: repository preparation implemented; live commissioning **pending**.
No real cycles, merged Octomus PRs, measured costs or live screenshots are claimed
by this document. Fixture results establish orchestration behavior only.

The owner chose a dedicated, owner-provisioned VM, an owner-supplied bot with
repository-restricted authentication, and existing Codex subscription allowance
only. The owner performs account login and reviews and merges PRs. Do not enable
paid overage or add an API billing credential for this run.

## Repository validation

Validated locally on 2026-09-08: 16 Rust tests, 14 deterministic integration
scenarios and 2 Playwright tests (desktop/mobile) pass. Rust formatting and
clippy, Svelte/TypeScript checks, Prettier, and the static dashboard build pass.
The configuration example matches `--print-config`.

New coverage includes configurable repair routes, saved-route preservation on
retry, version mismatch diagnostics with parseable CLI JSON, interactive-request
blocking, failed starts, repeated repair admissions, atomic/concurrent budget
accounting, UTC day boundaries, restart persistence and read-only legacy reports.
These results used local fixtures; they do not count toward the live exit gate.

## Commissioning handoff

Supply the VM's SSH host/user and administrator access, and install the bot's
working GitHub authentication on the VM. Keep secrets out of this document.
The observation window begins after commissioning; if delayed, record actual dates.
The September 14 runner-seam gate is not automatically extended.

- [ ] Dedicated Ubuntu 24.04 VM on Proxmox, with no unrelated credentials,
  workloads, workstation GPU access or mounted workstation storage.
- [ ] Owner verifies restricted egress to GitHub, OpenAI and required package
  registries, with necessary host DNS/time services. Test actual login, clone,
  dependency installation and runtime access through that policy.
- [ ] `octomus` service account with home `/var/lib/octomus`; target checkout at
  `/srv/projects/octomus-agent`; installed binary at `/usr/local/bin/octomus-agent`.
- [ ] Git, gh, Codex CLI **0.153.4**, Rust/rustfmt/clippy, C compiler, Node 22.12+,
  npm and Python 3 installed for the service account. Install Playwright Chromium
  and system dependencies when running full browser coverage.
- [ ] Owner runs `codex login`, installs bot authentication, configures Git identity
  and noninteractive Git transport. Confirm the bot can fetch and is authorized to
  push branches and create/update PRs in `tyk-swe/octomus-agent`.
- [ ] Owner confirms the actual account uses subscription allowance only and paid
  overage is disabled. Record the subscription fee and allowance display, where
  exposed, privately. If this cannot be verified, leave cycles paused.
- [ ] Install with the [deployment guide](deployment.md); ensure systemd PATH
  includes the service user's Cargo and Codex binaries. Verify tools as that user
  under the unit's environment, not only in an interactive login shell.
- [ ] Build and run the launch-checklist verification in a fresh clone. Record cold
  timings before changing command/session/task timeouts or enabling a build cache.
- [ ] Access the loopback dashboard over an SSH tunnel; keep the token private.

## Initial configuration

Use the dashboard to save this profile; other limits initially retain shipped
values. This is a dogfood profile, not yet a measurement-backed product default.

| Setting | Value |
| --- | --- |
| repository | `/srv/projects/octomus-agent` |
| github_repo / default_branch | `tyk-swe/octomus-agent` / `main` |
| branch_prefix | `tyk/` |
| discovery_agents | 9 |
| execution_concurrency / max_tasks_per_cycle | 1 / 1 |
| cycle_interval_seconds | 21600 (6 hours after planning finishes; queued work can delay it) |
| max_sessions_per_day | 150 admissions per UTC day; not an allowance or dollar cap |
| command / session / task timeout | 600 / 1800 / 14400 seconds |

Load available models. Explicitly select all four role routes, five execution
routes and the repair route against the live catalog. Preserve exact efforts;
if a default is unavailable, choose an available route explicitly before running.
Record the saved configuration, CLI version and catalog in private evidence.
Availability in one account is not evidence of universal availability.

Enter the verification command from [the operator checklist](operations.md#4-configure-your-project).
Install browser prerequisites before full coverage. Dashboard embedding requires the
web build before Cargo, so use the documented Make targets. Run **Check connection**
while the service is running, or stop it and run:

```bash
sudo -u octomus /usr/local/bin/octomus-agent --data-dir /var/lib/octomus/.octomus --doctor
```

Doctor reports installed/tested versions, a warning on mismatch, and validates
saved routes and authentication. It does not spend a model turn, verify push/PR
writes, or prove live model quality. Stop on an unsupported route or version
mismatch and correct the host/configuration before enabling cycles.

## First cycle and daily routine

- [ ] Select **Run a cycle**; this enables subsequent continuous cycles too.
- [ ] Read grounding, every proposal decision and both adversarial assessments.
  Confirm rejected/deferred proposals are not queued; idle is a valid result.
- [ ] Inspect executor, fresh reviewers, persistent repair thread, verification
  revision and publication evidence. Confirm the bot's PR uses `tyk/` and `main`
  changes only when the owner merges it.
- [ ] Exercise pause/resume and cancellation on an appropriate running task.
  Pause allows in-flight work to finish and publish. A cancelled task must not
  publish. Test restart recovery with a preserved task and inspect its outcome.
- [ ] Each morning review and merge worthwhile PRs yourself; cancel unsuitable
  tasks and record why. Do not force acceptance merely to reach three merges.
- [ ] Export usage, inspect allowance and incremental charges, and record disk
  usage including completed clones, unresolved workspaces and Codex transcripts.
- [ ] Pause when allowance is exhausted or uncertain; never enable overage to
  preserve the schedule. Diagnose every blocked task before retrying it.
- [ ] Capture real proposal/rejection, task verification and PR screenshots for
  Week 3. Keep raw evidence private and commit only reviewed, redacted assets.

The report can run alongside the service without a token or dashboard assets:

```bash
sudo -u octomus /usr/local/bin/octomus-agent --data-dir /var/lib/octomus/.octomus --usage-report > week-1-usage.json
```

It opens existing SQLite state read-only and takes a consistent snapshot. Save
exports outside the repository, under operator-only permissions. JSON includes
all daily counters, cycle durations/decision counts, task tiers/PR links, and an
admission ledger with role and route. See [cost methodology](cost.md).
The report does not collect account billing or infer whether a PR was merged.

## Daily log

Use UTC dates and retain one row per cycle plus a daily billing/allowance note.
Link private evidence by reference, without embedding credentials or transcripts.

| UTC date / cycle ID | Wall seconds | Planning / task admissions | Accepted / rejected / deferred | Blocked reason and resolution | PR and owner merge evidence | Allowance / charges evidence | Disk bytes / screenshot reference |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Pending commissioning | — | — | — | — | — | — | — |

For every fix record the symptom, cause, regression test, deployed revision and
observed recovery. Expected areas: protocol drift, interactive requests, cold
verification timeouts, long executor sessions and retained-clone growth.

## Cache and timeout decisions

Start with workspace-local `target/`. Nine discovery clones plus task clones can
consume substantial disk, and a cold build can exceed 600 seconds. Measure first.
If needed, configure an operator-owned sccache for compiler caching; record cache
hits, cold/warm timings and disk cost. Do not claim a timeout increase is validated
until a fresh-clone verification succeeds under the service environment.

A shared `CARGO_TARGET_DIR` also introduces build locks and changes binary paths.
The integration harness normally runs `target/debug/octomus-agent` inside the
current checkout. If a shared target directory is introduced, explicitly set
`OCTOMUS_TEST_BINARY` to the just-built debug binary for that revision and serialize
verification that might overwrite it. Otherwise retain workspace-local targets.
Never accidentally test the installed production binary instead of the PR build.

## Exit review

- [ ] Five consecutive days of real cycles; every blocked task has an explanation.
- [ ] Three or more Octomus PRs merged into `main` by the owner, with links.
- [ ] `cost.md` filled with real sample counts, usage, subscription fee and verified
  incremental charges. Missing provider dollar attribution is marked unavailable.
- [ ] Real screenshots saved and linked for Week 3.
- [ ] Promote the six-hour/single-task profile to shipped defaults only after the
  live measurements validate it. Update Rust defaults, configuration example,
  dashboard descriptions and documentation together. Retune timeouts from evidence.
- [ ] Record actual completion date in the release plan. If the full gate is unmet
  by September 14, defer the runner seam to post-launch.
