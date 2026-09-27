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
- Verification runs on the reviewed revision. A command that changes tracked files,
  moves `HEAD` or leaves an untracked file that is not git-ignored is recorded as failed
  evidence rather than passing silently, so verification artifacts such as coverage
  reports must be git-ignored.
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
  status, secret-scrubbed within 16 KiB, and marks every cut. A failed Git or GitHub
  command's error keeps both ends of long stdout and stderr around an explicit omission
  marker, and always leaves room for stderr.
- A command stopped by a timeout or cancellation gets `SIGTERM` and up to two seconds to
  exit before its process group is killed, so Git can remove its lock files.
- Pull request descriptions and follow-up comments list one verification line per
  configured command, from its latest result at the reviewed commit, so a re-run flaky
  failure no longer appears beside its pass.
- Errors name what failed: configuration errors the setting and its accepted range (the
  dashboard's operating-limit help shows each range), route errors the route and
  component, structured-result errors the field path (for example
  `proposals[7].evidence`), planning validation the offending proposal, enum errors the
  accepted names, and an unreadable saved record its kind and id. A task blocked as
  `verification_failed` says why.
- The discovery and consolidation prompts state the rules planning enforces: the enabled
  categories, the candidate limit, proposal metadata limits and how rediscovery requests
  are decided.
- Every failed planning cycle, audits and Continuous cycles included, writes a
  `planning_error` event. A cancelled task records `Cancelled by the operator` with no
  blocked reason; the underlying cause stays in its error event.
- Runner connection failures include a redacted tail of the runner's stderr. A failed
  OpenCode event subscription reports its HTTP status like other OpenCode requests, and
  a failed OpenCode response body is read for at most two seconds and redacted before
  it is cut.
- A failed attention-outbox read or write prints one redacted `WARN notifications:` line
  to standard error per episode, without the destination URL.
- Run evidence and the scheduler's per-tick task query use indexed lookups instead of
  scanning every saved task, so neither slows as task history grows.
- The dashboard shows the service's plain-text rejections, marks archived and discarded
  cycles in the cycle picker, offers only the workspace action still open, counts in
  the singular where one item is meant, and shows the version from the `VERSION` file.
- Dashboard pull request rows state ownership in words, task tabs follow the ARIA tabs
  pattern, the baseline confirmation takes and returns focus, disabled connection checks
  say why, routes are listed in pipeline and size order, an idle cycle reads as complete
  with nothing accepted, the PR-refresh notice shows its failure reason, runner storage
  rows name their status, limit inputs declare the service maxima, both runner
  executables are required before a save, and a stale-revision save conflict offers
  **Discard edits and reload**.
- `POST /api/doctor` lists version warnings in a passing response and no longer writes
  them to the service log, `405` responses name the allowed methods, an unknown task or
  cycle action is a `404` before any eligibility check, and an audit refused after the
  first paused-and-idle check (a second tab, a double click or a baseline check started
  in between) is a `409` rather than a `400`.
- The installer takes its version from its argument or `OCTOMUS_VERSION` (not the
  generic `VERSION`), refuses a latest-release lookup that finds no stable release,
  creates a missing `INSTALL_DIR` without `sudo` when its parent is writable, and ends
  by pointing at `docs/getting-started.md` and the `/var/lib/octomus/.octomus` data
  directory.
- Development: every e2e suite runs selected scenarios by name through the shared
  `tests/harness.py`, `make test-race-e2e` runs `tests/e2e.py` against a
  race-instrumented build, `make check` runs the module toolchain's gofmt over `tests/`
  and `web/*.go` as well, and `make audit` runs govulncheck under the module's toolchain.
- Internal simplification: secret redaction lives in one leaf `internal/redact` package,
  saved records and request bodies decode through named `internal/wirejson` helpers,
  runner diagnostics are a typed document, and Go tests hold the dashboard's TypeScript
  types, limits and vocabularies to the Go records.

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
- Reconciliation waits for its publication to return before recording the outcome, so a
  retry cannot start a second publication beside it; a late success is recorded as
  delivered, and a panic becomes an error. The publication-uncertain error states its
  reason once.
- Publication pushes to the exact validated origin URL without recursing into
  submodules, and the commit message Octomus generates is secret-scrubbed like PR text.
- Redaction replaces overlapping secrets whole, keeps catching `sk-` keys after terminal
  control sequences, and leaves ordinary words such as `task-` readable.
- A structured runner answer that repeats an object key is refused as invalid JSON, so a
  review can no longer list a finding and then read as clean through a repeated empty
  `findings`.
- Existing-PR work checks default-branch freshness before recording its output, and a
  Run once request against a stale control record answers a conflict instead of doing
  nothing.
- The remote branch lookup accepts only the exact `refs/heads/<branch>` ref, so a branch
  such as `a/refs/heads/main` can no longer stand in for `main`. Grounding fetches after
  reading the remote heads, and existing-PR initialization takes its comparison base
  from the verified default-branch revision, so a push racing either step no longer
  breaks it.
- Child processes no longer inherit Git's repository-locating variables (`GIT_DIR`,
  `GIT_INDEX_FILE`, `GIT_WORK_TREE` and the like), so a service started from a Git hook
  works in its own checkouts.
