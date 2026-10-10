# Maintenance merge destination protection

Maintenance delivery can automatically squash-merge a small reviewed and verified PR.
It requires **GitHub to restrict the merger to the configured default branch** as well
as the existing head, review, check, footprint, task and operating-mode gates. An
unconfigured or unverifiable destination restriction produces a visible **Manual**
outcome before any merge request is sent. Standard delivery still publishes for a
human merge decision.

## Why a separate restriction is required

GitHub's [merge endpoint](https://docs.github.com/en/rest/pulls/pulls#merge-a-pull-request)
accepts the expected head SHA but has no expected-base parameter. A maintainer can
retarget a PR after a client checks it. Another read narrows that window without
closing it. Octomus therefore uses a dedicated merger credential whose branch updates
are restricted by a repository ruleset **at the time GitHub applies the merge**.

This assumes the trusted repository administrators maintain the configured ruleset.
An administrator who removes the restriction changes the security policy; Octomus
cannot defend against an administrator intentionally changing protections during a
request. Pause maintenance before changing the ruleset, merger identity, or default
branch. Octomus checks the current policy before every attempt and never installs,
edits or bypasses repository rules itself.

## Configure the repository

Use a separate GitHub user for merging, with a fine-grained token scoped to this one
repository and Contents read/write permission. The token is used only for identifying
that user and sending the squash-merge request. Normal `gh` credentials remain the
publication, remote-status and policy-reader identity. The merger is not granted
administration or any ruleset bypass. Keep the default branch's required reviews,
checks and other protections in place; this destination rule does not replace them.

Create an **active repository branch ruleset** with the following exact shape. Replace
`OWNER/REPOSITORY`, `main`, and the example publisher user ID with your actual values.
The ruleset ID is allocated by GitHub; it is not part of the create request.

```json
{
  "name": "Restrict the Octomus merger destination",
  "target": "branch",
  "enforcement": "active",
  "conditions": {
    "ref_name": {
      "include": ["~ALL"],
      "exclude": ["refs/heads/main"]
    }
  },
  "bypass_actors": [
    {"actor_id": 12345, "actor_type": "User", "bypass_mode": "always"}
  ],
  "rules": [
    {"type": "update", "parameters": {"update_allows_fetch_and_merge": false}}
  ]
}
```

Add each trusted publisher or maintainer who must update other branches to the
explicit user bypass list, including the normal publication identity. **Never add the
merger.** This rule affects every non-default branch, including branches created in
the future. Review the bypass list before activating it so ordinary collaborators
retain the intended access. An empty list is restrictive and accepted, but prevents
all non-default branch updates. Creation is separate from update permission.

Octomus deliberately accepts only explicit `User` bypass entries. Team, repository
role, organization-administrator, deploy-key and App bypasses need additional
membership/provenance checks that are not implemented, so they produce Manual. The
excluded branch must be the exact configured branch; wildcard exclusions and the
movable `~DEFAULT_BRANCH` alias are rejected. Inherited organization rulesets cannot
substitute for this repository rule.

The normal policy-reader identity must be able to see the complete bypass list.
GitHub [omits `bypass_actors` unless the reader has write access to the ruleset](https://docs.github.com/en/rest/repos/rules#get-a-repository-ruleset).
An omitted or null list is unverifiable, and Octomus refuses it. If this visibility
or the required access model is unsuitable for your repository, keep human merging.
See GitHub's [ruleset API](https://docs.github.com/en/rest/repos/rules) and
[available rules](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets)
for setup and access details.

## Configure the service

| Setting | Purpose |
| --- | --- |
| `OCTOMUS_MERGE_TOKEN` | Dedicated merger token, supplied to the trusted service process |
| `OCTOMUS_MERGE_TOKEN_FILE` | Alternative one-line secret file; do not also set the token variable |
| `OCTOMUS_MERGE_RULESET_ID` | Positive numeric ID of the active repository ruleset above |

These are deployment settings, not editable dashboard fields. The merger secret and
its file path are removed from ordinary child environments. Only the two dedicated
`gh` requests receive the token as `GH_TOKEN`; it is never passed as a command argument,
used by the Git credential helper, persisted in state, or supplied to a sandbox.

For Docker Compose, install the token file with the same ownership as the existing
deployment secrets, set the ruleset ID in `.env`, and include the optional overlay:

```bash
# From deploy/docker. /path/to/merger-token contains the token, not shell syntax.
sudo install -o 10001 -g 10001 -m 0600 /path/to/merger-token secrets/merge_token
# Add OCTOMUS_MERGE_RULESET_ID=<actual numeric ID> to .env.
docker compose -f compose.yaml -f maintenance.compose.yaml up -d --force-recreate octomus
```

Use both `-f` arguments for subsequent stack updates and secret rotations. Only the
control plane receives this secret. Select Maintenance delivery in the dashboard
after the repository policy and service configuration are ready. A configured policy
does not authorize a PR by itself: every original maintenance gate still has to pass.

## Outcomes and recovery

A missing token, inactive/wrong ruleset, hidden bypass list, ambiguous identity, or
unreadable policy produces Manual with its reason and no merge mutation. The reviewed
authorization remains recorded; after correcting deployment settings, continuous
operation can check again. A new run-once batch does not adopt an older manual
delivery: use continuous operation, merge it manually, or produce a new reviewed
delivery. A GitHub refusal
at the mutation, including a retargeted destination, never counts as a merged PR.

After a successful acknowledgement, Octomus reads the authoritative PR outcome and
checks the repository, source branch, reviewed head, destination and merge commit
before recording **confirmed**. If that read fails or disagrees, the outcome stays
**uncertain** for reconciliation. Recovery observes the remote before another attempt;
a matching completed merge can be recorded as **observed** without repeating the write.

The deterministic GitHub fixture models this same configured update rule. Its race
regression retargets the PR inside the merge operation, after policy/readiness reads,
and checks that neither branch changed. It does not invent a base-SHA or base-name
guard in GitHub's merge API. Live repository-policy setup remains an operator action.
