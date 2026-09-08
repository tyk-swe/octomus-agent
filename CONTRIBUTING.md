# Contributing

Octomus discovers useful work and delivers reviewed PRs for a single operator and
repository. Start with a concrete problem, reproduction or proposal; a justified
no-change outcome is welcome. Follow our [code of conduct](CODE_OF_CONDUCT.md).
Report security issues privately using [SECURITY.md](SECURITY.md).

## Development

Use Linux, Rust 1.88+ with rustfmt/clippy, a C compiler, Node 22.12+, npm, Python 3
and Git. Codex/GitHub credentials are not needed for fixture tests.

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

`tests/e2e.py` runs the real service, SQLite and local Git with deterministic
Codex/GitHub peers in temporary directories. It covers discovery, reviews,
repairs, publication recovery and audits without model calls or network writes.
`tests/distribution.py` checks the executable and installer using local release
fixtures. `tests/crate_guards.py` checks release input guards with real Cargo,
and `tests/crate.py` verifies the extracted application crate after packaging.
Browser tests use clearly synthetic data; their screenshots are not
live operating evidence. `tests/systemd.py` requires root on a disposable systemd
VM and exercises the unit's write restrictions and child cleanup.

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
raw transcripts or private billing images. Codex-created branches use `tyk/`.
License/NOTICE ownership changes await owner-cleared facts.

Backend requests are welcome; Codex is currently the only backend.