- A Codex model catalog with empty continuing pages no longer pages forever, a Codex
  protocol message is no longer dropped when a timer races it, and an OpenCode server
  that logs a stdout line over 16 KiB no longer hangs the turn once its pipe fills.
- A PR refresh cut short by a pause, a configuration save or shutdown, or made obsolete
  by a saved change to the repository or branch settings, is no longer recorded as an
  inventory failure, an earlier refresh failure clears on the next complete inventory
  even while paused, and planning and housekeeping continue with the newer inventory
  when a concurrent refresh saved it first.
- PR capacity and the default-branch revision stay fresh across one slow housekeeping
  pass, the baseline panel shows a default-branch observation only for the configured
  target, every scheduler pause drops process-local PR admission authority, and a
  repository path respelled in settings (`/srv/repo/`) no longer strands queued new-PR
  tasks behind back-to-back refreshes.
- Housekeeping runs each step even after an earlier one fails. Shutdown no longer records
  a false `housekeeping_error`, and a remote observation made obsolete by a saved change
  to the repository or branch settings is dropped instead of reported as one. Retention
  skips records discarded while it runs, resumes after the last candidate it visited so
  100 permanently failing candidates cannot hold back newer ones, and removes workspaces
  that contain read-only directories such as a Go module cache.
- An unchanged cleanup failure is recorded as an activity event at most once a day per
  service process instead of on every housekeeping pass.
- Archiving a superseded task withdraws its pending rediscovery request. Archiving or
  discarding a cycle a second time is a conflict, so a repeated archive no longer
  restarts its retention clock.
- Decision `relevant_paths` match literally, so pathspec magic such as `:(glob)src/**` no
  longer fails a plan after its admissions are spent. Planning roles receive the
  grounding summary text rather than its JSON envelope, and a blank summary fails the
  pass before discovery. They also receive the PR capacity grounding observed, so audits
  no longer read capacity as unavailable.
- A finished plan commits under the scheduler gate, so a concurrent pause or observation
  cannot restore a stale idle streak, and a remote observation during a planning
  preflight no longer rejects an audit with
  `Control state changed during planning preflight`. An operator audit writes one event,
  on the cycle it starts.
- A command that breaks the workspace state check during verification still leaves
  failed evidence naming it. A role turn whose task or cycle is already cancelled is
  refused before it reserves an admission, and retry refuses a partially initialized
  workspace before starting any runner.
- A panic inside a store transaction rolls it back, and operator controls release the
  scheduler gate when a handler panics, so neither wedges the service. A row error fails
  the usage report, the dashboard counts and the duplicate lookup instead of returning
  partial results.
- An interrupted `--doctor` stops the runner processes it started and exits with
  status 1, and the HTTP server bounds header reads and idle connections.
- `--assets` directory index pages are served as HTML instead of as downloads, and a
  history `limit` beyond the integer range returns the 100-item page cap instead of one
  item.
- Dashboard: task and run panels close on every Escape, polled lists keep focus on the
  same record, an action's refresh runs after an in-flight poll instead of being dropped,
  a failed **Load older cycles** is reported and cleared on retry, a session-expired
  message survives an operator action's `401`, and a successful response that is not
  JSON, such as a proxy's sign-in page, is reported rather than shown as connected.
- Dashboard settings accept `0` for the PR maintenance thresholds, keep a cleared limit
  empty until it is filled, keep a route's effort and variant while its model ID is
  typed, never send a redacted executable preview to the model catalog, and clear a
  stale load failure after a save. The baseline panel no longer claims that no check has
  run while it loads, the overview lists only decisions that occurred, and an archived
  task's rediscovery request reads as withdrawn.
