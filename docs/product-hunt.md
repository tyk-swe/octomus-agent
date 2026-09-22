# Octomus Product Hunt launch

This package presents a **self-hosted preview for solo developers**. The public homepage,
documentation, and guided walkthrough are live on Cloudflare Workers at
**https://octomus-agent.tyk.sh/**. Product Hunt submission is a separate owner action;
deploying the website does not submit a listing or book a launch slot.

## Listing copy

**Name:** Octomus

**Tagline:** AI that finds repo improvements and opens reviewed PRs

**Description:**

Octomus finds useful work in your repo, challenges each proposal with two independent
reviewers, and delivers reviewed, verified PRs through Codex or OpenCode. You decide what
merges. Self-hosted preview: bring a dedicated VM and your provider access. Explore the
labeled synthetic demo at https://octomus-agent.tyk.sh/showcase/ before installing.

**Product URL:** https://octomus-agent.tyk.sh/

**Demo:** https://octomus-agent.tyk.sh/showcase/

**Documentation:** https://octomus-agent.tyk.sh/docs/

**Source:** https://github.com/tyk-swe/octomus-agent

**Topics to select from the submission form:** Developer Tools, GitHub, AI coding assistants.

**Pricing explanation:** Self-hosted software; you supply the VM and Codex or OpenCode
provider access. Their charges depend on your setup and usage. No validated per-task or
daily cost claim is available. Public binary distribution remains pending.

### Maker's first comment

Hi Product Hunt! Octomus is a few extra arms for a repository you care about.

It explores the code and project context, proposes useful improvements, and asks two
independent reviewers to challenge each proposal. Accepted work gets an isolated checkout,
fresh reviews of the full diff, and your configured checks before a PR is published.
Rejecting every proposal is a valid outcome. You decide what merges.

This launch is a self-hosted preview for one operator and one repository. You bring a
dedicated Ubuntu VM, your Codex or OpenCode access, and a repository-restricted GitHub
identity. Installation currently means building from source. Start with an audit to read
recommendations before choosing an execution run; later execution plans afresh.

The walkthrough and screenshots are explicitly synthetic. They show the interface and
decision process, including work that gets rejected, deferred, or blocked. They don't
establish real-world reliability, cost, speed, or savings.

I'd love feedback on whether the proposal reasoning and review evidence give you enough
context to decide what deserves a PR. Setup feedback and reproducible issues are welcome
on GitHub too.

### Short replies

| Question | Suggested reply |
| --- | --- |
| Does it merge changes? | Octomus publishes PRs. The owner decides what to merge, deploy, or migrate. |
| How is this different from prompting a coding agent? | It discovers and challenges its own proposals, then coordinates execution, review, checks, and PR delivery. It can decide that none of the proposed work is worthwhile. |
| Can I use my current AI access? | Configure explicit Codex or OpenCode routes for models your provider account exposes. Unsupported routes fail visibly; Octomus doesn't silently substitute a model. |
| Is there a hosted version? | This launch is a self-hosted preview. There is no hosted signup flow. |
| Is the demo real? | It's a labeled synthetic walkthrough, including a failed check. It is not evidence of live delivery or measured performance. |
| What is the cost per PR? | We don't have validated per-PR cost figures. VM and provider costs depend on your setup. Session-admission limits are not dollar caps. |
| Can I run it on my laptop? | The supported model uses a dedicated Ubuntu 24.04 VM. Runner and verification commands have the service user's permissions. Read the threat model before setup. |
| What does Pause do? | Pause stops new scheduling. In-flight tasks can still finish and publish; cancel individual tasks if needed. |

## Gallery and walkthrough

All product screenshots contain a synthetic-data label. These are export sizes, not a
claim about Product Hunt's current upload rules; confirm the submission form's crop and
file requirements before posting.

| Asset | Size | Caption |
| --- | --- | --- |
| [Thumbnail](launch-assets/thumbnail.png) | 240 × 240 | Octomus |
| [Social card](launch-assets/social-card.png) | 1200 × 630 | Your repo's next improvement. Ready for review. |
| [1. Discover](launch-assets/01-discover.png) | 1270 × 760 | Find worthwhile work, grounded in the repository. Synthetic example. |
| [2. Review](launch-assets/02-review.png) | 1270 × 760 | Two independent reviewers challenge the scope. Synthetic example. |
| [3. Verify](launch-assets/03-verify.png) | 1270 × 760 | A failing check stops delivery. Synthetic example. |
| [4. Control](launch-assets/04-control.png) | 1270 × 760 | Follow the work and decide what merges. Synthetic interface preview. |

### 60-second walkthrough script

Keep “Synthetic example — not a real run” visible when recording. The script describes
fabricated evidence, not something that happened on a live repository.

