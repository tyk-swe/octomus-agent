# Week 2: installation and trust record

Repository implementation and local verification are complete. No public release, crates.io
publication, owner VM commissioning or ten-minute install gate is claimed.

## Repository changes

- Embedded dashboard with explicit filesystem override; build order and Cargo
  packaging include generated assets.
- Native x86_64/aarch64 release workflow, tarballs, checksums and installer.
- One-shot audit controls, durable cycle modes, planning-only connection checks,
  proposal filtering and usage attribution without task dispatch.
- Hardened systemd unit, bounded failed-authentication backoff and listener warning.
- Private security/conduct reporting at mail@mail.tyk.sh, three-business-day
  acknowledgement target, threat model, contribution and community files.
- Operator checklist moved from todo.md to [operations](operations.md).

## Repository validation

Validated locally on 2026-09-08 using Rust 1.98.0 and Node 24.20.0 on x86_64
Ubuntu 26.04.1 (the development host, not the dedicated owner VM):

| Check | Result |
| --- | --- |
| `make check` | Rust formatting/clippy, Svelte/TypeScript, Prettier and dashboard build pass. |
| `make test` | 21 Rust tests, 21 deterministic integration scenarios, distribution checks and four desktop/mobile Playwright tests pass. |
| `make audit` | cargo-audit 0.22.2 and npm audit report no vulnerabilities. |
| ShellCheck and Actionlint 1.7.12 | Installer, packaging scripts and workflow definitions pass. |
| `sudo python3 tests/systemd.py` | Fixture unit syntax, allowed writes, denied protected writes and control-group child cleanup pass on this host. |
| Configuration and documentation | Printed defaults match the checked-in example; local document links resolve. |
| Production packaging | `make package` and release-archive HTTP/installer tests pass on x86_64. |
| Cargo packaging | Extracted application crate builds and serves with no external assets or Node on PATH. A clean-source snapshot passes the release wrapper. |
| Crate input guards | Real Cargo fixtures verify generated-asset inclusion and reject changed source or unexpected ignored files. |

Fixture screenshots remain under ignored browser-test output and are not live
media. The systemd test substitutes fixture paths/user/executable; it does not
commission the real service. Native hosted ARM, Ubuntu 24.04 CI and public-download
results require actual workflow/owner evidence and remain pending.

## Owner acceptance and publication

- [ ] Week 1: five consecutive real operating days, no unexplained blocked tasks,
  three owner-merged PRs, measured usage/cost evidence and real screenshots.
- [ ] Public-name/employer clearance and owner-supplied LICENSE/NOTICE facts.
- [ ] Confirm private security/conduct mail delivery and the response commitment.
- [ ] Publish a cleared GitHub release; verify both assets and checksums download.
- [ ] Publish the crates.io package and verify installation from the registry.
- [ ] Apply labels, confirm CODEOWNERS access and enable Discussions.
- [ ] Replace the labeled synthetic README image with reviewed real media; add GIF.
- [ ] Rehearse the README on a fresh Ubuntu 24.04 VM. Record OS/architecture,
  revision/release, start/end times, dependency and login time, first running cycle
  ID, every detour, and private evidence references. Do not subtract setup time to
  manufacture the ten-minute result.
- [ ] Validate the hardened service environment on the owner's actual VM, including
  tool PATH, writable checkout/caches and cancellation/restart cleanup.

Runner seam is not implemented: the full Week 1 gate must be evidenced by
September 14 to admit it. Otherwise it is the first post-launch milestone.
Optional Sigstore signing is deferred. Shipped routes, timeouts and cadence stay
unchanged pending measurements. See [distribution handoff](distribution.md).
