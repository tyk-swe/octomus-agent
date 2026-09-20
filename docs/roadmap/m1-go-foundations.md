# M1 — Establish Go foundations and serialization compatibility

[Roadmap](README.md) · **Depends on:** [M0](m0-reference-and-contracts.md) ·
**Enables:** [M2](m2-storage-and-exports.md), [M3](m3-processes-and-git.md)

## Deliverable

A buildable Go executable, configuration loading and validation, domain types,
structured-result validation, and embedded-dashboard build integration.

Port behavior rather than Rust syntax. Keep immutable snapshots explicit and
prevent shared mutable maps or slices from escaping their owners. Use the
toolchain pinned in M0 and create only packages needed by this implementation.

Preserve all existing CLI flags:

```text
--data-dir
--listen
--assets
--print-config
--doctor
--audit
--usage-report
--export-run
--help
--version
```

Commands owned by later milestones must fail explicitly until implemented; never
return fabricated success. Preserve environment handling, flag validation,
stdout/stderr separation, and exit semantics as each command becomes available.

## Acceptance criteria

1. `--print-config` matches the reference semantically, including nested defaults
   and absent-versus-explicitly-empty maps.
2. JSON fixtures cover missing fields, explicit nulls, empty arrays, optional
   fields, enum values, malformed types, invalid numeric ranges, and unknown
   fields.
3. Strictness matches at each type boundary. Do not globally reject fields the
   reference tolerates or accept unknown configuration fields it rejects.
4. Preserve legacy route defaults, missing `repair_route`, missing backend
   identity, legacy execution-mode defaults, and saved attempt-policy behavior.
5. Preserve problem-identity normalization and repository/title comparison.
   Changes to Unicode or case-folding rules require compatibility evidence.
6. Byte-sensitive identities have explicit fixtures: baseline configuration
   fingerprints, notification destination identities, and decision-memory
   fingerprints.
7. The executable embeds the real `web/build`, including `_app` assets. Production
   builds fail if dashboard output is absent; no placeholder is substituted.

## Verification and evidence

Run focused `internal/config` and `internal/model` tests, CLI fixtures, fingerprint
fixtures, and embedded-build checks. Port the corresponding inline Rust tests and
the M1 cases allocated from shared suites in M0.

The baseline fingerprint hashes serialized configuration bytes. Semantic JSON
equality is insufficient: cover field ordering, map ordering, escaping, optional
fields, and newline behavior.

## Implemented surface

The Go module pins Go 1.27.1. Rust remains the default executable, build, release,
and complete service. Migration-only commands are:

```sh
npm ci --prefix web
make build-go                 # bin/octomus-agent-go, with the real dashboard
make check-go test-go         # formatting, vet, unit/race, CLI and embed contracts
bin/octomus-agent-go --print-config
```

`go build ./cmd/octomus-agent` also works after `npm run build --prefix web`.
Compilation fails without `web/build/200.html` or the `_app` entry bundles;
`web/embed.go` uses `//go:embed all:build` so underscore-prefixed assets are not
lost. Tests compare every embedded production file with its source bytes and
exercise a relocated executable from an unrelated working directory.

`--print-config`, `--help`/`-h`, and `--version`/`-V` work. Flags, environment
overrides, conflicts, parse-error exit 2, and output streams have frozen CLI
coverage, including decimal IPv6 scope IDs through `u32::MAX` for both flags
and environment variables; named scopes remain invalid. Service startup,
doctor/audit, usage reporting and run export return
exit 1 with an explicit unimplemented-milestone diagnostic, before creating a
data directory or lock. These are deliberate M1 boundaries, not mock successes.
No runner, GitHub, database, or service operations are implemented by this gate.

`internal/config` and `internal/model` retain declaration-order serialization,
owned `Clone` snapshots, route pointers, legacy defaults, and explicit attempt
policies. `internal/jsoncompat` handles the actual serde boundary differences:
required fields, explicit nulls, defaulted fields, exact field-name matching,
duplicate known keys, per-record unknown fields, positional records, unit enum
objects, Unicode escapes, integer bounds, and opaque numeric evidence.
`internal/schemas` separately validates strict new runner output; permissive
saved-record loading never loosens the proposal/review schemas.

### Acceptance coverage

| Contract | Executable evidence |
| --- | --- |
| Defaults, exact routes, audit/baseline readiness, bounds and branches | `internal/config/config_test.go`, `wire_test.go`; frozen `cli.json` print output |
| All configuration/domain JSON records and enums; legacy snapshots and actions | Rust `tests/compatibility.rs` oracle and Go config/model `TestFrozenWireContracts` |
| Structured proposal/review results and malformed schemas | `internal/schemas/TestFrozenStructuredResults` |
| Unicode problem keys, trimmed ASCII titles and repository/path comparison | Config/model identity fixtures and `TestFrozenSameWorkComparisons` |
| Baseline, notification and decision-memory byte identities | `m1.json` hashes; config/model wire and identity tests |
| Snapshot ownership and local domain behavior | Config/model ownership, attempt-policy, session, UTC timestamp and mode tests |
| CLI boundaries, real embedded assets, missing-build failures | `cmd/octomus-agent`, `web/embed_test.go`, `tests/go_foundations.py` |

The M1 portions of shared Rust suites are allocated explicitly in M0: legacy
repair/audit configuration and cycle mode (`core`), saved mode/attempt policies
and allowed actions (`hardening`), and pure ASCII/Unicode comparison behavior
(`review_regressions`). Catalogs, scheduling, indexed database lookup and API
behavior remain assigned to their later milestones. The historical serialized
`branch_prefix: "octomus/"` default is retained for parity; this milestone does
not create any branches.

## Progress record

```text
Milestone: M1
Status: DONE
Implementation revision: Working tree based on 3c2b5cd50924033873d7f740f9df44daee5685db.
Delivered output: Go CLI, configuration/domain records and behavior, strict structured-result validation, exact identities, owned snapshots, real dashboard embedding, Rust-derived fixtures and migration CI/Make targets.
Acceptance tests and commands: make check-go test-go; cargo test --locked --test compatibility; Rust-default and Go-selected tests/go_foundations.py; reference checks and all full-suite components recorded in M0.
Results: PASS. 1421 Rust-derived wire cases, including signed-zero rejection in integer fields, 115 structured-result cases, 79 CLI cases, exact identity fixtures, ownership/domain tests, real embedded-file/binary checks and absent-asset build failures. gofmt, go vet ./..., go test ./..., and CGO_ENABLED=1 go test -race ./... all passed. Go executable built with CGO_ENABLED=0.
Intentional behavior differences: None approved.
Unrun required checks and blockers: None for M1. Later service/database/runner commands explicitly fail as required by this milestone; Rust remains the default.
Next eligible milestone: M2 and M3.
```
