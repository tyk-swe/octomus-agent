# Octomus: GPT-6 Astra Challenge launch milestones

**Goal:** give judges and prospective users a useful result they can inspect,
a clear demonstration, and an eligible contest entry.

**Launch target:** September 16, 2026. Keep September 17 available for corrections
before the advertised September 18 deadline; confirm the exact cutoff and time
zone in milestone 1. Budget 24 focused hours for one operator with coding-agent
help. Complete the milestones in order; all tasks below remain pending.

**Positioning:** Octomus finds useful repository improvements, challenges them,
and delivers verified PRs. Lead with “Our coding agent improved its own codebase.”
The audience is maintainers and solo developers who can review PRs but lack time
to keep finding and specifying useful work.

The strongest existing proof is [PR #3](https://github.com/tyk-swe/octomus-agent/pull/3),
merged September 13 at 02:26:21 UTC, as checked through GitHub on September 14.
Four regression tests failed before the fix and passed afterward; the
[historical rehearsal](../launch/day1-result.md) records the run and interventions.
The [newer recorded run](../launch/day3-result.md) is separate: it is blocked with
`publication_uncertain` and has no PR. Keep these outcomes distinct.

Owner-supplied text from the [challenge page](https://www.producthunt.com/contests/gpt-6-astra-challenge)
offers five winners $10,000 in API credits, a year of ChatGPT Pro, and OpenAI
promotion. Full eligibility and judging rules remain unverified. Usefulness,
ambition, and ease of evaluation guide this roadmap; they are not asserted
official judging criteria or a numerical prediction of winning.

## Milestone 1 — Prove a useful improvement

**Outcome:** an eligible launch story backed by a concrete PR and reviewable
evidence of Astra's role.

- [ ] Confirm the entry requirements in the submission form and linked terms:
  existing-project eligibility, whether Astra must be used to build and/or run
  the product, acceptable model-use evidence, required assets, selection method,
  prize conditions, and the exact cutoff. Start the listing draft and verify how
  the normal Product Hunt launch form associates it with this contest.
- [ ] Follow the [first-run procedure](../launch/first-real-run.md) on the owner's
  dedicated VM with its bot account, targeting Octomus. Record application and
  repository baselines and establish baseline checks. Apply the
  [Astra preset](../launch/astra-rehearsal.md), validate exact supported routes,
  and use one concurrent task and one task per cycle. Begin paused with an empty
  queue, then select **Run once**; an extra Audit is unnecessary.
- [ ] Capture proposal decisions, session references, interventions, elapsed
  time, and the actual outcome through return to paused/idle. Let the orchestrator
  own PR publication. Verify that the output commit, clean completed full-diff
  review, successful required checks, and observed PR head match. For a bug fix,
  demonstrate the same regression failing on the baseline and passing on the
  output in isolated checkouts.
- [ ] Complete the [run record](../launch/real-run-template.md) and a short case
  study: problem → selected improvement → deferred/rejected alternatives →
  change → verification → usefulness. Include a candid maintainer assessment.
  Distinguish configured routes from independently reported runtime identity;
  leave attributable cost unavailable without a suitable source.
- [ ] Follow the [evidence export](../launch/run-evidence.md) and
  [public showcase review](../launch/showcase.md) procedures. Prepare candidates
  privately, preserve adverse outcomes and gaps, and obtain owner approval bound
  to the exact public bytes. Keep raw transcripts and billing material private.
- [ ] Invite five relevant maintainers or developers to review the demo, aiming
  for three feedback sessions. Prepare 15 relevant existing contacts and select
  two communities that permit launch posts for milestone 3.

**Done when:** entry requirements are recorded, the listing draft exists, and one
useful public PR supports a case study with revision-linked review/check evidence
and a maintainer assessment. Public material has been reviewed for sharing.

**Fallback:** timebox the fresh-run attempt, including preparation, to four hours.
If it produces no useful PR, preserve the outcome, follow the documented
stop/recovery procedure for unfinished work, and use historical PR #3. Protect
time for the remaining milestones; never reconstruct a structured export from
the historical narrative.

## Milestone 2 — Ship a clear public demo

**Outcome:** a visitor can understand Octomus in 90 seconds, inspect the proof,
and find an honest installation path.

- [ ] Reuse the [static showcase](../launch/showcase.md) and publish through
  owner-controlled GitHub Pages at `https://tyk-swe.github.io/octomus-agent/`.
  Put the positioning statement and case-study/video links up front. Use
  “Explore a run” as the primary call to action and “Run it on your repository”
  as the secondary action, linking to the [source-build instructions](../../README.md).
  No release binary or crates.io package was available in the September 14 checks.
- [ ] Build recorded mode only from an authorized payload with its matching
  exact-byte approval. Earlier approval or local staging does not establish a
  current public deployment. If showing the older blocked run, retain its blocked
  status and absence of a PR. If authorized recorded input is unavailable, use
  the explicitly labeled synthetic explorer alongside the historical case study.
- [ ] Record a captioned 90-second video: problem and outcome in the first
  15 seconds, then proposal decisions, review/checks, and the PR. End with an
  invitation to inspect or try Octomus. Label accelerated footage and report
  actual elapsed time separately.
- [ ] Prepare four gallery images covering the product promise, proposal
  decisions, review/check evidence, and resulting PR. Label synthetic imagery.
  Polish the tagline, description, maker comment, and explanation of Astra's
  role to fit the form's actual limits and the available evidence.
- [ ] Run the feedback sessions and one external source-install attempt with a
  tester who has the required dedicated environment. Record understanding,
  trust concerns, completed setup steps, time, and blockers. Fix the two most
  consequential clarity or onboarding problems.
- [ ] Run `make check` and `make test` on the final application revision, then
  rebuild the intended public showcase: tests replace or remove generated output
  and use synthetic inputs. For recorded mode, verify `public-run.json` matches
  the approved bytes before final deployment. Check signed-out access, desktop
  and mobile layouts, captions, downloads, links, and absence of operator API
  requests. Keep the operator dashboard private.
- [ ] Attach the assets and schedule the listing for September 16 at
  00:01 America/Los_Angeles (07:01 UTC), subject to the confirmed rules and
  scheduler. Verify contest association and save the scheduling confirmation.

**Done when:** the public demo works, the video and four gallery images are ready,
setup findings and feedback are recorded, and the listing is scheduled with a
clear source-install path.

**Fallback:** if testers are unavailable, record the shortfall and perform a fresh
installation walkthrough yourself. Do not count it as external validation or
delay submission to chase attendance.

## Milestone 3 — Launch the entry and support evaluation

**Outcome:** a confirmed contest submission, accessible proof, and substantive
feedback from the developers Octomus serves.

- [ ] Verify that the listing is public, associated with the challenge, and
  linked to the correct demo. Save the public URL and submission confirmation.
  Publish the maker comment with the audience, Astra's role, strongest evidence,
  current limitations, and the feedback wanted.
- [ ] Publish one concise launch post and one technical walkthrough centered on
  the actual fix. Send the 15 personalized invitations and post in the two
  selected communities, asking for inspection and feedback. Follow confirmed
  promotion rules; avoid incentives, vote exchanges, and mass upvote requests.
  Do not assume leaderboard position determines the prize.
- [ ] Answer substantive comments and help interested maintainers evaluate setup
  and suitability. Correct confusing copy and broken links. Keep feature work
  frozen; verify any necessary launch-blocking application fix before publishing
  a replacement build.
- [ ] Address the most repeated objection in the listing or FAQ. Add feedback
  with permission and accurate attribution. Record available visits, meaningful
  conversations, setup attempts, and useful feedback without adding tracking to
  the isolated showcase. Treat outreach counts as targets until completed.
- [ ] Recheck enrollment, assets, and the final public URL. Resolve uncertain
  enrollment through the official submission or support path before the cutoff;
  a normal Product Hunt listing alone is insufficient confirmation. Use the
  September 17 buffer for responses and corrections, checking the deadline in its
  confirmed time zone.

**Done when:** eligibility and submission are confirmed before the cutoff, all
public proof is accessible, and actual outreach, setup outcomes, and feedback
are recorded. Mark missing results pending or unavailable.

The owner handles live operation, account actions, public sharing, deployment,
and launch posting. Coding-agent help prepares authorized files, copy, checks,
and evidence. Octomus delivers PRs for human review and merging. Defer new
runners, hosted SaaS, release binaries, crates.io publication, and unrelated
features until after the launch.
