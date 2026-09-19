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

## Progress record

```text
Milestone: M1
Status: TODO
Implementation revision: Not started.
Delivered output: None.
Acceptance tests and commands: Not run.
Results: No acceptance evidence recorded.
Intentional behavior differences: None approved.
Unrun required checks and blockers: All acceptance checks unrun; depends on M0.
Next eligible milestone: M2 and M3 after M1 is DONE.
```
