# M0 — Freeze the reference and establish executable contracts

[Roadmap](README.md) · **Depends on:** none · **Enables:** [M1](m1-go-foundations.md)

## Deliverable

A reproducible Rust reference, executable compatibility fixtures, a complete test
inventory, and fixed qualification budgets. Keep Rust as the default build and
release implementation; introduce Go alongside the existing tree.

Identify the exact source behind the supplied `octomus-agent.zip` plan. Record a
Git revision when available and a source/archive hash otherwise. A ZIP or a
package version does not establish a Git revision. If the archive is unavailable
or differs from the checkout, resolve and record the reference choice before
freezing expected output.

Pin the supported stable Go toolchain and initial dependencies, including the
candidate SQLite driver. The full driver compatibility gate belongs to
[M2](m2-storage-and-exports.md). Fix the measurement protocol and qualification
budgets proposed in [M8](m8-qualification.md#measurement-protocol) before seeing Go
measurements.

Standardize executable-based tests on `OCTOMUS_TEST_BINARY`. Extend helpers that
still hard-code the Rust binary, including `tests/serve_ui.py` and
`tests/evidence_snapshot.py`. Capture supported current and older configuration,
task records, database layouts, CLI output, API responses, and evidence exports.

## Acceptance criteria

1. The reference builds, and its existing `make check` and `make test` results are
   recorded. Identify pre-existing failures by test and failure signature.
2. Every Rust behavior-test group, including inline `#[cfg(test)]` modules, has a
   destination milestone. No assertion disappears because it is inconvenient to
   port.
3. The same Python fixture tests can select either executable without changing
   behavioral assertions.
4. Compatibility expectations come from the Rust reference and existing contracts,
   not Go-generated output approved after the fact.
5. Any behavior needing a correction instead of literal parity has a reproducer
   and expected corrected result.
6. The reference identity, pinned toolchain/dependency choices, test inventory,
   measurement protocol, and qualification budgets are recorded before dependent
   implementation begins.

## Test inventory

This allocation is reconciled against the frozen reference, including individual
cases from mixed-scope Rust suites below. Extend it in place rather than creating
a separate parity document. M8 reconciles the final inventory against passing
coverage.

| Reference tests | Destination |
| --- | --- |
| `src/config.rs`, `src/model.rs` inline tests | M1 |
| `src/store.rs` inline tests; `tests/usage.rs`, `tests/review_regressions.rs` | M2; pure comparisons M1 and API actions M7 as detailed below |
| `tests/evidence.rs`, `src/evidence.rs` inline tests | M2 exports and store semantics; M7 complete evidence/API integration |
| `tests/process_lifecycle.rs` | M3 |
| `tests/runners.rs`, `tests/contracts.rs` | M4 |
| `tests/core.rs`, `tests/hardening.rs` | M1–M7 by behavior; process/Git M3, planning M5, publication/recovery M6 |
| `tests/pr_capacity.rs`, `tests/pr_context.rs`, `src/engine/memory.rs` inline tests | M5 |
| `tests/review_findings.rs` | M6 |
| `tests/baseline.rs`, `tests/notifications.rs`, `src/api.rs` inline tests | M7 |
| `tests/history_scale.rs` | M8, including bounded history and duplicate lookup |
| `tests/e2e.py`, `tests/e2e_runners.py`, `tests/e2e_hardening.py` | M4–M6 as features land; consolidated in M8 |
| `tests/e2e_baseline.py`, `tests/e2e_notifications.py`, `tests/evidence_snapshot.py` | M2 exports; complete M7/M8 integration |
| `web/tests`, `web/showcase-tests`, `web/site-tests`; `tests/serve_ui.py`, `tests/serve_site.py` | M7/M8 |
| `tests/distribution.py`, `tests/crate.py`, `tests/crate_guards.py`, `tests/systemd.py` | M9; replace Rust-specific coverage before M10 retirement |
| `tests/common/`, `tests/fixtures/`, build/check/audit and CI jobs | Shared support; preserve each caller’s assertions and qualification responsibility |

## Mixed-suite case allocation

### `tests/core.rs`

| Cases | Destination |
| --- | --- |
| `repair_routes_are_backward_compatible_and_validated`, `audit_readiness_requires_only_planning_routes_and_no_verification` | M1 (configuration); M4 (catalog checks) |
| `legacy_cycles_default_to_execution_without_rewriting_evidence` | M1 |
| `cancellation_kills_the_command_process_group` | M3 |
| `unsupported_effort_never_falls_back`, `codex_version_diagnostics_do_not_accept_prefix_matches` | M4 |
| `proposal_dependencies_require_delivered_code_and_no_cycles`, `rejected_and_unowned_work_is_never_executable`, `daily_admission_budget_is_atomic_under_concurrency` | M5 |
| `private_api_enforces_auth_content_type_and_configuration_rules`, `embedded_dashboard_and_overrides_preserve_http_boundaries`, `valid_authentication_bypasses_pending_failure_delay_and_audit_controls_conflict`, `control_conflicts_explain_the_requested_operation_without_changing_eligibility` | M7 |

### `tests/hardening.rs`

| Cases | Destination |
| --- | --- |
| `legacy_modes_and_attempts_have_explicit_defaults` | M1 (saved modes/policies/actions); M5 (idle delay) |
| `old_attention_survives_bounded_dashboard_and_pages`, `unresolved_problem_identity_survives_rewording`, `published_work_remains_in_duplicate_lookups` | M2 |
| `machine_capture_never_corrupts_successful_json`, `machine_capture_fails_closed_on_any_command_failure`, `predicate_commands_interpret_only_documented_false_statuses`, `git_ancestry_is_a_predicate_and_command_errors_fail_closed` | M3 |
| `shared_branch_requires_a_total_dependency_order`, `one_shot_membership_and_committed_phase_survive_restart`, `paused_housekeeping_preserves_unresolved_evidence_and_rejects_symlinks`, `late_retry_cannot_erase_a_one_shot_failure`, `queued_history_never_hides_active_branch_writers`, `one_shot_blocks_dependents_of_retries_excluded_from_the_batch`, `unaffordable_planning_refuses_audit_and_run_once_without_side_effects`, `run_once_pauses_when_the_drain_consumed_planning_allowance`, `continuous_waits_for_planning_allowance_without_failed_cycles` | M5 |
| `remote_preflights_release_controls_and_preserve_concurrent_task_actions`, `concurrent_retries_queue_only_one_attempt`, `retry_starts_a_fresh_repair_round_budget`, `retry_rechecks_policy_after_remote_checks`, `live_policy_survives_restart_and_never_uses_task_snapshot`, `retry_preflight_adopts_the_current_command_timeout`, `publication_checks_every_identity_field_and_closed_reconciliation` | M6 |

### `tests/review_regressions.rs`

| Cases | Destination |
| --- | --- |
| `concurrent_planning_sessions_append_without_losing_evidence`, `cancellation_requires_explicit_rediscovery_and_preserves_the_route`, `cancellation_rechecks_publication_checkpoints_after_worker_saves`, `duplicate_lookup_loads_only_matches_without_truncating_or_repeating_them`, `proposal_content_revisions_cover_omitted_and_truncated_evidence_after_upgrade`, `cycle_summaries_count_candidate_decisions_after_upgrade`, `commit_plan_is_atomic_on_lineage_failure` | M2 |
| `repository_history_and_rediscovery_lineage_ignore_repository_casing`, `duplicate_titles_trim_saved_and_proposed_whitespace_after_upgrade`, `duplicate_problem_identities_preserve_unicode_and_legacy_fallbacks` | M1 (pure comparisons); M2 (indexed lookups/lineage) |
| `unknown_cycle_actions_are_not_reported_as_archive_conflicts` | M7 |

All six inline test modules are accounted for above: `config`, `model`,
`store`, `evidence`, `api`, and `engine/memory`. No additional inline modules
were found in the frozen reference. `tests/compatibility.rs` and
`tests/fixtures/compatibility/` add Rust-derived M0/M1 contracts; they do not
replace later milestones' database, protocol, API, or execution assertions.

## Verification and evidence

Record reference identity, tool versions, commands, results, and fixture paths in
the progress record. Keep a reproducible reference available for differential
tests and M9 rollback rehearsal.

Normalize nondeterministic IDs, timestamps, and temporary paths only where
necessary. Preserve identity relationships, ordering, Git ancestry, and
revision-equality assertions. Never normalize away the behavior under test.

## Frozen reference and qualification choices

The supplied archive is not present in this workspace. The implementation
reference is the unmodified Git tree `3c2b5cd50924033873d7f740f9df44daee5685db`
(Cargo package 0.1.0). This explicit checkout choice resolves the absent archive;
no archive equivalence is claimed. Reproduce with `git archive` of that revision,
then install the locked frontend dependencies and build the dashboard before
Cargo. Keep that Git object through M10.

The roadmap requests GPT-6 Astra (`gpt-6-astra`). Effective model and reasoning
settings are managed outside this repository and are not exposed to these tests;
no independently verified setting is claimed. Runtime model routes are unchanged.
Pin Go **1.27.1** and reserve
`modernc.org/sqlite` **v1.59.0** for M2 (no unused driver is added in M1).
M1 introduces `golang.org/x/text` **v0.42.0** for Unicode lowercasing and
`github.com/nlnwa/whatwg-url` **v0.6.2** for the reference URL algorithm.
Its indirect `golang.org/x/net` dependency is pinned to **v0.59.0** so IDNA
uses Unicode 17 tables on Go 1.27, with Rust-derived Unicode and Punycode fixtures
covering the newer characters.

Before Go measurements, freeze the M8 numeric budgets as written: 100k-task state
p95 <= max(2 x reference p95, 50 ms), 1k-to-100k p95 growth <= 2 x, and response
size growth <= 5%. Incremental-build median improvement is a reported target,
not an additional pass/fail budget. Use a single otherwise idle Linux amd64 host,
record CPU/RAM/kernel and exact tool versions, pre-download dependencies, and use
the same prebuilt dashboard. Compare Rust `--release --locked` with Go
`CGO_ENABLED=0 go build -trimpath -ldflags=-s\ -w`. Measure five clean backend
builds (new target/cache directories) and ten incremental builds after one
function-body literal change, with one discarded warmup, using monotonic wall
clock time. For each fixed 1k/10k/100k synthetic task database, warm up 100 state
requests, measure 1,000 sequential authenticated loopback requests in each of
five fresh service runs, and report p50/p95 and response bytes; sample idle RSS
from `/proc` after 30 seconds. Raw measurements stay outside the roadmap.

## Captured fixtures and reproducibility

`tests/fixtures/compatibility/m1.json` contains Rust-produced wire outcomes,
validation decisions, exact serialized configuration bytes/hashes, Unicode and
repository/title identities, canonical webhook URLs/hashes, decision-memory
hashes, and structured proposal/review schemas. Inputs are saved JSON strings,
not already-decoded objects, preserving duplicate keys and numeric spelling.
`tests/compatibility.rs` verifies them in `cargo test`; explicit recapture uses
`OCTOMUS_UPDATE_COMPAT=1 cargo test --locked --test compatibility` with the frozen
Rust sources. Go tests only consume these expectations.

`cli.json` freezes reference argv/environment cases, stdout, stderr and exit
codes. The shared `tests/go_foundations.py` checks command semantics and output
stream separation without requiring clap's help/error typography in Go. Its
`--go-m1` option additionally requires all later commands to fail without state
writes. `tests/serve_ui.py` and `tests/evidence_snapshot.py` now honor
`OCTOMUS_TEST_BINARY`; Rust remains their default. Existing behavioral assertions
are unchanged.

`state.json` stores fixed synthetic older records, legacy/current SQLite schema
SQL, read-only usage/evidence exports and paused-service API responses. The
self-contained `tests/compatibility_capture.py` recreates the old database,
verifies read-only exports do not write it or acquire a service lock, exercises
reference migrations/API, and confirms migrations preserve source records. API
collection waits for the service's asynchronous startup storage observation so
the paused snapshot is deterministic.
It compares against the golden by default; `--update` is restricted to the Rust
binary. Only export/storage-observation timestamps and the current UTC
capacity window are normalized. Stable UUIDs, record links, ordering, saved
timestamps, revision strings, schema SQL and privacy boundaries remain visible. These are synthetic contracts,
not live operational evidence. Database implementation/driver qualification is M2;
complete Go API/behavioral equivalence is M7/M8.

Reference verification environment: Linux amd64, Rust 1.98.0
(`88d9e12ae 2026-08-18`), Node 26.8.2, npm 11.19.1, Go 1.27.1. Frontend installation
used `npm ci --prefix web`; Chromium dependencies used
`npx --prefix web playwright install --with-deps chromium`. Raw local logs are
`/tmp/octomus-reference-check.log`, `/tmp/octomus-reference-test.log`,
`/tmp/octomus-capture.log`, `/tmp/octomus-state-verify.log`, and
`/tmp/octomus-go-qualification.log`, `/tmp/octomus-reference-browser-retry.log`,
`/tmp/octomus-reference-showcase.log`, and `/tmp/octomus-reference-site.log`;
they are not committed evidence artifacts.

## Progress record

```text
Milestone: M0
Status: DONE
Implementation revision: Working tree based on 15466e4; frozen behavior reference remains 3c2b5cd50924033873d7f740f9df44daee5685db.
Delivered output: Frozen reference/toolchain/dependencies/budgets; complete test allocation; Rust wire/CLI/state/API/export goldens; executable-selectable shared helpers.
Acceptance tests and commands: make check; make test; make check-go; make test-go; python3 tests/go_foundations.py. After the CLI parity fix: go vet ./...; go test ./cmd/octomus-agent -count=1; CGO_ENABLED=1 go test -race ./cmd/octomus-agent -count=1; CGO_ENABLED=0 go build -trimpath -o bin/octomus-agent-go ./cmd/octomus-agent; Rust-default and Go-selected tests/go_foundations.py; git diff --check.
Results: PASS. Fresh 2026-09-20 make check and make test both exited 0 without retries: 168 Rust tests passed, 5 pre-existing ignored; all Python integration stages passed; 115 dashboard, 8 showcase and 16 site browser tests passed. Go formatting, vet, unit/race and embedding gates passed. Five additional Rust-captured short help/version cases reproduced a Go CLI mismatch before the fix; all 84 CLI cases now pass against both executables. No assertion or qualification budget was weakened.
Intentional behavior differences: None approved.
Unrun required checks and blockers: None for M0/M1. No live validation, archive-equivalence claim, or later-milestone ignored-test qualification is implied.
Next eligible milestone: M1 is complete; M2 and M3 are eligible.
```

The first reference attempt overlapped two dashboard builds and failed with
`ERR_MODULE_NOT_FOUND` for `manifest-full.js`; the serial rerun completed those
stages. That rerun later stopped on Playwright trace output with `ENOSPC`, after
114 dashboard tests passed. Only this checkout's generated Rust incremental cache
was removed to free space. The exact failed case passed unchanged with:

```sh
npm test --prefix web -- tests/run-evidence.spec.ts:29 --project=desktop --output=/tmp/octomus-browser-retry
npm run showcase:test --prefix web
npm run site:test --prefix web
```

The remaining full-suite stages then passed. Successful Rust and integration
stages were not repeated; no assertion, timeout, retry policy, or required check
was weakened to obtain the result. The `make test` process itself exited on the
environment failure; the completed qualification is the recorded run plus these
explicit resumed stages. The newly added state/API fixture verifier was also run
separately after its addition to the Make target and passed.

### Fresh completion verification — 2026-09-20

The earlier environment-failure account above is historical. The fresh complete
`make check` and `make test` invocations both exited successfully, as did
`make check-go`, `make test-go`, and the Rust-default shared CLI/embed contracts.
After extending the CLI corpus and fixing short help/version handling, the
focused Go CLI and race tests, vet, static binary build, and both executable
selections of the shared contracts passed again (84 frozen CLI cases).

Fresh local logs use `/tmp/octomus-verify-20260920-122744-` with
`check.log`, `test.log`, `check-go.log`, `test-go.log`,
`go-foundations-rust.log`, `environment.log`, and `parity-verify.log` suffixes.
The additional frozen CLI outputs came from `probes.json` under the same prefix;
Go output was never used to set expected results. These local logs are not
committed evidence or live validation.

The checked-out `src/`, `Cargo.lock`, and `build.rs` are identical to the frozen
reference. The only `Cargo.toml` difference is the package include entry for
`tests/fixtures/compatibility/*.json`; it does not change executable behavior.
The reference revision and qualification budgets remain unchanged.
