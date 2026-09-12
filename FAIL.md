# Rehearsal failures and resolution

Historical failures are retained below. The completed live outcome is documented
in [the draft Day 1 result](docs/launch/day1-result.md).

## Historical preflight — blocked

Observed on 2026-09-12 at 15:28:49 UTC. The requested real Run once was not
started. This records unmet live prerequisites; it does not report a failed
model inference or a failed fixture test.

Application source: `a2f8f6288ba7a546fb4c2a1ab993c2210778bb73`.
The local changes are the environment-specific launch guide and this record;
implementation and test sources are unchanged. The selected repository is
`tyk-swe/octomus-agent`. These observations came from the development checkout
under its existing login user; its absolute path is retained only in private evidence.

## Observed blockers and reproduction

| Check | Actual observation | Impact / next action |
| --- | --- | --- |
| `codex --version` | Exit 0: `codex-cli 0.154.0` | Live acceptance requires the application's pinned 0.153.4 client. Install/select that version for the chosen service identity and recheck its catalog. |
| `getent passwd octomus` | Exit 2, no account returned | The documented dedicated service identity is absent. The owner must designate/provide the service identity. |
| `systemctl show octomus-agent.service -p LoadState -p ActiveState -p SubState` | Exit 0: `LoadState=not-found`, `ActiveState=inactive`, `SubState=dead` | No installed system service was found. There is a local debug test binary, but no `octomus-agent` executable on PATH. |
| Inspect the documented separate target checkout | Directory absent | Prepare the approved target checkout and baseline before live verification. |
| Owner-approved service authentication and repository restriction | NOT ESTABLISHED | No approved service login or repository-restricted GitHub identity was supplied/validated. Do not reuse unrelated development credentials. |
| Owner allowance/overage policy and daily admissions | NOT ESTABLISHED | Obtain the owner's policy before spending admissions. Admissions are not a dollar cap. |

