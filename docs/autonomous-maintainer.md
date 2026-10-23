# Full autonomous maintainer — decision record

**Status: Final** — accepted by the owner ("accept", 2026-10-23).
Acceptance approves this record only; implementation requires a separate
explicit request and proceeds one stage at a time.

**Implementation status.** Stage 1 (multi-repository foundation) is
implemented on branch `tyk/multi-repo-foundation`. Focused store, engine,
API, CLI and broker validation tests, the API contract check and dashboard
type checking pass locally; browser, race and broader suites remain for CI.
Stages 2 (issue intake) and
3 (tiered auto-merge) remain deferred proposals awaiting their own
interviews and explicit go-aheads.

## Starting point (researched, 2026-10-23)

Octomus is already an autonomous repository maintainer orchestration system
within deliberate fences:

- Single-operator, single-repository service (`README.md`, `docs/deployment.md`).
- Discovers improvements (8–10 discovery agents), adversarial proposal review,
  orchestrator consolidation, execution via Codex/OpenCode, fresh full-diff
  code review, sandboxed verification, and PR delivery (`docs/architecture.md`).
- Cycles run in durable `paused` / `run_once` / `continuous` modes with idle
  backoff, daily admission limits, and operator work requests.
- The operator decides what merges. The service never merges, deploys, or
  migrates production systems — except opt-in maintenance mode, where the
  trusted orchestrator may squash-merge a small, clean-reviewed, verified
  maintenance PR once fresh GitHub checks, reviews, and protections allow it
  (`docs/maintenance-merging.md`, `AGENTS.md`).
- External/fork PRs are read-only context, never execution or maintenance
  targets. Workers never push, publish, or merge; the orchestrator owns
  publication from a trusted checkout, with untrusted children sandboxed.

So "make this into a full autonomous maintainer" must mean removing or
widening one or more of those fences. Which ones, and under what policy,
is what this interview decides. Nothing here assumes any fence is removed
until settled below.

## Scope contract (accepted)

**This interview's deliverable** is this document only. Implementation
requires a separate explicit request and proceeds one stage at a time.

**Stage 1 artifact boundary (multi-repository foundation).** In scope:
repository-scoping of SQLite state and schema migration (golden databases
must keep opening), saved configuration, scheduler and admission budgets,
per-repository credentials, engine orchestration, HTTP API and dashboard,
plus their tests and the affected guides (`docs/architecture.md`,
`docs/deployment.md`, `docs/configuration.md`, `README.md`, `AGENTS.md`).
Out of scope: auto-merge tier behaviour, issue intake, post-merge
watching/revert, deploy/release automation, multi-operator support, and
any change to sandbox isolation boundaries beyond repository scoping.

**Done means (stage 1).** All of: (a) one instance runs today's full
cycle behaviour — discovery, review, execution, verification, human-merged
PR delivery — independently for at least two configured repositories;
(b) per-repository caps and the instance-wide ceiling are enforced with
fair rotation, covered by tests; (c) existing single-repository state
migrates forward after a verified backup, and all golden databases open;
(d) affected Go packages, dashboard checks, and the relevant e2e scenarios
pass per `AGENTS.md` verification rules; (e) docs above describe the new
model and this record's later stages remain marked deferred. Nothing
outside this checklist is a completion dependency.

**Deferred stages (proposals, not approvals).** Stage 2: GitHub issue
intake through existing planning gates. Stage 3: tiered auto-merge with
shadow logging, post-merge watching, and the tiered revert response,
climbing the per-repository rollout ladder. Each returns for its own
interview and explicit approval before any work starts.

**Non-goals throughout.** No automatic release, deploy, or production
migration. No multi-operator tenancy. No execution, triage, or merging of
external/fork PRs. No weakening of review, verification, sandbox, or
branch-protection gates to gain autonomy.

**Risks.** Repository content, dependencies, and model output are
untrusted (`docs/threat-model.md`); stage 3 widens blast radius and is
contained only by tiers, ladder, and revert policy above. Multi-repository
credentials enlarge the value of the instance's GitHub access; per-repository
fine-grained tokens are required, never one fleet-wide token.

## Settled decisions

1. **Merge and deploy authority — tiered auto-merge, no auto-deploy (owner
   chose A).** The system may merge its own PRs without a human click only
   when the existing gates hold: clean fresh full-diff review, passing
   sandboxed verification, green GitHub checks, branch protections, and
   footprint/category limits, with stricter policy tiers for riskier
   changes. Releases and deploys remain human-triggered; the system never
   deploys or migrates production systems on its own. Today this authority
   exists only for opt-in small maintenance PRs
   (`docs/maintenance-merging.md`); "full" widens it under the tiered
   policy to be settled in decision 4.
