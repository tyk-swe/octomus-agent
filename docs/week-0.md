# Week 0 decision record

Recorded Tuesday, 2026-09-08. This is the owner-approved documentation and research portion of [the release plan](release-plan.md). Week 0 remains incomplete until the pending owner actions below are resolved. No accounts, reservations, purchases, calendar events, or GitHub issues were created by this work.

## Decisions

| Area | Status | Decision and remaining work |
| --- | --- | --- |
| Employer clearance | Pending | Confirm permission to publish under the employer's open-source/IP policy, the exact copyright holder, and any required disclaimer. Defer LICENSE/NOTICE changes until these facts are supplied. |
| Name | Provisional | Retain Octomus and the existing repository/package names while public-name clearance and reservations remain pending. Existing uses are recorded below. |
| Launch date and hours | Adopted; booking pending | Launch Tuesday, October 6 at 13:00 UTC / 22:00 KST. Cover replies through Wednesday, October 7 at 06:00 KST; resume at 10:00 KST. Owner still needs to book the calendar time and October 7 leave. |
| v0.1.0 scope | Adopted | Codex-only backend, single binary with embedded dashboard, and audit mode. Include the runner seam only if Week 1 meets its exit criteria by September 14; otherwise make it the first post-launch milestone. |
| Bot identity | Pending | Use a dedicated GitHub identity; `octomus-bot` is an unreserved candidate. Resolve the authentication limitation below before creating credentials. |
| Metrics | Adopted | Preserve all section 2 targets in the [launch issue draft](launch-issue.md). Actual results remain blank until measured. |

The Week 1 gate is five consecutive days of real cycles with no unexplained blocked task, at least three Octomus PRs merged into `main` by the owner, a cost table with real numbers, and saved screenshots. The owner subsequently adopted subscription-only usage and the measured-usage cost criterion described in [Week 1](week-1.md) and the [release plan](release-plan.md#5-week-1-sep-9-to-14-make-it-true). Scope adoption does not mean those features or exit criteria are already complete.

## Name research

Checked 2026-09-08. These are observations at the linked sources, not reservations or trademark clearance. A missing API record does not establish that a name can be registered; recheck with the service when reserving it.

| Surface | Result | Evidence and next action |
| --- | --- | --- |
| GitHub `octomus` | Occupied | The [GitHub user API](https://api.github.com/users/octomus) returned HTTP 200 for a user named `Octomus`. The exact name is occupied by an existing account; do not plan an organization at that name. |
| GitHub `octomus-bot` | Not found; unreserved | The [user lookup](https://api.github.com/users/octomus-bot) returned HTTP 404. Confirm availability during owner signup. |
| crates.io `octomus` / `octomus-agent` | Not found; unreserved | Both [octomus](https://crates.io/api/v1/crates/octomus) and [octomus-agent](https://crates.io/api/v1/crates/octomus-agent) returned HTTP 404. No package was published. |
| npm `octomus` / `octomus-agent` | Not found; unreserved | Both [octomus](https://registry.npmjs.org/octomus) and [octomus-agent](https://registry.npmjs.org/octomus-agent) returned HTTP 404. No package was published. |
| `octomus.dev` | Registered | The [registry RDAP record](https://pubapi.registry.google/rdap/domain/octomus.dev) lists registration on 2026-02-10 and expiration on 2027-02-10. Ownership by this project's owner has not been established. |
| `octomus.sh` | Unverified | The [RDAP lookup](https://rdap.org/domain/octomus.sh) returned HTTP 404. Confirm availability and price with a registrar; no purchase was made. |
| X `@octomus` | Unverified | The [profile URL](https://x.com/octomus) returned an HTML page, but did not establish handle ownership or signup availability. Check while signed in. |
| Existing brand use | Found | [Octomus.com](https://octomus.com/) presents “Collaboration Intelligence”; its [company profile](https://www.linkedin.com/company/octomus) describes a collaboration/project-management platform. Record this adjacent use when reviewing the public name. |
| Trademark search | Inconclusive | Exact-name web searches, including searches restricted to USPTO, WIPO, and KIPRIS domains, did not establish clearance. The [USPTO](https://tmsearch.uspto.gov/) and [WIPO](https://branddb.wipo.int/) search interfaces did not expose usable registry results in this check. Owner must finish relevant registry searches, including [KIPRIS](https://www.kipris.or.kr/), and resolve the existing brand use before treating the name as cleared. |

## Calendar and availability

The [official 2026 calendar announcement](https://www.kasi.re.kr/kor/post/newsMaterial/32031) lists the Chuseok break as September 24–27, including Sunday; September 28 is not part of that announced holiday break. October 5 is a substitute holiday for National Foundation Day. Keep the milestone dates and correct their weekday labels: September 8, September 22, and September 29 are Tuesdays.

| Event | UTC | KST | Status |
| --- | --- | --- | --- |
| Show HN | October 6, 13:00 | October 6, 22:00 | Adopted launch time |
| First reply shift | October 6, 13:00–21:00 | October 6, 22:00–October 7, 06:00 | Adopted; calendar booking pending |
| Rest | October 6, 21:00–October 7, 01:00 | October 7, 06:00–10:00 | Adopted |
| Replies resume | October 7, 01:00 | October 7, 10:00 | Adopted; calendar booking pending |
| Day off | October 6, 15:00–October 7, 15:00 | October 7, all day | Leave request pending |

Target replies within two hours during the first reply shift, then handle the overnight backlog when replies resume. This is eight hours of initial coverage, with a four-hour rest gap. Calendar entries and leave must be booked by the owner; this document does not assert personal availability or employer approval.

## Bot setup handoff

The desired result is a dedicated identity visibly authoring PRs on `tyk-swe/octomus-agent`. The repository was private when inspected, and no launch issue existed. Changing Git commit author metadata alone does not change the authenticated GitHub PR author.

The release plan's proposed fine-grained token needs an ownership/membership check first. [GitHub documents](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens#fine-grained-personal-access-tokens-limitations) that fine-grained tokens do not support contributions where the token owner is an outside or repository collaborator. A separate bot account added as a collaborator to another personal account's repository therefore cannot simply use that recipe.

Keep the identity/authentication arrangement pending. The owner must select a supported arrangement that preserves access restricted to the target repository; do not silently substitute a broadly scoped classic token. Account creation, repository ownership changes, GitHub App setup, and runtime credential support are outside this documentation change. Once the arrangement is resolved, document its precise permissions and credential lifecycle before Week 1 live use.

## Owner checklist

- [ ] Confirm employer permission to publish; record the clearance date and outcome without copying private policy material into the repository.
- [ ] Supply the exact copyright holder and required disclaimer, if any; then add the appropriate attribution to LICENSE or NOTICE.
- [ ] Resolve the existing Octomus brand use and complete the trademark/name review. Keep the name provisional until then.
- [ ] Confirm registry, domain, and handle availability; reserve the chosen identities through owner-controlled accounts. Record actual reservation outcomes and URLs.
- [ ] Resolve a supported repository-restricted bot authentication arrangement, then create the identity and credentials. Store credentials outside repository documents.
- [ ] Book the October 6–7 reply shifts and request October 7 leave.
- [ ] Create the launch issue from [the prepared draft](launch-issue.md), updating document links to the branch containing these files if they are not yet on `main`; record its URL here after creation.

Launch issue URL: pending creation.

When configuring dogfooding for this repository, use branch prefix `tyk/`. Branches created by Codex also use `tyk/`; the product's general default is outside this documentation change.
