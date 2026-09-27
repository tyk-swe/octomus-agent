# Threat model

Octomus is a single-operator service for a **dedicated Linux VM**. The VM is the
isolation boundary. Runner sessions (Codex or OpenCode) and verification commands run with the service
account's full permissions, without a process sandbox or interactive approvals.
Do not place unrelated credentials, production services, workstation storage or
GPU access on this VM.

## Boundaries and capabilities

| Surface | Trust and capability |
| --- | --- |
| Host administrator | Controls binaries, unit configuration, credentials, backups and network policy. |
| Operator dashboard/API | A bearer token grants configuration changes, arbitrary verification commands, task controls and model usage. Treat it as a host capability. |
| Service account | Can read its credentials, change its state/checkouts, run programs and contact permitted network destinations. |
| Repository, PRs, dependencies and model output | Untrusted input, including adversarial instructions and executable build scripts. |
| Model providers and GitHub | External services receive repository content and authenticated requests under the operator's accounts and their terms. |
| Browser | Single operator, same-origin API; token stays in tab memory and is lost on reload. Browser extensions or a compromised endpoint can still steal it. |
| SQLite, clones, logs and transcripts | Private local evidence. Backups inherit their sensitivity. Each runner maintains its own state and transcript storage. |

## Prompt injection and publication

Prompt injection through source files, AGENTS.md, dependency scripts, issue/PR
content or model output is in scope and **not prevented by this design**. Prompts
instruct workers not to push, publish, merge or deploy. The orchestrator owns normal publication
and checks branch ownership, reviewed revisions, verification and remote leases.
Those workflow checks do not prevent a malicious process from directly using the
service user's credentials or changing files it can access.

An audit runs planning only: the orchestrator queues no tasks and invokes no
publication operation. It still spends model allowance, fetches repositories,
creates clones and invokes unsandboxed agents. Planning worktree checks detect
some local changes after a session; they are not containment or proof that no
external side effect happened.

A process sandbox can reduce host access, but would not by itself eliminate what
an agent with push-capable credentials can do to a repository. Repository-scoped
authentication, protected default branches and release tags, owner review and dedicated-host
isolation remain necessary. Octomus delivers PRs; the owner decides whether to merge.

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

Controls stay at the module that owns its seam; there is no central security
facade. Each fails closed: when a check cannot run or does not hold, the
operation is refused rather than allowed to proceed unguarded.

| Control | Enforcing seam | Fail-closed behavior |
| --- | --- | --- |
| Host isolation | `deploy/octomus-agent.service`, dedicated VM | Hardened unit and VM policy bound impact; no in-process sandbox is claimed. |
| Operator HTTP edge | `internal/httpapi` | Requests without a valid operator token are refused before reaching controls. |
| Child environment and lifetime | `internal/process` | The operator token, webhook URL and repository-locating Git variables are stripped; every child joins an owned process group. |
| Runner protocol policy | `internal/runner` | Catalog, route and message-bound violations refuse the session or connection. |
| Configuration identity | `internal/config`, `internal/wirejson`, `internal/engine` | An identity change mid-operation fails that operation. |
| Managed workspaces | `internal/workspace` | Cleanup refuses noncanonical paths, indirect children and symlink paths. |
| Publication | `internal/git` | A push proceeds only while branch ownership, the reviewed revision and remote leases hold. |
| Text egress | `internal/redact` | Complete values and cut fragments are normalized and scrubbed before display or evidence bounds. |
| Durable state and evidence | `internal/store`, `internal/evidence` | Records stay typed and versioned; exports are read-only. |

## Host controls and residual risk

The systemd unit uses NoNewPrivileges, a read-only system filesystem with explicit
service-home/checkout write paths, private temporary directories, protected kernel
tunables and restricted SUID/SGID changes. KillMode=control-group stops remaining
children. These controls reduce accidental host changes; they do not isolate
agents from other data belonging to the service user. Do not grant that account
sudo privileges. PrivateTmp does not disable temporary-file execution.

Restrict egress using operator-managed VM/network policy and verify real login,
clone and registry access. No application egress allowlist is enforced. Session
admission limits are not dollar or subscription-allowance caps. Disk limits are
checked before admission, not continuous filesystem quotas. Exhaustion, service
compromise, credential misuse and malicious dependency code remain possible.

Use protected backups, monitor unresolved workspaces and disk growth, and stop the
service if credentials may be compromised. Follow [deployment](deployment.md) for
stopping/recovery and [SECURITY.md](../SECURITY.md) for private disclosure.