Reproduce the version/account/service observations with the exact read-only
commands above, from this VM. Inspect the target directory without creating or
overwriting it. After the owner resolves these prerequisites, follow the
[live procedure](docs/launch/first-real-run.md#live-rehearsal-prerequisites):
baseline verification as the service user, authenticated paused/idle and empty
queue checks, all ten saved/catalog-validated Astra routes, and execution doctor.
Doctor success alone is not inference evidence. Do not start an audit or silently
retry a live cycle to obtain a PR.

## Local fixture verification

Environment: Ubuntu 26.04.1, Rust/Cargo 1.98.0, Node 24.20.0, npm 11.19.0,
Python 3.14.4, Chromium 153.0.8010.12. This differs from CI's Ubuntu 24.04/Node 22.

| Command | UTC start/end | Result |
| --- | --- | --- |
| `npm ci --prefix web` | 15:21:44–15:21:47 | Exit 0; 62 packages installed, audit reported zero vulnerabilities. |
| `make check` | 15:21:47–15:22:08 | Exit 0; formatting, Clippy, Svelte/TypeScript and Prettier passed. |
| `make test` | 15:22:08–15:34:13 | Exit 0; all enabled suites passed in 724.82 seconds. |

No local test failure occurred. Rust: 59 passed, zero failed, four intentionally
ignored. Integration: 25 core, 13 runner, and 20 hardening scenarios passed.
Embedded-binary/installer distribution checks and crate packaging guards passed.
All 22 desktop/mobile browser tests passed, including accessibility assertions.
No assertions, routes, or verification requirements were weakened.

The default target intentionally ignores two pinned-client contracts, the pinned
OpenCode protocol smoke test, and the explicit 100,000-record history benchmark.
They were not run or counted as passes. Standalone CI audit, release packaging,
crate-install and systemd checks were not run. The npm esbuild install-script
advisory and Playwright color-environment warnings did not fail the commands.

Private logs, exact UTC timings, command exit codes and initial artifact inventory
are retained in the local verification evidence bundle; its location is not a
public artifact. No credentials or raw runner transcripts are
included in this record. Fixture peers simulate model/GitHub behavior and operate
on temporary local repositories.

## Cleanup and remaining evidence

The run's temporary fixture state and newly generated browser results were removed
after the tests exited. Pre-existing browser results were restored. A process
attribution check found no remaining processes using this run's temporary paths;
the browser-test port 4299 had no listener. No manual process kill was needed.
Private command logs and the cleanup manifest were retained outside Git. Existing
build/dependency caches were retained; the checkout's `.octomus/`, credentials,
account configuration and unrelated workspaces were not changed.

Live cycle/task IDs, runtime model identity, reviewer decisions, output/review/check
revision matches, and PR URL: **NOT OBSERVED**. Attributable live cost:
**UNAVAILABLE — no live cycle was started**. No human usefulness assessment or
owner public-safety approval is asserted.

## Real attempt 1 — executor startup blocked

The earlier preflight entry above is historical. Following the owner's explicit
instruction to run on this machine, existing ChatGPT and GitHub authentication
were checked successfully, Codex 0.153.4 was installed privately, and a separate
target at the recorded baseline passed its full verification. A fresh paused
instance saved all ten Codex / `gpt-6-astra` / `medium` routes and passed execution
doctor with no warnings. No account login or billing change was required.

Run once started at 17:09:03 UTC and returned to paused at 17:15:14 UTC
(371.32 seconds measured with a monotonic clock). Execution cycle
`1a25930b-4c9e-4bb3-9b0c-1ac6eee09936` completed all 13 planning sessions and
accepted proposal `d1-process-pipe-completion`. Both proposal reviewers accepted
that proposal. Task `73aaf60b-cedc-49a1-b8ce-374022c4afe7` belongs to that same
execution cycle and was blocked before its first executor turn:

```text
Codex thread/resume: {"code":-32600,"message":"no rollout found for thread id 01a0969d-9499-7e32-83e9-8a22c3f5b53a"}
```

No code review, output commit, successful task verification, or PR resulted from
this attempt. The failed task, original state, and workspace were preserved before
stopping the already-paused rehearsal service.

The cause was reproduced using the existing deterministic `e2e.scenario('normal')`
after making its Codex peer reject resume before the first turn, matching the
pinned client's actual behavior. The unchanged application failed with the same
error (exit 1, 17:17:34–17:17:39 UTC). The narrow development fix resumes only
previously existing executor sessions; fresh sessions proceed directly to their
first turn. The identical regression then passed (exit 0, 17:19:21–17:19:28 UTC).
The pinned Codex contract also passed against its synthetic local provider.

Changes are on development branch `tyk/rehearsal-fix-20260912`, in
`src/engine/execution.rs`, `tests/fixtures/codex.py`, and `tests/contracts.rs`.
Full development checks passed: `make check` exited 0 at 17:21:04 UTC and
`make test` exited 0 at 17:33:18 UTC (59 Rust tests, 58 integration scenarios,
distribution/packaging checks, and 22 browser tests). The fix is local commit
`14ffefc610a0f0330624c531f945f87ad28580b1`; it has not been pushed. No model
substitution or publication check was changed. The original accepted
subprocess-cleanup task remains separate from the executor startup defect.

At 17:33:35 UTC the corrected temporary service was restarted while paused. The
supported **Supersede and rediscover** action retained the old task and requested
a fresh decision. The operator raised the daily admission limit from 30 to 40
to cover the second planning pass and normal review/repair turns; this is not a
dollar cap. Routes, target baseline, and verification commands were unchanged.
Run once was deliberately requested again at 17:33:58 UTC, creating execution
cycle `aebbd918-063b-440e-97c9-1758c93b36b0`.

## Resolution and cleanup

The replacement task `85231f37-a1a0-41c9-aa09-901ad2e3980f` completed execution,
fresh full-diff review and the required full verification, then Octomus published
[PR #3](https://github.com/tyk-swe/octomus-agent/pull/3). Output, clean review,
successful required check and observed PR head all match
`06bdf7a81dde2bb932e9d18b102568423493690d`; the comparison base remains
`a2f8f6288ba7a546fb4c2a1ab993c2210778bb73`.

The identical four-case lifecycle regression failed on the original baseline
(exit 101) and passed on the output commit (exit 0), in isolated verification
checkouts. The corrected executor startup worked in the real continuation.
No further actionable internal review finding occurred. Total live admissions
were 29; attributable cost remains UNAVAILABLE.

The service returned to paused/idle at 18:20:45 UTC and was stopped at 18:25 UTC.
Rehearsal-owned runtime, token, private pinned client, clones and caches were
removed after retaining private state/source/evidence archives. No owned service
process or listener remained. The original preflight and missing-rollout failure
remain preserved; this successful continuation does not erase them.
