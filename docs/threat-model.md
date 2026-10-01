# Threat model

Octomus is a single-operator service with two deployment models:

- **Docker deployment (default).** Every agent turn and verification command runs in its own
  [sandbox](sandbox.md) container. The control plane, which holds the GitHub token, the
  operator token and state, never runs repository code or model-directed commands. This
  deployment can share a host with other services.
- **Dedicated VM (`--sandbox off`).** Runner sessions and verification commands run with the
  service account's full permissions, without a process sandbox or interactive approvals.
  The VM is the isolation boundary. Do not place unrelated credentials, production services,
  workstation storage or GPU access on it.

## Boundaries and capabilities

| Surface | Trust and capability |
| --- | --- |
| Host administrator | Controls images, the compose file or unit, `.env`, credentials, backups and network policy. |
| Operator dashboard/API | A bearer token grants configuration changes, verification commands, task controls and model usage. In the Docker deployment it cannot turn the sandbox off, widen egress, raise limits, choose the image, or repoint the repository or runner programs. Unsandboxed, treat it as a host capability. |
| Control plane (`octomus`) | Trusted. Holds state, the operator token and the GitHub token; runs only its own git and gh. No Docker socket. |
| Sandbox broker (`sandboxd`) | Trusted and root-equivalent through the Docker socket. No network. Builds every container spec itself from a narrow, validated request. |
| Egress gateway (`egress`) | Trusted. Opens HTTPS tunnels for sandboxes holding a live lease, to allowlisted hosts with public addresses only. |
| Sandboxes | Untrusted. See their own root's work tree, read-only git metadata and home; runner sandboxes also see the runner login volume. |
| Service account (unsandboxed) | Can read its credentials, change its state and checkouts, run programs and contact permitted network destinations. |
| Repository, PRs, dependencies and model output | Untrusted input, including adversarial instructions and executable build scripts. |
| Model providers and GitHub | External services receive repository content and authenticated requests under the operator's accounts and their terms. |
| Browser | Single operator, same-origin API; token stays in tab memory and is lost on reload. Browser extensions or a compromised endpoint can still steal it. |
| SQLite, clones, logs and transcripts | Private local evidence. Backups inherit their sensitivity. Each runner maintains its own state and transcript storage. |

## Prompt injection and publication

Prompt injection is in scope and **not prevented by this design**. It can arrive through
source files, AGENTS.md, dependency scripts, issue or PR content, or model output. Prompts
instruct workers not to push, publish, merge or deploy. The orchestrator owns publication
and checks branch ownership, reviewed revisions, verification and remote leases.

In the Docker deployment, those checks are backed by isolation:
- A sandbox holds no GitHub credential and has no route to GitHub, so an injected
  instruction cannot push, open a pull request or reach other repositories.
- Git metadata the orchestrator acts on lives outside the sandbox's write reach, so a
  sandbox cannot plant hooks or configuration that the orchestrator's own git would run.
- Every process a turn started is gone before the orchestrator reads the work tree.

What remains possible:
- Injected work can still shape the proposed change. Full-diff review, verification on the
  reviewed revision and the owner's merge decision are the gate.
- A runner sandbox can read and misuse the runner login, and send data to allowlisted
  hosts.

Unsandboxed, the workflow checks do not stop a malicious process from directly using the
service user's credentials or changing files it can access.

An audit runs planning only: the orchestrator queues no tasks and invokes no publication
operation. It still spends model allowance, fetches repositories, creates clones and runs
agents, sandboxed or not depending on the deployment. Planning worktree checks detect
local changes after a session; unsandboxed, they are not containment or proof that no
external side effect happened.

Repository-scoped authentication, protected default branches and release tags, and owner
review remain necessary in both models. Octomus delivers PRs; the owner decides whether to
merge.

## Dashboard exposure

Bind to `127.0.0.1:4200` and access it through SSH forwarding. Non-loopback startup
emits a warning; it is not blocked. The API has no TLS listener, multi-user roles,
per-operation authorization, token expiry or comprehensive request-rate limit.
It has constant-time token-hash comparison and a shared failed-authentication
backoff (100 ms, doubling to one second; resets after 60 seconds without failures).
Valid tokens bypass this delay. Concurrent requests can bypass its practical
throttling effect, so it is a deterrent rather than protection for public exposure.
Request headers must arrive within 10 seconds, idle keep-alive connections close after
two minutes and request bodies are limited to 256 KiB. Reading a body and writing a
response have no time limit, because connection checks and model catalogs can take
about a minute.

Authenticated mutations require a JSON content type; no cross-origin access policy
is enabled. Security headers reduce browser attack surface but cannot secure a
compromised client. The unauthenticated health endpoint and dashboard assets expose
application identity/version. Rotate the operator token through the service
environment and restart; old tokens then fail.

## What redaction does