| Time | Screen | Narration |
| --- | --- | --- |
| 0–10 s | Landing-page hero and **Explore a sample run** | “Octomus discovers useful repo improvements and turns accepted ideas into reviewed pull requests. You decide what merges.” |
| 10–23 s | First proposal; **The idea**, then **Both reviewers** | “This synthetic example follows a small export bug. The proposal explains the scope, and two reviewers challenge whether it is worth doing.” |
| 23–35 s | Delivered task, latest review, configured checks | “Accepted work still needs a clean code review and passing checks on the output revision before delivery. This PR reference is fabricated.” |
| 35–48 s | Rejected rewrite, deferred cache, then blocked retry task | “Some ideas are rejected. Some wait for better evidence. Here, a failed regression test blocks delivery and stays visible.” |
| 48–60 s | Setup section and prerequisites | “Start with an audit. Octomus is a self-hosted preview: bring a dedicated VM, provider access, and a repository-restricted GitHub identity.” |

## Build, verify, and publish

The homepage and documentation work without JavaScript. Optional documentation search
runs in the browser over a local static index, and code blocks offer a copy button.
There are no analytics, account flows, or operator APIs on the public site. The interactive
showcase embeds only the explicit allowlisted synthetic payload and retains its restrictive
network policy. The build never reads operator configuration or live data.

Documentation pages are generated from the explicit source list in
`web/scripts/site-docs.mjs`. Edit the corresponding `docs/*.md` file to update a guide;
the build also regenerates navigation, heading links, the search index, and sitemap.

```sh
npm ci --prefix web
npm run site:build --prefix web
python3 tests/serve_site.py
```

Open **http://127.0.0.1:4310/**. Stop the preview with Ctrl-C before running
the site tests, which use the same port. The output is `dist/site/`, including the guides
at `docs/` and the sample at `showcase/`. The preview and browser tests also cover the
legacy `/octomus-agent/` subdirectory. To preview Cloudflare's routing and response headers,
use `npm run site:dev --prefix web` and open **http://127.0.0.1:4311/**.

```sh
npx --prefix web playwright install --with-deps chromium
make check
make test
make audit
```

`make test` includes the public-site browser tests and rebuilds the launch sample after
the adversarial showcase tests. For a focused site pass, use `npm run site:test --prefix web`.
To recreate the image exports after dashboard or demo changes:

```sh
make build
npm run launch:assets --prefix web
npm run site:build --prefix web
```

The capture command starts the existing temporary synthetic dashboard fixture on port
4299 and the explicit sample explorer on port 4308. Both are stopped on completion. It
overwrites `docs/dashboard.png` and `docs/launch-assets/*.png`; inspect and commit those
outputs with their source changes. Do not replace them with private operator screenshots.
It uses Playwright and the existing SVG mark, with no remote images or fonts.
The dashboard image uses a configured, paused sample state supplied in the browser;
capture blocks dashboard writes and external requests.

### Deploy to Cloudflare Workers

`web/wrangler.jsonc` defines the `octomus-agent-site` Worker and the
`octomus-agent.tyk.sh` custom domain. It uploads only `dist/site/` as Workers static assets.
No application server, database, runtime secret, or private Octomus service is deployed.
Cloudflare manages the custom domain's DNS and TLS certificate. Missing paths return the
custom 404 page with a 404 status; nested guides use directory URLs and work on refresh.

After the checks above, authenticate Wrangler using `CLOUDFLARE_API_TOKEN` supplied by
your shell or secret manager, then run from the repository root:

```sh
npm run site:deploy --prefix web
```

The token needs Workers Scripts edit, Workers Routes edit, and Zone read permissions for
the account and `tyk.sh` zone. Keep it out of source files, build output, and commit history.
The checked-in Cloudflare account ID is an identifier, not a credential.

The **Publish GitHub Pages mirror** workflow remains an optional manual GitHub Pages
mirror. It runs full CI and uploads only `dist/site`. Canonical URLs and social metadata
point to the Cloudflare domain. After updating `main`, rebuild and redeploy the Workers
site to publish the changes; pushing a commit alone does not update the live site.

### Launch-day checklist

- [ ] Resolve the existing owner naming/ownership clearance in [releasing](releasing.md).
      This preparation does not change LICENSE or NOTICE ownership facts.
- [ ] Review the final copy, synthetic payload, gallery, social card, and both mobile and
      desktop previews. Remove any claim that does not have evidence behind it.
- [ ] Commit the reviewed source and generated assets; require successful repository checks.
- [x] Run `npm run site:deploy --prefix web` with the Cloudflare token in the environment.
      Confirm the custom domain and TLS certificate are active.
- [ ] Open the live URL, follow every sample and setup link, download the public payload,
      and inspect the social-card URL. Confirm there is no login or operator API on the site.
- [ ] Confirm Product Hunt's current listing and image requirements, upload the reviewed
      assets, and submit the description and maker comment. No date or launch slot is booked
      by this repository workflow.
- [ ] Use GitHub issues for public feedback and SECURITY.md for private vulnerability reports.
      Record the live URL and launch date only after publication.

Rollback: from a clean checkout of a previously reviewed commit, install its locked web
dependencies and rerun `npm run site:deploy --prefix web`. Alternatively select a prior
deployment in the Worker's Cloudflare dashboard. Binary releases and registry publication
use their separate release process.
