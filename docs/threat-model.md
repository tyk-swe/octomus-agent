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

Authenticated mutations require a JSON content type; no cross-origin access policy
is enabled. Security headers reduce browser attack surface but cannot secure a
compromised client. The unauthenticated health endpoint and dashboard assets expose
application identity/version. Rotate the operator token through the service
environment and restart; old tokens then fail.

## What redaction does

The filter replaces common bearer tokens, selected GitHub/OpenAI key patterns,
credential-bearing URL prefixes, and environment values of at least eight
characters whose variable names contain TOKEN, SECRET, PASSWORD or API_KEY. It
bounds each returned string to 16,384 characters. Events and dashboard JSON pass
through redaction; this is not an encryption or data-loss-prevention system.

It can miss encoded, split, unfamiliar or short secrets, credentials read from
files, and private source/text that is not a recognized token. Raw task records,
workspaces, process output and runner transcripts may retain sensitive content.
Review every shared screenshot, JSON export and log manually. Never commit private
billing screenshots, credentials or raw transcripts. Record redacted observations
and private evidence references instead.

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
