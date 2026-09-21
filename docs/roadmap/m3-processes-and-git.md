# M3 — Implement process ownership and Git primitives

[Roadmap](README.md) · **Depends on:** [M1](m1-go-foundations.md) ·
**Enables:** [M4](m4-runner-adapters.md), once M2 is also complete

## Deliverable

A shared process-execution implementation and Git/GitHub primitives exercised
with real temporary repositories and fixture peers.

Use argument vectors for Git and GitHub operations. Preserve Bash with `pipefail`
for operator-configured shell verification. Keep diagnostic preview and
machine-readable output capture as separate contracts.

Every subprocess and goroutine needs an owner, cancellation path, and cleanup or
join path. `exec.CommandContext` alone does not reproduce the Rust process-group
lifecycle.

## Acceptance criteria

1. Cancellation, deadline expiration, service shutdown, and normal parent
   completion terminate owned descendants and complete pipe draining within
   bounded time.
2. A child inheriting stdout or stderr cannot hang the service after its parent
   exits.
3. Every successfully started direct child is waited for. Startup failures and
   cleanup errors do not leak goroutines or descriptors.
4. Preserve the 256 KiB diagnostic preview and 16 MiB machine-output ceiling.
   Oversized or invalid-UTF-8 machine output fails explicitly. This ceiling is
   distinct from the runner protocol’s 16,000,000-byte bound in M4.
5. Preserve exit-status distinctions: documented false predicate results differ
   from command failure, signal termination, and timeout.
6. Remove `OCTOMUS_TOKEN` and `OCTOMUS_NOTIFICATION_WEBHOOK_URL` from child
   environments.
7. Workspace ownership, symlink rejection, cleanliness, revision lookup, clone
   identity, and ancestry checks pass with real Git.
8. Cleanup leaves unrelated processes untouched.

## Verification and evidence

Port `tests/process_lifecycle.rs`, process cases from `tests/core.rs` and
`tests/hardening.rs`, and relevant Git regressions identified in M0.

Explicitly test descendant cleanup after successful parent exit as well as
cancellation. Record bounded cleanup results and fixture-peer outcomes; do not
use live GitHub writes for these checks.

## Progress record

```text
Milestone: M3
Status: DONE
Implementation revision: Working tree based on ecc1701; frozen behavior reference remains 3c2b5cd50924033873d7f740f9df44daee5685db.
Delivered output: internal/process (Command with owned process groups and secret scrubbing, GroupChild group-kill ownership, Capture plus Run/RunMachine/RunPredicate/ShellCheck, 256 KiB diagnostic and 16 MiB machine capture contracts, WithDeadline and Bounded/BoundedAt), internal/git (git/gh argument-vector primitives, remote validation and revision lookup, independent detached CloneAt, cleanliness/Snapshot/At/IsAncestor, paginated inventory and PR parsing, publication safeguards and exact-lease push for M6), internal/workspace (Initialized, DirectorySize, RemoveOwnedDir), and a multi-cause BlockedReasonFromError traversal in internal/model. ShellCheck is the single `bash -o pipefail -c` construction site; no other code path builds a shell command line.
Acceptance tests and commands: gofmt -l cmd internal tests/go web/embed*.go (via make check-go); go vet ./...; go test ./...; CGO_ENABLED=1 go test -race ./...; go test -race -count=2 ./internal/git/; CGO_ENABLED=0 go build -trimpath -o bin/octomus-agent-go ./cmd/octomus-agent; OCTOMUS_TEST_BINARY=bin/octomus-agent-go python3 tests/go_foundations.py --go-m1; OCTOMUS_TEST_BINARY=bin/octomus-agent-go python3 tests/evidence_snapshot.py; cargo test --locked --test process_lifecycle; cargo test --locked --test core cancellation_kills_the_command_process_group; cargo test --locked --test hardening -- machine_capture_never_corrupts_successful_json machine_capture_fails_closed_on_any_command_failure predicate_commands_interpret_only_documented_false_statuses git_ancestry_is_a_predicate_and_command_errors_fail_closed; make check-go; make test-go; git diff --check.
Results: PASS. 16 process cases, 9 git cases and 3 workspace cases pass with and without -race. Ported process_lifecycle: inheriting stdout/stderr on zero and nonzero exits cannot hang capture because the owned group is killed when the leader exits and readers drain before return. Cancellation and deadline expiration kill the whole group (asserted via a descendant pid file and a surviving unrelated process), the leader is reaped, and startup failure leaks no descriptors or goroutines. Machine capture fails closed on command failure, >16 MiB output and invalid UTF-8 while diagnostic capture keeps a truncated preview and drains completely; predicates treat only configured exit codes as false and fail closed on signals, timeouts and unexpected statuses. Child environments drop OCTOMUS_TOKEN and OCTOMUS_NOTIFICATION_WEBHOOK_URL and pin GIT_TERMINAL_PROMPT=0; ShellCheck fails a mid-pipeline failure and preserves filtered output. Real-git cases cover clone identity and detached HEAD, remote validation (SSH and credential-free HTTPS forms) and ls-remote revision lookup, cleanliness and Snapshot, and merge-base ancestry failing closed. Fixture gh peers (tests/fixtures/git.py, gh.py on PATH) drive paginated inventory parsing with duplicate/conflict/missing-identity rejection, PR creation with marker and URL validation, follow-up comments, stale-base refusal and fixture-peer descendant cleanup; no live GitHub writes. Workspace cases cover direct-child-only removal, missing targets, symlinked target and ancestor refusal, the filesystem-root edge, saturating directory size that ignores symlinks, and the three-part initialization check. The four allocated hardening cases, the core cancellation regression and all of process_lifecycle still pass in the unchanged Rust reference. go_foundations (84 frozen CLI cases) and evidence_snapshot keep passing under the Go binary.
Intentional behavior differences: (1) On cancellation or timeout Go's Capture bounds post-kill joining at 30 seconds — it waits for the leader reap and reader joins, then force-closes the read ends and returns so a setsid-escaped descendant or an uninterruptible leader cannot hang the caller; Rust drops the capture future immediately and relies on kill_on_drop plus tokio's orphan reaper. The observable contract (group killed, pipes unwound, bounded time) is the same; Go additionally waits for reaping in the common case, which the tests assert through /proc. (2) with_deadline's Deadline is a struct (Output, Expired, AlreadyCancelled) rather than a Rust enum, and process/git helpers take context.Context instead of CancellationToken — Go-idiomatic equivalents of the same cancellation contract. (3) Seconds-based deadlines use time.Duration, so an absurd seconds value (> ~292 years) wraps to immediate expiry instead of Rust's saturating Duration; configured timeouts are days at most. (4) RemoveOwnedDir rejects the "/" target at its name check where Rust rejects it at the parent check; same refusal, different message.
Unrun required checks and blockers: None for M3. Publication functions exist as tested primitives but nothing in cmd/ calls them; engine integration, run_check_command's CheckOutcome wrapper and the housekeeping loops belong to M5/M6. Live GitHub writes were not exercised, per the milestone's fixture-peer requirement.
Next eligible milestone: M4 (M2 and M3 are DONE).
```
