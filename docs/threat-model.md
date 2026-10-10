# Threat model

Octomus is a single-operator service with two deployment models. In the default Docker
deployment, every agent turn and verification command runs in a fresh [sandbox](sandbox.md)
container. The trusted control plane holds the operator and GitHub tokens but never runs
repository code or model-directed commands. With `--sandbox off`, the dedicated VM and its
service account are the boundary; do not place unrelated credentials, production services,
workstation storage or GPU access there.

The [sandbox guide](sandbox.md) owns lifecycle, mounts, egress, resource limits, trusted Git
metadata and containment checks. [Deployment](deployment.md) owns host hardening, private
access, credentials and recovery. This page records the security decisions and their limits.

## Boundaries and capabilities

| Surface | Trust and capability |
| --- | --- |
| Host administrator | Controls images, compose or systemd, `.env`, credentials, backups and network policy. |
| Operator dashboard/API | A bearer token grants configuration and task controls. Docker deployment settings, egress, image, repository and runner programs stay host-owned. Unsandboxed, treat the token as a host capability. |
| Control plane | Trusted. Holds state and tokens; runs only its own trusted Git and GitHub CLI operations. It has no Docker socket. |
| Sandbox broker | Trusted and root-equivalent through Docker. It validates owned roots and builds every container specification. |
| Egress gateway | Trusted allowlist and lease gate. It cannot inspect encrypted HTTP authority or prevent misuse of an allowed endpoint. |
| Sandboxes | Untrusted. They see their owned work tree, read-only Git metadata and home; runner sandboxes also see the runner login volume. |
| Repository, PRs, dependencies and model output | Untrusted input, including executable scripts and adversarial instructions. |
| Model providers and GitHub | External services receive repository content and authenticated requests under the operator's accounts and their terms. |
| Browser and local evidence | The token stays in tab memory, but a browser extension or compromised endpoint can steal it. SQLite, clones, logs, transcripts and backups are private local evidence. |

## Prompt injection and publication

Prompt injection is in scope and **not prevented by this design**. It can arrive through
source files, `AGENTS.md`, dependencies, issues, pull requests or model output. Workers are
instructed not to push, publish, merge or deploy; the orchestrator owns publication and
checks branch ownership, the reviewed revision, verification and remote leases.

Docker isolation backs those workflow checks: a sandbox has no GitHub credential or route,
trusted Git metadata is outside its write reach, and every process started for a turn is
gone before the orchestrator reads the work tree. Injected work can still shape a proposed
change, and a runner sandbox can read its login and send data to an allowlisted host. Full
diff review, verification and the owner's merge decision remain the gate. Unsandboxed,
workflow checks do not stop a process from using the service user's credentials or files.

An audit performs planning only: it queues no tasks and invokes no publication operation.
It still fetches repositories, creates clones, runs agents and consumes provider allowance.
Planning checks detect local changes after a session; they are not containment or proof of
no external side effect in unsandboxed mode.

Repository-scoped authentication, protected default branches and owner review are required
in both models. Octomus delivers pull requests; the owner decides whether to merge. The
opt-in maintenance mode may squash-merge a small reviewed and verified maintenance PR only
after fresh remote checks and protections confirm the recorded head; conflicts, required
reviews, queue requirements, disabled squash, failed checks or incomplete evidence stay
manual. See [maintenance merging](maintenance-merging.md).

## Dashboard exposure

Bind to `127.0.0.1:4200` and use SSH forwarding. Non-loopback startup warns but is not
blocked. The API has no TLS listener, multi-user roles, per-operation authorization, token
expiry or comprehensive rate limit. It compares token hashes in constant time and applies a
shared failed-authentication backoff; this is a deterrent, not public exposure protection.

Request headers have a 10-second deadline, idle keep-alive closes after two minutes and
bodies are limited to 256 KiB. Authenticated mutations require JSON content type. Health and
dashboard assets are unauthenticated and reveal application identity/version. Rotate the
operator token through the service environment and restart.

## Redaction and evidence

The shared filter removes common bearer tokens, selected GitHub/OpenAI key patterns,
credential-bearing URL prefixes, matching secret environment values and the attention
webhook URL. Returned strings are bounded to 16,384 characters; outbound PR text uses the
remote's limits instead. [Run evidence](run-evidence.md) describes the durable evidence
shape.

Redaction is not encryption or data-loss prevention. It can miss encoded, split, unfamiliar
or short secrets, credentials read from files and private text that is not a known token.
Raw records, workspaces, process output and runner transcripts may retain sensitive content;
review shared screenshots and exports manually.

## Enforcement seams

Controls live at the module that owns each boundary and fail closed when a check cannot run:

| Boundary | Owner | Refusal condition |
| --- | --- | --- |
| Untrusted children | `internal/sandbox`, `internal/process` | No configured backend, invalid sandbox request or lost broker refuses the operation. |
| Egress | `internal/egress` | No live lease, unlisted host or non-public resolution refuses the tunnel. |
| Trusted Git and workspace | `internal/git`, `internal/workspace` | Foreign metadata, unsafe paths, hooks or indirect cleanup targets refuse the operation. |
| Operator edge | `internal/httpapi` | Invalid token, method, content type or bounded-body violation never reaches controls. |
| Runner protocol and configuration | `internal/runner`, `internal/config`, `internal/wirejson` | Unsupported route, message, identity or schema fails the session or operation. |
| Publication and evidence | `internal/git`, `internal/store`, `internal/export` | Branch/revision/lease checks or typed export checks fail before delivery. |

## Residual risk

Docker sandboxes share the host kernel and an internal network; runner login volumes are
readable inside runner sandboxes, Docker socket access is root-equivalent, and allowlisted
endpoints can still be misused. Disk limits are admission checks, not quotas. See [what the
sandbox does not cover](sandbox.md#what-the-sandbox-does-not-cover).

On a dedicated VM, systemd hardening reduces accidental host changes but does not isolate
the service account from its other data. Do not grant it sudo, and enforce egress with VM or
network policy because application egress allowlisting is absent in this mode.

In both models, session admissions are not dollar or subscription caps. Protect backups,
monitor unresolved workspaces and disk growth, and stop the service if credentials may be
compromised. Follow [deployment](deployment.md) for recovery and [SECURITY.md](../SECURITY.md)
for private disclosure.