2. **Repository and operator scope — one operator, many repositories per
   instance (owner chose B).** One running instance orchestrates several
   repositories for a single operator, each with its own policy, budgets,
   routes, verification commands, and merge tiers. This replaces today's
   single-operator, single-repository model and its one-repository token
   assumption (`README.md`, `docs/deployment.md`); state, configuration,
   scheduling, and the dashboard must become repository-scoped. It is not
   multi-tenant: one operator remains the only trust principal, and no
   tenant isolation is required.
3. **Work sources — discovery, operator requests, and GitHub issues
   (owner chose B).** Self-discovered improvements and operator work
   requests remain inputs, and GitHub issues become gated work candidates
   entering through the same adversarial proposal review and consolidation
   as discovered work — an issue never bypasses review, verification, or
   eligibility gates. External and fork PRs stay read-only context: never
   execution, triage-comment, or merge targets. Dependency and security
   work arrives via discovery or issues, not a separate alert pipeline.
4. **Auto-merge tiers — three risk tiers per repository (owner chose A).**
   Every merge keeps the floor: clean fresh full-diff review, passing
   sandboxed verification, green GitHub checks, and branch protections.
   On top of that, each repository classifies changes by category, paths,
   and footprint into: **auto** (low risk — docs, tests, small dependency
   bumps, small well-tested fixes; standard gates), **auto-with-extra-
   evidence** (medium risk — e.g. a second independent clean review and a
   green-check soak before merging), and **human-only** (high risk —
   authentication/security, schema and data migrations, CI/release/deploy
   configuration, sandbox and trust-boundary code, and anything over the
   footprint cap). New repositories start fully human-merged; the operator
   promotes tiers per repository. Exact tier rules, footprint caps, and
   the extra-evidence definition are per-repository policy to be specified
   in implementation planning, not loosened by defaults.
5. **Bad auto-merge response — automatic revert for low risk, freeze for
   medium (owner chose B for low risk).** Default-branch checks are
   watched after every auto-merge. If a failure is attributed to a
   low-risk (**auto** tier) merge, the system merges the revert itself
   and notifies the operator after the fact. If attributed to a medium
   (**auto-with-extra-evidence**) merge, it notifies immediately, pauses
   auto-merge and new cycles for that repository only, and prepares a
   revert PR for one human merge click; other repositories keep running.
   Human-only merges are the operator's own action and get notification
   only. Attribution rules (which failing checks, what window) are
   implementation detail and must fail toward the safer response when
   attribution is uncertain: pause and notify, never auto-revert.
6. **Budgets and fairness — per-repository caps under an instance ceiling
   (owner chose A).** Each repository has its own daily admission cap,
   cadence, and limits. An instance-wide ceiling bounds total admissions
   across all repositories. The scheduler rotates fairly between
   repositories with available work so no repository starves another.
   Notifications and run evidence remain per repository, extending the
   existing webhook outbox and `RunEvidenceV1` export rather than
   replacing them.
7. **Validation and rollout — per-repository ladder with a shadow phase
   (owner chose A).** Each repository climbs: audit-only (plans and
   would-merge decisions recorded, nothing executed) → human-merge with
   logged would-auto-merge decisions → low-risk auto-merge → medium
   auto-merge. Promotion is manual, per repository and per step; the
   fleet ships with every repository at the bottom. Existing test suites
   and golden state databases (`internal/store/testdata`) must keep
   passing at every stage; checked-in goldens must keep opening.
8. **First stage — multi-repository foundation, behaviour unchanged
   (owner chose A).** Repository-scoped state, configuration, scheduling,
   budgets, credentials, and dashboard, with human merge everywhere
   exactly as today. Issue intake and tiered auto-merge follow as later
   stages, each returning for its own go-ahead; their presence in this
   record does not approve them.

## Unresolved decisions

1. ~~Merge and deploy authority~~ — settled above (tiered auto-merge, no
   auto-deploy).
2. ~~Repository and operator scope~~ — settled above (one operator, many
   repositories per instance).
3. ~~Work sources~~ — settled above (discovery, operator requests, and
   gated GitHub issues; external PRs read-only).
4. ~~Autonomy policy and guardrails~~ — settled above (three risk tiers
   per repository over the existing review/verification/check floor).
5. ~~Operations~~ — settled above (tiered rollback; per-repository caps
   under an instance ceiling with fair rotation).
6. ~~Validation~~ — settled above (per-repository rollout ladder with
   shadow phase).
7. ~~Scope contract~~ — written above and accepted; this document is
   Final.

## Risks

See "Risks" in the scope contract above. No unresolved risk questions
remain in this interview; stage interviews revisit them.
