# Contributing

Octomus discovers useful work and delivers reviewed PRs for a single operator and
repository. Start with a concrete problem, reproduction or proposal; a justified
no-change outcome is welcome. Report security issues privately using
[SECURITY.md](SECURITY.md).

## Development

Use Linux, Go (per `go.mod`), Node 22.12+, npm, Python 3 and Git; the race
detector additionally needs a C compiler. Codex, OpenCode and GitHub credentials
are not needed for fixture tests.

```bash
npm ci --prefix web
npx --prefix web playwright install --with-deps chromium
make check
make test
```

The dashboard must be built before compiling Go because it is embedded in the
executable. Make targets handle this order. For focused Go work, first run
`npm run build --prefix web`, then `go test ./...`. After UI edits rebuild
the dashboard and binary so browser tests exercise current assets.

`make build` creates the production executable at `bin/octomus-agent`, and
`make build-race` produces a race-instrumented variant. `--assets web/build`
explicitly serves a development dashboard instead of the embedded copy. The
application version lives in the root `VERSION` file; change it in one place.
See [architecture](docs/architecture.md) and [AGENTS.md](AGENTS.md) for the code map.

## Meaningful evidence

`tests/e2e.py`, `tests/e2e_runners.py` and `tests/e2e_hardening.py` run the real
service, SQLite and local Git with deterministic Codex, OpenCode and GitHub peers
in temporary directories. They cover both runner protocols, discovery, reviews,
repairs, publication recovery and audits without live model calls or network writes.
`tests/distribution.py` checks the executable and installer using local release
fixtures. `tests/package_guards.py` checks `scripts/package.sh` rejection cases
and the release archive allowlist.
Browser tests use clearly synthetic data; their screenshots are not
live operating evidence. `tests/evidence_snapshot.py` runs the documented SQLite backup
and `--export-run` examples on a synthetic database, and the showcase tests
(`npm run showcase:test --prefix web`) cover the public showcase's input contract and
rendering. Public-site tests (`npm run site:test --prefix web`) cover homepage and docs
navigation, local search, code copying, accessibility, links, and nested 404 pages at the
domain root and legacy Pages subdirectory. `tests/systemd.py` requires root on a disposable systemd
VM and exercises the unit's write restrictions and child cleanup.

Three checks run in CI rather than in `make test`, because each needs something a
working copy does not have: `make package` followed by `tests/distribution.py
--package`, which needs a real release build; `tests/systemd.py`, which needs
root; and `shellcheck`. Run any of them locally before changing packaging, the
installer or the unit file.

Dashboard regressions cover configuration drafts in tab memory, saved-configuration
checks, keyboard navigation and list recovery. Keep drafts across view changes,
clear them at session boundaries, and use entered executable paths for catalogs.
The dashboard browser tests include axe WCAG A/AA and overflow checks. To refresh
`docs/dashboard.png` and the Product Hunt gallery, build the current dashboard and binary,
then run `npm run launch:assets --prefix web`. The capture script starts temporary
fixture servers, blocks dashboard writes, and labels the screenshots as synthetic.
Inspect the generated images before committing them; see the
[launch guide](docs/product-hunt.md#build-verify-and-publish) for the complete commands.

The public docs are generated from the allowlisted `docs/*.md` sources in
`web/scripts/site-docs.mjs`. Run `npm run site:build --prefix web` after editing a guide.
The live homepage, documentation, and sample use `https://octomus-agent.tyk.sh/`;
publishing them requires a separate `npm run site:deploy --prefix web` after checks pass.

Run relevant behavior tests while editing and full `make check`/`make test` before
delivery. `make audit` runs govulncheck and `npm audit` for dependency
advisories; it needs module download access. CI retains browser failure traces.

## A good pull request

Explain the concrete problem, resulting behavior, validation and limitations.
Keep changes cohesive and preserve full-diff review, fresh reviewer threads,
persistent repair threads, exact model routes and verification before publication.
Add behavior tests for meaningful changes; avoid tests that merely duplicate code.
Keep Go/dashboard types and documentation aligned. Saved configuration and task
snapshots must still load after upgrades.

Workers must not push or publish; the orchestrator owns publication. Do not merge, deploy,
migrate production systems, edit live `.octomus/` state, or commit credentials,
raw transcripts or private billing images. Octomus-created branches use `tyk/`.
License/NOTICE ownership changes await owner-cleared facts.

Requests for additional backends are welcome; Codex and OpenCode are the supported
backends.
