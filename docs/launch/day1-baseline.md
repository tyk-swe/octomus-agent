# Day 1 launch baseline — 2026-09-12

## Revision and scope

- Starting revision: `96e7bf07717ac8b25e43579f1529c4e49390609e` on `main`.
- Initial inventory: `git status --porcelain=v1 --untracked-files=all` returned
  no tracked changes or untracked files.
- Tested revision: the same starting commit, with no source or lockfile changes.
  This report was added during testing and finalized afterward; it was the only
  untracked file at baseline completion. Dashboard, Cargo, and browser artifacts
  are Git-ignored.
- Read `AGENTS.md` (no nested instructions found), `Makefile`, `README.md`,
  `docs/acceptance.md`, and both CI/release workflows in `.github/workflows/`.
- Local baseline only. Integration evidence uses temporary repositories and
  synthetic Codex/OpenCode/GitHub peers; it is not a real Astra cycle or PR.

## Environment

Ubuntu 26.04.1 LTS, x86_64. This differs from CI's Ubuntu 24.04 and Node 22.
Installed versions satisfy the documented minimums:

| Check command | Version | Exit |
| --- | --- | --- |
| `rustc --version` | 1.98.0 | 0 |
| `cargo --version` | 1.98.0 | 0 |
| `rustfmt --version` | 1.9.0-stable | 0 |
| `cargo clippy --version` | 0.1.98 | 0 |
| `node --version` | 24.20.0 | 0 |
| `npm --version` | 11.19.0 | 0 |
| `python3 --version` | 3.14.4 | 0 |
| `git --version` | 2.53.0 | 0 |
| `cc --version` | GCC 15.2.0 | 0 |
| `make --version` | GNU Make 4.4.1 | 0 |
| `npx --prefix web playwright --version` | 1.63.0 | 0 |

## Commands and results

Raw command logs remain outside Git in temporary storage. Both Make targets
build the dashboard before Cargo, as required.

| Command | Exit | Result |
| --- | --- | --- |
| `npm ci --prefix web` | 0 | Installed 62 packages; audit reported zero vulnerabilities. |
| `npx --prefix web playwright install chromium` | 0 | Browser prerequisite available. |
| `npx --prefix web playwright install-deps --dry-run chromium` | 0 | “All system dependencies are installed.” No privileged installation needed. |
| Chromium smoke command below | 0 | Chromium 153.0.8010.12 launched headlessly and closed. |
| `make check` | 0 | Dashboard build, Rust formatting, Clippy with warnings denied, Svelte/TypeScript (zero errors/warnings), and Prettier passed. |
| `make test` | 0 | All required components passed; details below. |

Completed `make test` components: `cargo test --locked` (59 passed, zero failed,
four ignored), `cargo build --locked`, `python3 tests/e2e.py` (25 scenarios),
`python3 tests/e2e_runners.py` (13 scenarios), `python3 tests/e2e_hardening.py`
(20 scenarios), `python3 tests/distribution.py` (embedded binary and installer),
and `python3 tests/crate_guards.py` (packaging guards) all exited 0.
`npm test --prefix web` exited 0: all 16 desktop/mobile browser tests passed
(2.5 minutes).

Browser smoke command:

```sh
node --input-type=module -e 'import {chromium} from "./web/node_modules/playwright/index.mjs"; const browser = await chromium.launch({headless:true}); console.log(`Chromium ${browser.version()}: headless launch OK`); await browser.close();'
```

## Fixes and remaining acceptance

**BASELINE READY** for the requested local checks. No environment/setup or
reproducible repository failure occurred; there is no failure excerpt to report.
No code fix or regression test was necessary, and no assertions or test selection
were changed. Each full Make target passed on its first run.

The npm install reported an advisory about esbuild's unapproved install script;
the dashboard build succeeded without changing npm settings or the lockfile.
Browser tests emitted only a harmless `NO_COLOR`/`FORCE_COLOR` precedence warning.
No missing privileged setup or network-access blocker was encountered.

The default Rust test target leaves four pre-existing tests ignored: the two
pinned-client contracts, the pinned OpenCode protocol smoke test, and the explicit
100,000-record history-scale benchmark. None is counted as a pass. Separate CI
audit, shellcheck, release packaging, crate-install, systemd, and pinned-client
jobs are not covered by these two Make targets and have not been run here.

Real-host acceptance remains owner work: on the dedicated VM with the dedicated
bot and already approved account setup, confirm exact Astra routes with **Check
connection**, then run one cycle and inspect review, verification, and PR evidence.
No live model session, remote push/publication, infrastructure provisioning,
account/credential change, licensing edit, or repository `.octomus/` edit was
performed during baseline verification.
