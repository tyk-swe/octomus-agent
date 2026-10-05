# A few extra arms for your repository.

Octomus discovers useful improvements, asks two independent reviewers to challenge each
proposal, and turns accepted work into reviewed, verified GitHub pull requests. You decide
what merges.

This documentation takes you from a first audit to running Octomus on your own host.
The current release is a **self-hosted preview for one operator and one repository**,
using your Codex or OpenCode access.

## Start with a little curiosity

1. **Prepare your host.** Bring a Linux host with Docker, provider access, and a GitHub
   token restricted to the target repository. Follow [getting started](getting-started.md):
   every agent turn then runs in its own [sandbox](sandbox.md).
2. **Make your first audit.** Enter your configuration, save it, check the connection,
   then run an audit. It records recommendations without queuing code changes.
3. **Choose when to execute.** Run once plans afresh and executes accepted work. You
   can enable continuous operation later. Inspect the evidence before merging any PR.

## Make Octomus your own

| Guide | What you will learn |
| --- | --- |
| [Configuration](configuration.md) | Set your repository, verification commands, and operating limits. |
| [Model routing](model-routing.md) | Choose an explicit Codex or OpenCode model for every role. |
| [Sandbox](sandbox.md) | What isolation guarantees, how to prove it, and how to extend the sandbox image. |
| [Deployment and operations](deployment.md) | Run the stack with Docker Compose (or systemd), manage work, and back up state. |
| [Usage and costs](cost.md) | Understand session admissions and what they do not measure. |

## Understand the decisions

Octomus keeps a reason for every proposal decision and evidence for task delivery.
An audit recommendation is separate from an execution task; passing a baseline check is
separate from verifying a change.

- [Architecture](architecture.md) explains discovery, proposal review, isolated execution,
  full-diff code review, repair, verification, and publication.
- [Run evidence](run-evidence.md) describes the recorded evidence and its limits.
- [Security and trust](threat-model.md) explains the trust boundaries between the control
  plane, the sandbox broker, the egress gateway and sandboxes, and what repository
  instructions can still do.

## Keep the final say

Octomus publishes pull requests. Merging, deployment, and production migrations stay
with you. **Pause** stops new scheduling; active tasks can still finish and publish.
Use **Cancel task** to stop an individual task.

Install from the published release: the checksum-verifying installer delivers the
binary, and the signed release images deploy with Docker Compose; building from
source remains supported. VM and provider costs depend on your setup;
session-admission limits are not dollar caps.

For questions and reproducible bugs, use [GitHub issues](https://github.com/tyk-swe/octomus-agent/issues).
Report vulnerabilities through the [security policy](../SECURITY.md).
