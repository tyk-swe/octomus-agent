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
Status: TODO
Implementation revision: Not started.
Delivered output: None.
Acceptance tests and commands: Not run.
Results: No acceptance evidence recorded.
Intentional behavior differences: None approved.
Unrun required checks and blockers: All acceptance checks unrun; depends on M1.
Next eligible milestone: M4 after M2 and M3 are DONE.
```
