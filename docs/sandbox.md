# Sandbox

Octomus runs every agent turn and every verification command in its own short-lived
container. The control plane, which holds your GitHub token, the operator token and
Octomus state, never runs repository code or model-directed commands. This page describes
what that guarantees, how each layer enforces it, how to check it yourself, and what it
does not cover.

Sandboxing is the default: the service starts with `--sandbox docker` unless you choose
`--sandbox off` for a [dedicated VM](#unsandboxed-mode).

## What the Docker deployment guarantees

1. **Untrusted code never runs in the control plane or on the host.** Agent tool use,
   repository build scripts and verification commands run only in sandbox containers.
2. **No sandbox holds a credential it could publish with.** The GitHub token, the operator
   token, the webhook URL and `state.db` stay in the control plane. Only the control plane
   pushes branches or opens pull requests.
3. **A sandbox sees only its own root.** Its work tree is writable, its git metadata is
   read-only, and it has a home directory. Other tasks, the trusted checkout and Octomus
   state are not mounted.
4. **A sandbox lives for one agent turn or one verification command.** Every process an
   agent started is gone before the orchestrator reviews, commits or verifies the work tree.
5. **The orchestrator never runs repository-controlled git configuration.** Trusted git
   metadata lives outside anything a sandbox can write, and every orchestrator git command
   pins hooks, fsmonitor and submodule behaviour off.
6. **Egress is deny-by-default and enforced outside the sandbox.** Sandboxes sit on internal
   networks with no route out and no gateway to the host. Their only exit is the egress
   gateway, which opens HTTPS tunnels only to allowlisted hosts with public addresses.
7. **Resources are bounded.** Each sandbox has CPU, memory (no swap), process and `/tmp`
   limits. The number of live sandboxes is capped, and the kernel kills sandboxes first
   under memory pressure, so other services on the host keep running.
8. **Only the broker touches the Docker socket.** It has no network, builds every container
   spec itself from a narrow request, and only touches containers carrying its own label.
9. **The deployment, not the operator token, owns the security posture.** Sandbox mode,
   egress allowlists, limits, the sandbox image, the repository and the runner programs
   cannot be changed from the dashboard or API.
10. **You can prove it.** The self-test runs containment probes from inside a real sandbox
    and shows every result.

## How it fits together

```text
VPS · Docker Engine ≥ 28
├─ octomus    control plane: state, scheduler, dashboard (127.0.0.1 only), GitHub publication
│      │ unix socket: one request and one stream per sandbox
├─ sandboxd   broker: the only holder of the Docker socket, network_mode none
│      │ Docker Engine API
├─ sandbox-*  one per agent turn or verification command; non-root, no capabilities,
│             read-only image, limits, internal network only
└─ egress     gateway: the only way out, CONNECT-only allowlist per sandbox kind
```

All three services run from one image and differ only by command
(`--sandbox docker`, `--sandboxd`, `--egress`). Each runs as uid 10001 with a read-only
filesystem, no capabilities and `no-new-privileges`. See
[deploy/docker/compose.yaml](../deploy/docker/compose.yaml).

## One sandbox's life

1. The control plane asks the broker for a sandbox of one **kind**: `runner` (a Codex or
   OpenCode turn, or a route catalog check), `verify` (one verification command) or `probe`
   (the self-test). It names an **owned root**: a task, a planning role, a baseline check,
   or an empty scratch root. It cannot name an image, mount, capability, network, user or
   limit.
2. The broker checks the request against fixed patterns, walks the root without following
   symlinks, and requires every directory to be owned by the sandbox user. It then creates
   the container from its golden spec and grants an egress lease.
3. The broker attaches to the container before starting it, so no output is lost. The
   control plane's stream to the broker is the sandbox's lifeline: if it closes, because the
   turn ended, the operator cancelled or the control plane crashed, the broker kills and
   removes the container. The hard lifetime starts before the broker sends Docker's start
   request. A delayed start response still consumes that lifetime, and cannot block kill
   requests or a closed lifeline. A kill that arrived before Docker started the container
   is applied again once its start is confirmed.
4. The broker revokes the lease and removes the container, then reports the exit code,
   whether Docker reported an out-of-memory kill and whether a time limit stopped it. A
   sandbox it could not remove keeps its slot until a retry succeeds, and its report says so.

Executors, fresh reviewers and repair turns each get a new sandbox. Persistent repair
threads resume from the runner session store (below), not from a live process. These turns
share the task's home, so what one leaves there, shell startup files included, reaches the
next; only git's own per-user configuration in it is ignored. Because what a turn leaves
can still change what git shows inside the next one, the fresh reviewer's prompt also
carries the change set as Octomus's own git reads it from the trusted metadata. That
account ignores every `.gitattributes` file, shows every file as text and every submodule
entry as the commits it points at, and shows each control, invisible or line-separator
character and each byte that is not UTF-8 as a `⟦…⟧` escape, so no statement hides behind
a line break git does not split at. It lists every changed file with its line counts, and
a change set whose list exceeds 32 KiB is not reviewed. Within 64 KiB it embeds the
complete diff, or else the whole diffs of the smallest files, and names each file whose
diff it leaves out: the reviewer reads those in its sandbox and treats them as unverified.
The account is authoritative over what git in the reviewer's sandbox shows, apart from the
differences the prompt names as expected. Each verification run starts from a fresh clone
of exactly the reviewed commit, so nothing an agent left beside it can influence the
result. Each verification command gets a fresh sandbox. Commands in one verification run share a home
directory, so `npm ci` in one command can serve `npm test` in the next. The home starts empty at every new run.
When the sandbox rather than the command fails (the broker refuses or loses the command,
or cannot confirm how it ended), the command has no result. The task is blocked as
`runner_unavailable` for a retry, with no verification record and no repair round spent,
and a baseline check ends interrupted rather than failed. If the command ran before the
sandbox failed, the broker's sandbox record is kept as a `sandbox_evidence` event. A start
whose response was lost or exceeded the hard lifetime may also have run the command:
the broker preserves an incomplete record, revokes its lease and force-removes its known
container ID. It reports an infrastructure failure without inventing a command exit.

OpenCode serves HTTP on the sandbox's own loopback. A helper inside the sandbox
(`octomus-agent --sandbox-init`) checks its readiness and relays its API as HTTP/2 over
the container's standard streams, so no network path exists between a sandbox and the
control plane or broker.

## What each session recorded

Task details show a sandbox record for every session and verification command:
- how many containers it ran in, and the image;
- whether Docker reported an out-of-memory kill in one;
- the hosts the gateway let it reach, and those it refused, with counts.

A refused host is often the first sign of prompt injection, or of a registry missing from
`OCTOMUS_EGRESS_BUILD_HOSTS`. An allowlisted host the gateway could not reach (a failed
lookup or connection, or too many open tunnels) is listed apart, as unreachable.

Each list names at most 64 hosts and counts the rest under `other`. Within one sandbox, the
gateway log names each host past a list's first 64 the first time it appears, outside the
log's per-minute budget, for up to 1,024 such hosts per sandbox. A session whose turns ran
in several sandboxes, such as a resumed repair thread, merges their lists under the same
limit, so its list can count under `other` a host one of those sandboxes named; the log
names that host only where its per-minute budget allowed.

When the broker could not read part of a sandbox's record, the record is marked
**incomplete**: the gateway's count was unreachable or lost to a gateway restart, or
Docker did not say whether the memory limit killed a process or confirm the start. Empty
host lists in an incomplete record do not mean the sandbox made no connections; `docker compose logs
egress` may still hold them.

The OOM flag comes from Docker's `State.OOMKilled`. On systemd-managed cgroup v2 hosts,
containerd can miss an OOM event if systemd removes the cgroup before its memory counters
are read; [containerd documents this reporting limitation](https://github.com/containerd/containerd/blob/ee2735368117d2eb259779949d5e75cdafec9761/internal/cri/server/events.go#L210-L227).
Consequently, `oom: false` does not rule out an OOM kill, even with `incomplete: false`:
the incomplete flag records collection failures the broker can detect. Exit code 137
alone never sets the OOM flag. This reporting limitation does not disable the configured
memory limit.

Host names come from untrusted code, so they stay in private task records and the
dashboard. They are never part of exported [run evidence](run-evidence.md). Planning
sessions and baseline commands carry the same record.

## What a sandbox can see

Mounts use the same absolute path inside the sandbox as in the control plane, so runner
working directories and OpenCode session checks line up.

| Mount | runner | verify | probe |
| --- | --- | --- | --- |
| The root's work tree | read-write | read-write | — |
| The root's trusted git metadata (`repo.git`) | read-only | read-only | — |
| Home | the root's own, kept for the task | one per verification run | tmpfs |
| Runner login and sessions (`octomus-runner` volume) | read-write | — | — |
| The broker's helper binary | read-only | read-only | read-only |
| `/tmp` | tmpfs, executable, sized | same | same |

The image itself is read-only. Environment variables are built from scratch by the
broker; nothing is inherited from the control plane.

## Network

Sandboxes join `octomus-sandbox-runner` or `octomus-sandbox-verify`. Both are internal
Docker networks with `gateway_mode_ipv4=isolated`: no route out, no external DNS, and no
gateway address through which to reach services on the host. The broker refuses to start
if either network lacks these settings.

Each sandbox receives proxy variables carrying its own random credential. The gateway:

- accepts only HTTPS `CONNECT` tunnels, and only from a live lease;
- checks the host against the allowlist **before** any DNS lookup, so a refused name never
  reaches DNS;
- refuses IP literals, and names that resolve only to loopback, private, link-local
  (including cloud metadata), carrier-grade NAT, documentation, benchmark, reserved or
  IPv4-embedding IPv6 addresses;
- dials the address it checked, so DNS rebinding cannot redirect the tunnel;
- inspects the initial TLS ClientHello before forwarding any client bytes and requires
  its plaintext SNI name to match the `CONNECT` host. Missing, malformed, duplicate or
  mismatched names and Encrypted ClientHello (ECH, including GREASE) are refused. Clients
  must use plaintext SNI with ECH disabled;
- bounds that inspection to 10 seconds, a 64 KiB ClientHello and 128 KiB of encoded TLS
  records. It accepts fragmented records and TCP writes and replays the original bytes
  unchanged after validation. A refusal after the `CONNECT` response closes the tunnel;
- bounds open tunnels per sandbox and connections per source, closes every refused
  connection, and ends a sandbox's tunnels as soon as its lease is revoked;
- logs each tunnel as a JSON line when it opens and again when it closes, and each refusal
  (`docker compose logs egress`); past 120 tunnels or 20 refusals a minute from one
  sandbox, the rest are counted in a single `suppressed` line instead, except the first
  line for a host its [record](#what-each-session-recorded) does not name. Docker rotates
  that log, and the gateway itself runs under memory and process limits.

Two allowlists come from the deployment's `.env`:

| Variable | Reachable from | Typical content |
| --- | --- | --- |
| `OCTOMUS_EGRESS_MODEL_HOSTS` | agent turns and [runner logins](#signing-in-a-runner) | `chatgpt.com,auth.openai.com,api.openai.com` for Codex; `models.opencode.ai` plus your providers for OpenCode |
| `OCTOMUS_EGRESS_BUILD_HOSTS` | agent turns, [runner logins](#signing-in-a-runner) and verification | package registries, for example `proxy.golang.org,sum.golang.org` or `registry.npmjs.org` |

Entries are host names or `*.suffix`, with an optional `:port` (default 443). An empty
`OCTOMUS_EGRESS_MODEL_HOSTS=` allows no model hosts; only a variable missing from `.env`
gets the Codex hosts above. List only what the project needs. Every allowed host is a way
out for data, and an allowlist is not data-loss prevention: an allowed multi-tenant
service, such as a package registry or a model API used with someone else's key, can still
carry data to an account that is not yours.

The gateway binds the initial [SNI](https://www.rfc-editor.org/rfc/rfc6066.html#section-3)
to the allowlisted `CONNECT` authority and vets its resolved addresses. It does not
terminate TLS, validate the upstream certificate, or inspect encrypted HTTP `Host` /
`:authority` values. Clients remain responsible for certificate verification. A shared
endpoint that accepts an allowed SNI but routes a different encrypted HTTP authority can
still permit domain fronting; allowing a multi-tenant endpoint also allows its supported
accounts and services. Tunnel audit names identify the `CONNECT` authority, confirmed
against the initial plaintext SNI, and do not attest to every encrypted application
destination. [ECH](https://www.rfc-editor.org/rfc/rfc9849.html) is refused because the
visible outer name cannot establish the encrypted inner name. The inspection follows
the [TLS record and handshake framing](https://www.rfc-editor.org/rfc/rfc8446.html#section-5.1)
without rewriting either.

## Private provider certificates

For a model endpoint signed by a private CA, the host operator can configure
`OCTOMUS_SANDBOX_CA_FILE` on the broker. Mount a regular PEM file outside the data,
runner and tools volumes; keep it under administrator control and readable by uid
10001. For example, add this override alongside `compose.yaml`:

```yaml
services:
  sandboxd:
    environment:
      OCTOMUS_SANDBOX_CA_FILE: /run/octomus-ca/provider.pem
    volumes:
      - type: bind
        source: /etc/octomus/provider-ca.pem
        target: /run/octomus-ca/provider.pem
        read_only: true
```

The broker refuses symlinks in any path component, special files, bundles over
1 MiB, malformed PEM, private keys and certificates without CA constraints. At
startup it combines the supplied certificates with **the broker image's system
roots** in `/opt/octomus/ca-certificates.pem`, on the tools volume that sandboxes
mount read-only. Runner sessions receive `SSL_CERT_FILE` and
`NODE_EXTRA_CA_CERTS` pointing to that bundle. A request cannot select another trust
file or set those variables. Removing the setting and restarting the broker removes
the generated bundle.

This option configures runner sessions; it does not modify the login service or
verification environment. A derived sandbox image may contain different system
roots from the broker image; include any required additional anchors in the supplied
bundle. Alternatively, leave this setting unset and manage client trust in the
derived image. The gateway still enforces its existing
allowlist, destination-address and SNI checks, and never terminates TLS. The file
adds certificate trust, not network destinations.

## Signing in a runner

`docker compose run --rm login …` runs Codex or OpenCode from the sandbox image to store a
login in the `octomus-runner` volume. Runner sandboxes can write that volume, so the login
gives nothing they leave there more reach than they have:
- it joins `octomus-sandbox-runner` and reaches out only through the egress gateway, under
  the runner allowlist. Just before it starts, a one-shot `login-lease` service with no
  network runs `octomus-agent --login-lease`, which grants it a lease and revokes the
  previous login's. That lease stays valid until the next login or until the broker
  restarts;
- it mounts only the Codex home and OpenCode's data directory, where the logins live.
  OpenCode's configuration, plugins, cache and state start empty;
- its image is read-only, and it runs without capabilities, under memory and process limits.

Codex's sign-in hosts are in the default allowlist. For OpenCode, first add
`models.opencode.ai` and your provider's sign-in host to `OCTOMUS_EGRESS_MODEL_HOSTS` and run
`docker compose up -d`; `docker compose logs egress` shows any host a login was refused.

OpenCode's user configuration (`opencode.json`, with custom providers, provider options and
variants) is read by runner sandboxes from `opencode/config` in the `octomus-runner` volume,
which the login does not mount. To install or replace it, run this from the directory that
holds your `opencode.json`, after `docker compose up -d` has let the broker create that
directory:

```sh
docker run --rm -i --network none --read-only --user 10001:10001 \
  -v octomus-runner:/runner --entrypoint tee octomus-sandbox:local \
  /runner/opencode/config/opencode.json < opencode.json > /dev/null
```

Runner sandboxes can rewrite that file like everything else in the runner volume (see
below), so keep your copy and install it again whenever in doubt.

## Resource limits

| Variable | Default | Meaning |
| --- | --- | --- |
| `OCTOMUS_SANDBOX_CPUS` | `2` | CPUs per sandbox |
| `OCTOMUS_SANDBOX_MEMORY` | `4g` | Memory per sandbox; swap is disabled |
| `OCTOMUS_SANDBOX_PIDS` | `1024` | Processes per sandbox |
| `OCTOMUS_SANDBOX_TMPFS` | `1g` | Size of `/tmp` |
| `OCTOMUS_SANDBOX_MAX` | `12` | Live sandboxes at once; more requests wait for a slot |
| `OCTOMUS_SANDBOX_MAX_SECONDS` | `21600` (6 hours) | Hard lifetime of each container, including runner startup; 60–604800 seconds |
| `OCTOMUS_SANDBOX_RUNTIME` | Docker's default | Container runtime, for example `runsc` |

The worst case is `OCTOMUS_SANDBOX_MAX` × `OCTOMUS_SANDBOX_MEMORY`. A planning pass starts
eight to ten discovery agents at once, so size these for the host. Evidence of memory-limit
kills depends on [Docker's OOM reporting](#what-each-session-recorded). Sandbox logs are
never kept by Docker, so runner transcripts do not accumulate on the host's disk.

The Overview displays the broker's hard lifetime. In Docker mode, saving settings,
connection checks and execution preflights reject a session timeout above that limit,
or a command timeout whose additional 60-second verification shutdown grace does not
fit. Every new runner connection rechecks the live limit, including later turns of a
task already in progress. A non-probe request above the limit is refused by the broker instead of silently
shortened; internal diagnostic probes remain capped. Raise the host-owned value in
`.env` and recreate `sandboxd`, or lower the saved timeout. Runner startup consumes part
of a container's hard lifetime, so leave margin when choosing a session timeout.
At expiry, the broker begins teardown under a separate 45-second budget. A daemon that
cannot confirm removal leaves an infrastructure failure and keeps the sandbox's capacity
reserved while removal is retried; cancellation of an HTTP request is not proof that the
daemon stopped its process.
The task timeout may span several containers and need not fit one container's limit.
Host mode retains the application's ordinary timeout range without a broker cap.

Volumes have no disk quota. Octomus checks application storage before admitting work (see
[configuration](configuration.md)); keep an eye on free space on a shared host.
The storage walk keeps at most three directory descriptors open and bounds repeated
ancestor traversal per owned workspace. An unreadable, excessively deep or expensive
subtree blocks further admission for its owner without blocking unrelated workspaces.
Owned-root cleanup has its own 30-second attempt deadline, checks cancellation between
filesystem operations, and bounds entries, depth and repeated ancestor traversal. It
uses at most three directory descriptors, repairs read-only directories without following
symlinks, and removes long paths one component at a time. A bound, cancellation, changed
directory or permission failure reports incomplete cleanup and leaves the remaining root
for retry; a record is not marked discarded until its root is removed. Excessive depth
may require the operator to simplify the tree before retrying.

## Trusted git metadata

Each owned clone keeps its git metadata in `repo.git` beside the work tree. The work tree
holds only a `.git` pointer file, so tools inside the sandbox still find the history. The
sandbox mounts `repo.git` read-only: agents can run `git status`, `git diff` and `git log`,
but cannot commit, change configuration or plant hooks. The orchestrator commits their
changes.

Every orchestrator git command on a work tree names `repo.git` explicitly and pins hooks,
fsmonitor, the untracked cache and submodule recursion off. Replacing the pointer file with
a directory or a link changes nothing the orchestrator reads. A snapshot that would add or
move a submodule entry is refused, because it would publish content nobody reviewed.

Planning agents inspect fork pull requests by SHA: the orchestrator fetches those heads into
the trusted checkout before cloning, since sandboxes have no route to GitHub.

## What the operator token can change

The token is still an operator capability. It can change policy, choose model routes, set
verification commands (which run in sandboxes), start work and cause pull requests to be
published once review and verification pass.

In the Docker deployment it **cannot**:
- turn the sandbox off;
- widen egress;
- raise limits;
- choose the image;
- point the orchestrator's own git at another directory;
- choose which runner program starts.

Those settings are host-owned, and the dashboard shows them read-only.

## Prove it: the self-test

Proof is tied to the live broker and egress gateway identities and their reported policy.
Each broker info request reads the gateway's boot identity and actual allowlist fingerprint
from its local collector. Recreating only the gateway therefore invalidates earlier proof,
even if the control plane still displays its original deployment environment. If the
collector is unavailable or too old to report its identity, networked proof is noncurrent;
run the self-test again after the gateway is available.

**Check connection** runs the containment self-test before anything else. You can also run
it from the Sandbox panel on the Overview, or with `POST /api/sandbox/self-test`. A probe
sandbox checks, from inside:

| Check | What it observes |
| --- | --- |
| Runs as an unprivileged user | uid is not 0 |
| Holds no Linux capabilities | effective, permitted and bounding sets are empty |
| Cannot gain privileges through setuid programs | `no_new_privs` is set |
| System calls are filtered by seccomp | seccomp mode 2 |
| The image filesystem is read-only | the root mount is `ro`, and writes to `/`, `/usr` and `/etc` fail with a read-only filesystem error, or are refused for permission under a mount that is itself `ro` (gVisor checks permissions first) |
| Cannot see Octomus state, secrets or the Docker socket | none of those paths exist |
| Has no direct route to the internet | direct TCP connections get no answer, not even a refusal |
| Cannot resolve internet names directly | DNS lookups fail |
| Has no gateway to the host or its neighbours | there is no default route, and the first address of the sandbox's subnet, where a bridge gateway would sit, answers neither ARP nor a connection on common host ports unless Docker names it as a container |
| Runs under memory and process limits | cgroup `memory.max` and `pids.max` (on cgroup v1, `memory.limit_in_bytes` and `pids.max`) match the broker's configured limits |
| The egress gateway refuses unlisted, metadata and local targets | with a runner lease, the gateway refuses a name outside its effective model and build allowlists, and `169.254.169.254` and `localhost` as not host names |

For the unlisted-name check, the broker asks the running gateway's local collector for a
denied target: `example.com` when unlisted, otherwise a reserved `.invalid` name outside
both allowlists. A networked self-test requires a reachable `OCTOMUS_EGRESS_COLLECTOR`;
without the gateway's policy-selected target, it fails before starting the probe.

A failed check fails the connection check, names what the probe saw, and appears on the
Overview. The last result is kept with a fingerprint of the image, broker process,
Docker/API versions, runtime, limits, networks, live gateway identity and policy, and
deployment settings. The probe reads fresh posture before and after its run; a change
during the check cannot produce current proof. A broker or gateway restart, changed
settings, or unavailable gateway invalidates old proof and requires a new self-test.
Legacy saved results without a fingerprint remain readable but do not count as current
containment evidence.

## Extend the sandbox image

The shipped sandbox image contains Codex 0.153.4, OpenCode 1.18.30, Git, Bash, Python 3,
Node 22 and common build tools. Add your project's toolchain in a derived image:

```dockerfile
FROM octomus-sandbox:local
USER root
RUN apt-get update \
 && apt-get install -y --no-install-recommends golang-go \
 && rm -rf /var/lib/apt/lists/*
USER 10001:10001
```

Build it, set `OCTOMUS_SANDBOX_IMAGE` in `.env` to its tag, and run `docker compose up -d`.
`docker compose build octomus sandbox-image` rebuilds only the base, `octomus-sandbox:local`,
so rebuild your image after each upgrade to pick up the new runners.
The broker uses only images already present on the host and never pulls. Baking toolchains
and warm caches into the image also keeps build hosts off the allowlist. Never put
credentials in the image; every sandbox can read it.

## Stronger kernel isolation

Containers share the host kernel. For a stronger boundary, install
[gVisor](https://gvisor.dev/docs/user_guide/install/), register `runsc` with Docker, and set
`OCTOMUS_SANDBOX_RUNTIME=runsc`. The self-test then runs under gVisor as well. gVisor
support has not been validated with Octomus yet.

## What the sandbox does not cover

- **Runner logins are readable inside runner sandboxes.** Runner sandboxes share the
  runner volume: the Codex or OpenCode login, and every session's transcript. A
  prompt-injected session can read them, and can leave runner configuration that a later
  sandbox loads; a later [login](#signing-in-a-runner) still reads the Codex configuration
  there. Either can send data only through the egress allowlist.
- **Sandboxes on the same network can reach each other's listening ports.** Nothing else on
  the host is reachable.
- **Docker socket access is root-equivalent.** The broker's validation is the boundary
  between the control plane and the host. Keep the broker's image and configuration under
  your control.
- **Kernel exploits are out of scope for runc.** Use gVisor if that matters to you.
- **Prompt injection can still produce harmful pull request content.** Full-diff review,
  verification and your merge decision remain the gate.

## Unsandboxed mode

`--sandbox off` (or `OCTOMUS_SANDBOX=off`) runs runners and verification commands directly
as the service user, as earlier releases did. Use it only on a dedicated VM that holds
nothing else. The dashboard shows a permanent **Unsandboxed** warning in this mode. The
supplied systemd unit uses it; see [deployment](deployment.md#dedicated-vm-without-a-sandbox).
A GitHub token file or a pinned repository is refused in this mode, because unsandboxed
runners would inherit the one and could rewrite the other.

## Checked so far, and what is still yours to check

Automated tests hold:
- the golden container spec for each sandbox kind, the broker's request validation and the
  gateway's policy (default suite);
- the broker against a real Docker daemon: containment from inside, runner stdio streams
  and the containment probe passing (`OCTOMUS_DOCKER_TEST=1`);
- the shipped compose file end to end with fixture runners: the self-test and a full Run
  once delivery through sandboxes (`make test-sandbox`);
- required production image acceptance on native amd64 and arm64: the actual shipped
  control-plane, broker, gateway and sandbox images, with both real pinned clients
  (`make test-production-images`, given the retained `dist/images/*.oci.tar` inputs).

The production image job builds each multi-platform OCI archive once with its SBOM
and provenance. Each native runner imports the corresponding manifest without
rebuilding, boots the embedded dashboard, and exercises real Codex and OpenCode
completed turns, structured output, persisted-session resume in a fresh container,
cancellation, verification and all eleven containment checks. A synthetic HTTPS
provider supplies deterministic responses through the normal leased CONNECT gateway;
no live provider account is used. The host inspects each of the four runner containers
before allowing its turn to continue, verifies its exact image, network and mounts,
and records its distinct container ID. Direct connections to the fixture's address
must fail from both runner and verification containers. Complete broker evidence,
provider CONNECT counts, and zero remaining sandboxes and leases are required.

This acceptance needs a disposable Docker Engine 28+ host with Compose, Skopeo and
OpenSSL. Its synthetic provider lives on a separate internal bridge using
`11.255.254.0/24`, a globally numbered range reserved locally for the test; it does not
contact the public owner of those addresses. The gateway is the only deployment
service that also joins that bridge, and the provider publishes no host port. Using a globally classified address lets
the test retain the production gateway's private-address denial. Temporary keys,
provider state and volumes are removed at teardown. Release promotion verifies the
retained native receipts and publishes the same index and manifest digests; see
[releasing](releasing.md#container-images).

Development checks on Docker Engine 29.8.1 found:
- A plain internal network lets a container reach the host's SSH through its gateway, which
  is why the isolated gateway is required.
- The pinned Codex and OpenCode clients send the sandbox's proxy credential.
- OpenCode fetches its catalog from `models.opencode.ai`.

None of this is live validation. Before relying on it, commission the deployment on your own
VPS:
1. Run `./setup.sh`, then **Check connection**, and confirm all eleven checks pass.
2. Sign in a runner with the `login` service and run one real audit. Confirm
   `docker compose logs egress` shows only the model hosts you expect.
3. Run once with a verification command that needs a registry, and adjust
   `OCTOMUS_EGRESS_BUILD_HOSTS` until it passes with nothing extra.
4. Confirm the GitHub token is fine-grained, limited to one repository, and cannot bypass
   branch protection.
