# Contributing

Octomus discovers useful work and delivers reviewed PRs for a single operator and
repository. Start with a concrete problem, reproduction or proposal; a justified
no-change outcome is welcome. Follow our [code of conduct](CODE_OF_CONDUCT.md).
Report security issues privately using [SECURITY.md](SECURITY.md).

## Development

Use Linux, Rust 1.88+ with rustfmt/clippy, a C compiler, Node 22.12+, npm, Python 3
and Git. Codex, OpenCode and GitHub credentials are not needed for fixture tests.

```bash
npm ci --prefix web
npx --prefix web playwright install --with-deps chromium
make check
make test
```

The dashboard must be built before compiling Rust because it is embedded in the
executable. Make targets handle this order. For focused Rust work, first run
`npm run build --prefix web`, then `cargo test --locked`. After UI edits rebuild
the dashboard and Rust binary so browser tests exercise current assets.

`make build` creates the production executable. `--assets web/build` explicitly
serves a development dashboard instead of the embedded copy. See
[architecture](docs/architecture.md) and [AGENTS.md](AGENTS.md) for the code map.

## Meaningful evidence

`tests/e2e.py`, `tests/e2e_runners.py` and `tests/e2e_hardening.py` run the real
service, SQLite and local Git with deterministic Codex, OpenCode and GitHub peers
in temporary directories. They cover both runner protocols, discovery, reviews,
repairs, publication recovery and audits without live model calls or network writes.
`tests/distribution.py` checks the executable and installer using local release
fixtures. `tests/crate_guards.py` checks release input guards with real Cargo,
and `tests/crate.py` verifies the extracted application crate after packaging.
Browser tests use clearly synthetic data; their screenshots are not
live operating evidence. `tests/evidence_snapshot.py` runs the documented SQLite backup
and `--export-run` examples on a synthetic database, and the showcase tests
(`npm run showcase:test --prefix web`) cover the public showcase's input contract and
rendering. `tests/systemd.py` requires root on a disposable systemd
VM and exercises the unit's write restrictions and child cleanup.

Dashboard regressions cover configuration drafts in tab memory, saved-configuration
checks, keyboard navigation and list recovery. Keep drafts across view changes,
clear them at session boundaries, and use entered executable paths for catalogs.
To refresh synthetic presentation captures after rebuilding, run
`npm test --prefix web -- captures.spec.ts --project=desktop`. Inspect the overview,
Configuration and evidence panels at 1440×1000, 1280×800 and 390×844 in
`web/artifacts/captures/after/`. The tests check contrast, overflow and reduced motion.
Copy the inspected `synthetic-1440x1000-overview.png` to `docs/dashboard.png` when
updating the README image; retain its synthetic-data caption.

Run relevant behavior tests while editing and full `make check`/`make test` before
delivery. Install `cargo-audit` with `cargo install cargo-audit --locked`, then run
`make audit` for dependency advisories. CI retains browser failure traces.

## A good pull request

Explain the concrete problem, resulting behavior, validation and limitations.
Keep changes cohesive and preserve full-diff review, fresh reviewer threads,
persistent repair threads, exact model routes and verification before publication.
Add behavior tests for meaningful changes; avoid tests that merely duplicate code.
Keep Rust/dashboard types and documentation aligned. Saved configuration and task
snapshots must still load after upgrades.

Workers must not push or publish; Rust owns publication. Do not merge, deploy,
migrate production systems, edit live `.octomus/` state, or commit credentials,
raw transcripts or private billing images. Octomus-created branches use `tyk/`.
License/NOTICE ownership changes await owner-cleared facts.

Backend requests are welcome; Codex and OpenCode are the supported backends.
