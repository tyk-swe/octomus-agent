# Hardening validation and scale measurement

Local development observations, not evidence of live GitHub delivery, account access,
model quality or week-long operation. The owner-operated seven-day acceptance procedure
is in [operations.md](operations.md#week-long-hardening-acceptance).

## Regression coverage

`tests/hardening.rs` and `tests/e2e_hardening.py` exercise changed limits across restart,
publication races and identity mismatches, total branch ordering, typed stale recovery,
supersession and obsolete objectives, one-shot restart boundaries, paused cleanup,
preserved unresolved evidence and symlink refusal, separate runner storage measurement,
stable problem identity, PR outcome observation and late-response handling in task
details. They run in `make test`.

The pinned Codex 0.153.4 and OpenCode 1.18.30 clients complete the isolated contracts in
`tests/contracts.rs` (a completed turn, structured review, session resume after client
restart, and cancellation) against the local synthetic provider in
`tests/fixtures/provider.py`. CI's `client-contracts` matrix reproduces them without
operator credentials; see [model routing](model-routing.md#verification).

## History scale measurement

Measured on 2026-09-10 at `5314f2d` with
`cargo test --locked --test history_scale -- --ignored --nocapture`. The fixture stores
4 KiB prompts in 1,000, 10,000 and 100,000 historical task records; each measurement
includes 20 dashboard snapshots using the debug build.

| Historical tasks | State JSON bytes | Median query time | p95 query time | Peak additional Rust allocations |
| --- | ---: | ---: | ---: | ---: |
| 1,000 | 95,227 | 14.27 ms | 18.04 ms | 1,344,963 bytes |
| 10,000 | 95,528 | 13.99 ms | 23.44 ms | 1,345,563 bytes |
| 100,000 | 95,829 | 16.50 ms | 23.57 ms | 1,346,163 bytes |

These timings are environment-dependent. Allocation measurements cover the Rust
allocator, not SQLite's C allocations or total process RSS. The regression also asserts
the 300-task summary bound and a response below 1 MiB for this fixture, and a separate
test checks that an old blocked task outside the recent window is still exposed by
authoritative attention counts and filtered history.
