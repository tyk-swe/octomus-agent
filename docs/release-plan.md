# Octomus Agent: thirty days to public

Prepared Tuesday 2026-09-08. Public launch Tuesday 2026-10-06, Show HN at 13:00 UTC (22:00 KST).

Week 0 decisions and research are recorded in [the decision record](week-0.md), with a [launch issue draft](launch-issue.md) ready for the owner to post. Scope, launch timing, reply hours, and metrics are adopted. Employer clearance, public-name clearance, reservations, bot setup, calendar booking, and leave remain pending; Week 0 is not yet complete.

This plan takes the repository from "MVP with passing fixtures" to a public release that a stranger can install in ten minutes, that survives a hostile Hacker News thread, and that arrives with proof it works. The order matters: truth first, then installability and trust, then story, then a freeze.

## 1. Where it stands today

Baseline verified on 2026-09-08 before Week 1 preparation; current preparation checks are recorded in [Week 1](week-1.md#repository-validation):

| Area | State |
| --- | --- |
| Rust crate | Builds on stable 1.94 (rust-version 1.88). `cargo fmt`, `cargo clippy -D warnings` clean. 6 tests pass. |
| Dashboard | `svelte-check` 0 errors, Prettier clean, static build succeeds. Playwright suite exists (not run here). |
| Integration | All 11 scenarios in `tests/e2e.py` pass with deterministic Codex/GitHub fixtures and real Git. |
| Size | ~5,000 lines: 2,900 Rust, 1,500 Svelte/TS, 400 Python fixtures. 157 crates in the lockfile. `npm audit` clean. |
| Docs | README, PRD, architecture, deployment, acceptance map, config example, systemd unit. |
| License | Apache-2.0, no copyright holder or NOTICE file yet. |

What is missing for a public release:

- **Live proof.** The launch checklist still says "validate the first real cycle". No cycle has run against a real Codex runtime or a real GitHub repository.
- **Route reality.** Tier defaults (`gpt-5.6-luna` xhigh/max, `gpt-6-astra` low/medium/high) and the configurable repair default (`gpt-6-astra` medium) have not been checked against a real model catalog. If any pair is absent, `Check connection` fails for every first-time user.
- **AGENTS.md — prepared.** Repository structure, verification commands, conventions and worker boundaries are now recorded; live grounding remains unvalidated.
- **Distribution.** Build-from-source only: Rust 1.88, Node 22, npm, Python 3, Codex CLI, gh. No tagged release, no binaries, no install script.
- **Community files.** No SECURITY.md, CONTRIBUTING.md, CODE_OF_CONDUCT.md, CHANGELOG.md, issue or PR templates, CODEOWNERS, Discussions.
- **Cost story.** A cycle spends 13 sessions (1 grounding + 9 discovery + 2 adversarial + 1 consolidation) before any task runs. Each task admits 2 to 10 more without retries (one executor, up to five reviews and four repairs). At the default 30-minute interval, idle cycles alone reach the daily budget of 150. Nobody can tell what a day costs.
- **Security narrative.** The design is deliberately unsandboxed. That is defensible, but only if the threat model is written before the comment thread writes it for you.

## 2. What "big" realistically means

OpenClaw got to six-figure stars because it was consumer-facing, ran on a laptop, plugged into chat apps everyone already had, and had a mascot people could meme. It then spent months on security fallout: exposed instances, prompt-injection incidents, malicious community skills. Two lessons transfer directly. First, lower the barrier to a first run as far as the product allows. Second, own the security story before launch, because an unsandboxed agent with push rights is exactly the kind of thing that thread will focus on.

Octomus is a developer tool that needs a paid Codex account, a dedicated Linux VM, and real money per day. The ceiling is a top dev-tool launch, not a consumer wave. Aim for that and hit it:

| Metric | Target by 2026-10-09 (72 h) | Target by 2026-10-20 |
| --- | --- | --- |
| Hacker News | Front page for 6+ hours, 200+ comments | Follow-up post lands |
| GitHub stars | 1,500 | 4,000 |
| External operators with a merged Octomus PR | 3 (from beta) | 10 |
| Issues from strangers | 20 | 60 |
| Media | This Week in Rust, GeekNews front page | Changelog/Console/TLDR mention |

The pitch line already exists in the PRD: **"Dependabot, with features."** Every other coding agent in 2026 (Copilot coding agent, Jules, Codex cloud, Devin, Cursor background agents) waits for you to write the task. Octomus finds the task, argues about it with two adversaries, and is allowed to conclude there is nothing worth doing. That "it says no" behaviour is the differentiator and the best material for the launch post.

Three product moves widen the funnel more than any marketing:

1. **Single binary.** Embed the built dashboard in the executable so install is one file, no Node at runtime.
2. **Audit mode.** Run one grounding, discovery, and challenge pass without executing anything, and show the accepted/rejected proposals with reasons. This costs 13 sessions once, needs no verification commands, and produces the most shareable artifact the product has: "here is what Octomus thinks of my repo."
3. **A runner seam, conditional on Week 1.** Codex app-server stays the only backend at v0.1.0. If Week 1 meets its exit criteria by Sep 14, extract the boundary the engine calls (`connect`, `models`, `start`, `turn`) into a trait so a second backend is a contribution-sized task. Otherwise this is the first post-launch milestone. Launch answers about other backends must reflect whether the trait actually shipped.

## 3. Calendar

| Phase | Dates | Theme | Exit criterion |
| --- | --- | --- | --- |
| Week 0 | Tue Sep 8 | Decisions | Employer clearance, name/reservations, bot setup, date/hours and bookings, scope, metrics resolved |
| Week 1 | Sep 9 to Sep 14 | Make it true | 5 consecutive days of real cycles on this repo, 3 Octomus PRs merged, daily usage and subscription/charge evidence recorded |
| Week 2 | Sep 15 to Sep 21 | Make it installable and trustworthy | Fresh Ubuntu 24.04 VM to first cycle in 10 minutes using only the README |
| Week 3 | Sep 22 to Sep 28 | Make it legible, private beta | rc.1 running at 3+ external operators, all launch copy and video done |
| Week 4 | Sep 29 to Oct 5 | Freeze and rehearse | v0.1.0 tagged, repo public, full install rehearsed from the public release |
| Launch | Tue Oct 6 | Ship | Show HN posted 13:00 UTC; target replies within 2 hours during 13:00–21:00 UTC coverage; resume Oct 7 at 01:00 UTC |
| Follow-through | Oct 7 to Oct 20 | Keep it alive | v0.1.1 within 72 h, follow-up post on day 7 |

The [official 2026 calendar announcement](https://www.kasi.re.kr/kor/post/newsMaterial/32031) lists the Chuseok break as Sep 24 to 27, including Sunday; Sep 28 is not part of that announced holiday break. Use that four-day block inside Week 3 for the video, the post, and the landing page, with Sep 28 available for follow-up work subject to the owner's schedule. Mon Oct 5 is a substitute holiday for National Foundation Day and remains the rehearsal day.

## 4. Week 0, Tuesday Sep 8: decisions

The owner adopted the decisions below on 2026-09-08. The [Week 0 decision record](week-0.md) contains dated research, source links, and the remaining owner checklist. This work covers documents and research only; external setup remains pending.

- **Employer clearance — pending.** Confirm the open-source/IP policy allows publishing, the exact copyright holder, and any required disclaimer. Defer LICENSE/NOTICE changes until these facts are supplied.
- **Name — provisional.** Keep Octomus and the existing repository/package names. Research found an existing collaboration platform, an occupied GitHub username, and a registered `octomus.dev`; other availability and trademark clearance remain unresolved. The owner must resolve public-name clearance and reservations; nothing has been reserved by this work.
- **Launch date and hours — adopted; bookings pending.** Tue Oct 6, Show HN at 13:00 UTC / 22:00 KST. Cover replies until Wed Oct 7 at 06:00 KST, rest until 10:00 KST, then resume. Calendar booking and Oct 7 leave are owner actions still pending.
- **Scope of v0.1.0 — adopted.** Codex-only backend. Single binary with embedded dashboard and audit mode in. Runner seam in only if Week 1 meets all exit criteria by Sep 14, otherwise the first post-launch milestone.
- **Bot identity — pending.** A dedicated identity should visibly author PRs; `octomus-bot` is an unreserved candidate. A separate bot added as a repository collaborator cannot simply use the proposed fine-grained token under [GitHub's documented limitations](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens#fine-grained-personal-access-tokens-limitations). Resolve a supported repository-restricted authentication arrangement before setup; see the decision record.
- **Metrics — adopted.** Preserve every target in section 2. They are copied verbatim into the [launch issue draft](launch-issue.md), with blank actual-results fields. The issue has not been posted.

## 5. Week 1, Sep 9 to 14: make it true

The README says "when the project builds itself." Week 1 makes that sentence true and produces the numbers the launch post needs.

**Implementation status:** repository preparation is implemented: configurable repair routes, configured-tier prompts, CLI version diagnostics, durable admission accounting, read-only usage reports and `AGENTS.md`. The [commissioning record](week-1.md) and [cost methodology](cost.md) are ready. Live commissioning, five consecutive days, three owner-merged PRs, measured usage and screenshots remain pending. No exit gate is complete.

**Owner decisions for execution:** the owner provisions the dedicated VM, supplies the dedicated bot with repository-restricted authentication, and performs login and PR review/merges. Use existing Codex subscription allowance only; never enable paid overage. The cost gate accepts real admissions, allowance consumption where observable, subscription fee and verified incremental charges. Per-task dollars unavailable from the provider must be marked unavailable, not estimated or presented as zero. If commissioning slips, record the actual five-day window; the September 14 runner-seam deadline stays unchanged.

**Day 1: live validation.**

- Provision a dedicated VM on the Proxmox host. Nothing else on it: no other credentials, no access to the workstation's GPUs or storage, egress limited to GitHub, OpenAI, and package registries.
- Install Git, gh, Codex CLI, Rust, Node, Python. Run `codex login` and `gh auth login` as the `octomus` service account.
- Run `octomus-agent --doctor`. Confirm the model catalog contains every default route with its exact effort. Repair is now configurable. If a route is missing, configure an available route explicitly before running; change shipped defaults only from live catalog evidence. This is the highest-probability first-run failure in the whole plan.

**Days 1 to 6: dogfood on this repository.**

- Point Octomus at `tyk-swe/octomus-agent` with prefix `tyk/` and the verification commands from the launch checklist. Branches created by Codex also use `tyk/`. Start with one concurrent task, one task per cycle, cycle interval 6 hours.
- Each morning: read every proposal decision and reason, merge the PRs that deserve it, cancel and note the ones that do not. Keep a log: cycle wall time, sessions used, blocked reasons, subscription allowance observations and verified incremental charges (from the actual account’s usage/billing evidence), proposals accepted vs rejected, PRs merged.
- Fix what breaks. Expected breakage, in order of likelihood: app-server protocol drift against the pinned 0.153.4 semantics; an interactive request from Codex that blocks a task; `command_timeout_seconds` 600 exceeded by a cold `cargo clippy` in a fresh clone (every task and every discovery agent is a full clone, so consider a shared `CARGO_TARGET_DIR` or sccache in the ops doc); `session_timeout_seconds` 1800 too short for an L or XL executor; disk growth from 9 discovery clones per cycle.
- Add `AGENTS.md` describing the repository for agents (structure, build, test, conventions, what not to touch). Octomus reads it on every grounding pass, and its presence tells visitors the project is agent-native.
- Fill `docs/cost.md` from the log: admissions and available billing evidence per idle cycle, per observed task tier/route, and per day at the shipped defaults. Include sample counts, subscription fee and unavailable attribution explicitly. Retune defaults from the data (cycle interval, timeouts, tasks per cycle) so a first-time user's first day costs what the README says it costs.

**Exit criteria.** Five consecutive days of real cycles with no unexplained blocked task. At least three Octomus PRs merged into `main` by you. A usage/cost table with real measurements under the subscription criterion above. Screenshots of real cycles, saved for Week 3.

## 6. Week 2, Sep 15 to 21: make it installable and trustworthy

**Distribution.**

- Embed `web/build` into the binary (`rust-embed` or `include_dir`) so the release is one file. Keep `--assets` as an override for development.
- Release workflow on tag: build `x86_64-unknown-linux-gnu` and `aarch64-unknown-linux-gnu`, produce tarballs with SHA-256 sums, attach to a GitHub Release with generated notes. Sign with Sigstore if the time is there; checksums are the floor.
- `install.sh`: detects arch, downloads the release, verifies the checksum, installs to `/usr/local/bin`, prints the three next commands. Support `cargo install octomus-agent` by publishing to crates.io with full metadata (repository, homepage, keywords, categories).
- Add the systemd hardening the unit currently lacks: `NoNewPrivileges=yes`, `ProtectSystem=strict` with `ReadWritePaths` for the state directory and target checkout, `PrivateTmp=yes`, `ProtectKernelTunables`, `RestrictSUIDSGID`. Keep `KillMode=control-group`.

**Security posture, written before anyone asks.**

- `SECURITY.md`: private disclosure address, response time, supported versions.
- `docs/threat-model.md`: trust boundaries; what an agent can do (everything the service user can); prompt injection through repository content is in scope and not mitigated by design; the operator token is a capability with no rate limit and must stay behind an SSH tunnel; the dashboard is single-operator; what the `redact` filter does and does not guarantee. State plainly that the sandbox is the VM, and that a process sandbox would not change what a push-capable agent can do to the repository.
- Add a small backoff on repeated 401s to the API, and a startup warning if `--listen` is not loopback.
- CI additions: `cargo audit` (or `cargo deny`), `npm audit --audit-level=high`, and the Playwright suite already in the Makefile.

**Product moves.**

- Audit mode: a control that runs grounding, discovery, and challenge, records the cycle with every decision and reason, and queues nothing. Dashboard: a "Run an audit" button beside "Run a cycle", and a proposals view that reads well as a screenshot.
- Runner seam, only if Week 1 met all exit criteria by Sep 14: extract the four calls the engine makes into a trait with the Codex implementation behind it. Otherwise defer to the first post-launch milestone. No second backend yet.

**Community files and docs.**

- `CONTRIBUTING.md` (dev setup, `make check`, `make test`, how fixtures work, what a good PR looks like), `CODE_OF_CONDUCT.md` (Contributor Covenant), `CHANGELOG.md` (Keep a Changelog format), `CODEOWNERS`, issue templates (bug, blocked task with redacted task JSON, backend request, proposal quality report), a PR template, labels including `good first issue` and `backend`. Enable Discussions.
- README rewrite for a reader's first ten minutes: one paragraph of what and why, the 10-second GIF, install in one command, first cycle, how it decides (the adversarial loop and the fresh-reviewer vs persistent-repair rule), what a day costs, security in three sentences with a link to the threat model, roadmap, then everything else behind links. Replace the synthetic screenshot with a real one.
- Move the operator checklist from `todo.md` into `docs/operations.md` written for an operator, and delete `todo.md`. It currently reads as a note to the author.

**Exit criterion.** A fresh Ubuntu 24.04 VM reaches its first running cycle within 10 minutes following only the README, with no step that says "see the source".

## 7. Week 3, Sep 22 to 28: make it legible, private beta

**Private beta, Tue Sep 22.**

- Tag `v0.1.0-rc.1`. Invite 5 to 10 operators: Rust people you trust, colleagues if policy allows, two or three from the Korean developer community. Give them the release, the README, and one ask: report time-to-first-cycle, the first blocked task, and the first PR they merged.
- Fix beta findings as they arrive. Week 3 is a bugfix week as much as an assets week.
- Ask each operator for one sentence you may quote, and a link to a merged PR you may cite.

**Assets, Chuseok block Sep 24 to 27; follow-up Sep 28.**

- **Demo video, 75 to 90 seconds.** Install command, dashboard opens paused, configure, run an audit, scroll the proposals with rejection reasons, run a cycle, land on the PR in GitHub with verification evidence in the body. Cut a 10-second GIF for the README and social.
- **Launch post.** Title candidate: "Octomus: a repository that files its own pull requests". Structure: the problem (task-driven agents need a task), the loop (9 discovery agents, 2 adversaries, consolidation), why the reviewer is always fresh and the repair thread is persistent, why delivery stops at a PR, why the host is the sandbox, real numbers from Week 1 (proposals seen, accepted, merged, cost per day), and the best rejected proposals with their reasons. The rejections will be the most-quoted part.
- **Landing page.** One page on GitHub Pages or the domain, reusing the dashboard's visual identity. Above the fold: the sentence, the GIF, the install command. Below: the loop, the numbers, the security paragraph, the GitHub link.
- **Show HN copy.** Title: "Show HN: Octomus – Dependabot, but for features. An agent that finds, challenges, and ships PRs". First comment, written by you: what it is, what it is not (no merge, no sandbox, Codex-only today), what a day costs, the three questions you most want feedback on.
- **Channel copy.** X thread of 6 to 8 posts with the GIF. GeekNews post in Korean with a Korean summary of the loop. Reddit r/rust ("Rust + Codex app-server, 3k lines, 11 integration scenarios") and r/ChatGPTCoding. This Week in Rust submission. Console.dev, Changelog News, and TLDR tips. Lobsters if you have an invite.

**Exit criteria.** rc.1 running at three or more external operators. Video cut, post drafted and reviewed by two people, landing page live on a private URL, all channel copy in one document.

## 8. Week 4, Sep 29 to Oct 5: freeze and rehearse

- **Code freeze Tue Sep 29.** Only beta bugfixes merge. Every merge goes through the full `make check` and `make test`.
- **Repository public Thu Oct 1, quietly.** No announcements. This lets links resolve, lets you see the README as a stranger does, and means star velocity on launch day is measured from a live repository rather than a brand-new one.
- **Tag `v0.1.0` Mon Oct 5.** Release notes written by hand, not generated. Confirm binaries, checksums, and the install script from the public URL.
- **Full rehearsal on Oct 5.** Fresh VM, public install script, first audit, first cycle, first PR. Fix any friction and re-tag if needed.
- **Prepared answers.** Write the reply to each of these before launch so you are pasting, not composing, at 02:00 KST:
  1. "Unsandboxed means prompt injection is remote code execution." Yes, by design; the VM is the boundary; here is the threat model; a process sandbox would not change what a push-capable agent can do.
  2. "Why Codex only?" It is what shipped and was tested; link the runner trait if it shipped, otherwise its first post-launch milestone, along with the issue for the next backend.
  3. "What does it cost?" The table from `docs/cost.md`.
  4. "This will drown maintainers in AI PRs." It never merges, it is paused on install, two adversaries reject weak ideas, idle is a valid cycle outcome, and every rejection reason is recorded.
  5. "Why Rust?" One long-lived process, process-group ownership of every child, SQLite with full-sync WAL, one binary.
  6. "How is this different from Copilot coding agent / Jules / Codex cloud / Devin?" They need a task; Octomus finds it, challenges it, and can say no.
  7. "Single operator, single repo?" Yes at v0.1; multi-repo is on the roadmap.
  8. "How do I stop it?" Pause, cancel task, `systemctl stop`; the unit kills the whole control group.
  9. "Does it train on my code?" It uses your own Codex account under its terms; Octomus stores nothing off-host.
  10. "License?" Apache-2.0.
- **Ops readiness.** Labels and a triage board ready. Saved replies for the five most likely issue shapes. A `docs/known-limitations.md` linked from the README. Notifications on for the repo, HN, and X.

## 9. Launch day, Tuesday Oct 6

| KST | UTC | Action |
| --- | --- | --- |
| 18:00 | 09:00 | Final check: release assets download, install script runs, landing page and README links resolve, `Check connection` passes on the demo VM. |
| 20:00 | 11:00 | Publish the launch post on the landing page or blog. Push the README GIF. |
| 21:30 | 12:30 | Post GeekNews (Korean evening traffic). |
| 22:00 | 13:00 | Show HN. Post the first comment immediately. Post the X thread linking the HN thread. |
| 22:00 Tue to 06:00 Wed | 13:00 to 21:00 Tue | Target replies to every HN comment and GitHub issue within 2 hours during this shift. Fix trivial README issues live. Do not push code. |
| 23:00 | 14:00 | Reddit r/rust and r/ChatGPTCoding. |
| 06:00 Wed | 21:00 Tue | Sleep until 10:00 KST; replies pause during this four-hour rest gap. |
| 10:00 Wed | 01:00 Wed | Second shift: handle the overnight backlog on HN, issues, Discussions. Submit This Week in Rust. Send Console/Changelog/TLDR tips with the HN link. |

## 10. Follow-through, Oct 7 to 20

- **v0.1.1 within 72 hours** with the top three fixes from the thread. A release three days after launch is the strongest signal a project is alive.
- **Day 7 post**: what the thread taught you, the numbers, what changed. Post it to HN as a normal submission, not a Show HN.
- **Open the flagship issues on launch day** so contributors have somewhere to go: runner seam first if deferred, second backend dependent on that trait, multi-repository support, audit mode as a GitHub Action or comment, a Docker image for the service (the PRD excludes Docker as a sandbox, not as packaging).
- **Recruit one co-maintainer** from the beta or the thread by Oct 20. Give them triage rights first.

## 11. Risks

| Risk | Likelihood | Impact | Mitigation |
| --- | --- | --- | --- |
| Default routes or the configured repair route do not exist in the public Codex catalog | High | Every first run fails | Day 1 of Week 1 verifies the catalog; repair route is configurable; validate it explicitly; defaults follow what the catalog offers |
| Cost shock from 13 sessions per cycle at a 30-minute interval | High | Bad first-day story, refunds, angry posts | Ship defaults from measured data; publish `docs/cost.md`; audit mode as the cheap first run |
| Security backlash, as with OpenClaw | High | The thread becomes about risk, not value | Threat model and hardened unit ship first; README leads with "dedicated VM"; no "it is safe" claims anywhere |
| "AI slop PRs" backlash | Medium | Reputation with maintainers | PR-only, paused by default, rejections public, merge rate published |
| Codex app-server protocol drift on a new CLI release | Medium | Silent breakage for new installs | Pin the tested Codex version in docs and in `--doctor`, with a clear message on mismatch |
| Single-maintainer bandwidth in launch week | High | Slow replies, stalled fixes | Freeze in Week 4, days off booked, prepared answers, co-maintainer recruited |
| Name or trademark conflict | Unresolved; existing adjacent brand use found | Forced rename after launch | Keep name provisional; complete owner name review and reservations recorded in Week 0 |
| Employer policy | Unresolved | Cannot publish | Owner clearance, copyright holder, and disclaimer remain pending in Week 0 |

## 12. Cut order if behind

If Week 1 slips, cut in this order and keep the launch date: runner seam, Sigstore signing, crates.io publish, aarch64 build, landing page (README plus the GIF is enough), Lobsters and secondary channels. Never cut: live validation, cost table, threat model, single binary, the video, prepared answers.