The filter replaces common bearer tokens, selected GitHub/OpenAI key patterns,
credential-bearing URL prefixes, and environment values of at least eight
characters whose variable names contain TOKEN, SECRET, PASSWORD or API_KEY, as well
as the attention webhook URL. It
bounds each returned string to 16,384 characters. Events and dashboard JSON pass
through redaction; this is not an encryption or data-loss-prevention system.

Complete values take `Secrets`, `Text` or `JSON`. Text already cut by a capture
or read limit takes `Fragment` with an explicit kind matching its retention
policy. That operation owns both boundary normalization and scrubbing — including
the policy-specific ordering needed to avoid exposing a token whose context was
cut — before any later display or evidence bound applies.

Outbound pull request titles, descriptions and follow-up comments pass through
the same secret patterns without the length bound; the remote's own size limits
apply instead, and text that would lose the delivery marker or exceed those
limits is refused before writing rather than shortened.

It can miss encoded, split, unfamiliar or short secrets, credentials read from
files, and private source/text that is not a recognized token. Raw task records,
workspaces, process output and runner transcripts may retain sensitive content.
Review every shared screenshot, JSON export and log manually. Never commit private
billing screenshots, credentials or raw transcripts. Record redacted observations
and private evidence references instead.

## Where each control is enforced

Controls stay at the module that owns their seam; there is no central security facade.
Each fails closed: when a check cannot run or does not hold, the operation is refused
rather than allowed to proceed unguarded.

| Control | Enforcing seam | Fail-closed behavior |
| --- | --- | --- |
| Where untrusted children run | `internal/sandbox` | Every runner and verification command starts through the configured backend; the Docker backend never falls back to the host, and the scheduler error-pauses while the broker is unavailable. |
| Sandbox construction | `internal/sandbox/broker` | Requests outside the owned-root patterns, or with symlinked or foreign-owned directories, are refused. Requests beyond capacity wait for a slot, and a slot is held until its container's removal is confirmed, so live sandboxes never exceed `OCTOMUS_SANDBOX_MAX`. A container Docker would not create exactly as specified (any create warning) is removed and refused. Docker Engine older than 28 (API 1.48), or a sandbox network that is not internal with an isolated gateway or that enables IPv6, stops the broker from starting. |
| Egress | `internal/egress` | No lease, an unlisted host or an IP literal refuses the tunnel before any lookup; a name that resolves only to non-public addresses is refused after the lookup and before any connection. |
| Trusted git metadata | `internal/git`, `internal/workspace` | Orchestrator git runs only against `repo.git` with hooks, fsmonitor and submodule recursion pinned off; new or moved gitlinks refuse the snapshot. |
| Deployment-owned posture | `cmd/octomus-agent`, `internal/engine` | A saved configuration that repoints the pinned repository is refused; a GitHub token file or pinned repository with `--sandbox off` stops startup. |
| Operator HTTP edge | `internal/httpapi` | Requests without a valid operator token are refused before reaching controls. |
| Child environment and lifetime | `internal/process`, `internal/sandbox` | Host children lose the operator token, webhook URL and repository-locating Git variables and join an owned process group; sandboxes get an environment built from scratch and die with their stream. |
| Runner protocol policy | `internal/runner` | Catalog, route and message-bound violations refuse the session or connection. |
| Configuration identity | `internal/config`, `internal/wirejson`, `internal/engine` | An identity change mid-operation fails that operation. |
| Managed workspaces | `internal/workspace` | Cleanup refuses noncanonical paths, indirect children and symlink paths. |
| Publication | `internal/git` | A push proceeds only while branch ownership, the reviewed revision and remote leases hold. |
| Text egress | `internal/redact` | Complete values and cut fragments are normalized and scrubbed before display or evidence bounds. |
| Durable state and evidence | `internal/store`, `internal/evidence` | Records stay typed and versioned; exports are read-only. |

## Residual risk

In the Docker deployment, the [sandbox page](sandbox.md#what-the-sandbox-does-not-cover)
lists what isolation does not cover:
- runner logins are readable inside runner sandboxes;
- sandboxes share an internal network;
- access to the Docker socket is root-equivalent;
- containers share the host kernel unless you use gVisor.

Disk limits are admission checks, not quotas. Exhaustion, credential misuse within the
egress allowlist, and malicious dependency code acting inside a sandbox remain possible.

On a dedicated VM, the systemd unit uses NoNewPrivileges, a read-only system filesystem
with explicit service-home and checkout write paths, private temporary directories,
protected kernel tunables and restricted SUID/SGID changes. KillMode=control-group stops
remaining children. These controls reduce accidental host changes; they do not isolate
agents from other data belonging to the service user. Do not grant that account sudo
privileges. PrivateTmp does not disable temporary-file execution. Restrict egress with
VM or network policy; no application egress allowlist is enforced in this mode.

In both models, session admission limits are not dollar or subscription-allowance caps. Use
protected backups, monitor unresolved workspaces and disk growth, and stop the service if
credentials may be compromised. Follow [deployment](deployment.md) for stopping and
recovery, and [SECURITY.md](../SECURITY.md) for private disclosure.
