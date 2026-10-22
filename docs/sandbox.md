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

1. The control plane requests a sandbox of kind `runner` (agent turn or catalog check),
   `verify` (one verification command) or `probe` (self-test), naming only an owned root
   (task, planning role, baseline check or scratch root)—never an image, mount, capability,
   network, user or limit.
2. The broker validates the path without following symlinks, checks uid ownership, creates
   the container from its golden spec and grants an egress lease.
3. The broker attaches before starting so no output is lost. The control-plane stream is
   the lifeline: closing it (turn completion, cancellation or control-plane crash) kills
   and removes the container. The hard lifetime starts before Docker's start request.
4. The broker confirms lease revocation and container removal independently, then reports
   exit code, OOM state and time-limit status (`command`, `time_limit` or `failure`).
   Unremoved containers or unanswered creates retain their concurrency slot and fail
   closed; startup sweeps all containers carrying this instance's label. Login lease
   rotation likewise confirms removal of prior login leases before granting a new one.

Executors, fresh reviewers and repair turns each run in a new sandbox sharing the task's
home (per-user git config is ignored). Because earlier turns can alter shell startup files
in that home, the reviewer prompt embeds the authoritative change set computed by the
orchestrator's own git against `repo.git` (up to 32 KiB of `--numstat --summary` file list
and 64 KiB of smallest whole-file diffs, with non-UTF-8 and control/line-separator
characters escaped as `⟦…⟧`; see [architecture](architecture.md#review-verification-and-delivery)).

Each verification run uses a fresh clone of the reviewed commit; each command gets a fresh
sandbox sharing one empty verification home (so `npm ci` can feed `npm test`). If the
sandbox itself fails, the task blocks as `runner_unavailable` without spending a repair
round (or a baseline check ends interrupted), and any container that may have run is
recorded as a `sandbox_evidence` event, revoked and force-removed.

OpenCode serves loopback HTTP inside the sandbox; `octomus-agent --sandbox-init` checks
readiness and relays HTTP/2 over stdio, requiring no network path to the control plane.

## What each session recorded

Task details show a sandbox record for every session and verification command:
- how many containers it ran in, and the image;
- whether Docker reported an out-of-memory kill in one;
- the hosts the gateway let it reach, and those it refused, with counts.

A refused host is often the first sign of prompt injection, or of a registry missing from
`OCTOMUS_EGRESS_BUILD_HOSTS`. An allowlisted host the gateway could not reach is listed
apart, as unreachable.

Each list names at most 64 hosts and counts the rest under `other`; the gateway log names
further hosts (up to 1,024 per sandbox) the first time they appear. A session spanning
several sandboxes, such as a resumed repair thread, merges their lists under the same
limit.

A record is marked **incomplete** when the broker could not read part of it (the gateway's
count was unreachable or lost to a restart, or Docker did not report the memory-limit
outcome or confirm the start). Empty host lists in such a record do not mean the sandbox
made no connections; `docker compose logs egress` may still hold them.

The OOM flag comes from Docker's `State.OOMKilled`. On systemd-managed cgroup v2 hosts,
containerd can miss an OOM event if systemd removes the cgroup before its memory counters
are read ([containerd documents this limitation](https://github.com/containerd/containerd/blob/ee2735368117d2eb259779949d5e75cdafec9761/internal/cri/server/events.go#L210-L227)).
So `oom: false` does not rule out an OOM kill, even with `incomplete: false`, and exit
code 137 alone never sets the flag. The configured memory limit still applies.

Host names come from untrusted code, so they stay in private task records and the
dashboard and never appear in exported [run evidence](run-evidence.md). Planning sessions
and baseline commands carry the same record.

## What a sandbox can see

Mounts use the same absolute paths as in the control plane.

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
- bounds that inspection to 10 seconds, a 64 KiB ClientHello and 128 KiB of TLS records,
  replays the original bytes unchanged after validation, and closes the tunnel on a
  refusal after the `CONNECT` response;
- bounds open tunnels per sandbox and connections per source, closes every refused
  connection, and ends a sandbox's tunnels as soon as its lease is revoked;
- logs each tunnel's open and close and each refusal as JSON lines
  (`docker compose logs egress`); past 120 tunnels or 20 refusals a minute from one
  sandbox the rest are counted in one `suppressed` line, except the first line for a host
  its [record](#what-each-session-recorded) does not name.

Two allowlists come from the deployment's `.env`:

| Variable | Reachable from | Typical content |
| --- | --- | --- |
| `OCTOMUS_EGRESS_MODEL_HOSTS` | agent turns and [runner logins](#signing-in-a-runner) | `chatgpt.com,auth.openai.com,api.openai.com` for Codex; `models.opencode.ai` plus your providers for OpenCode |
| `OCTOMUS_EGRESS_BUILD_HOSTS` | agent turns, [runner logins](#signing-in-a-runner) and verification | package registries, for example `proxy.golang.org,sum.golang.org` or `registry.npmjs.org` |

Entries are host names or `*.suffix`, with an optional `:port` (default 443). An empty
`OCTOMUS_EGRESS_MODEL_HOSTS=` allows no model hosts; only a variable missing from `.env`
gets the Codex hosts above. List only what the project needs: an allowlist is not
data-loss prevention, and an allowed multi-tenant service, such as a package registry or
a model API used with someone else's key, can still carry data to an account that is not
yours.

The gateway does not terminate TLS or validate the upstream certificate, so clients remain
responsible for certificate verification. It also cannot see encrypted HTTP `Host` /
`:authority` values, so a shared endpoint that accepts an allowed SNI but routes a
different authority can still permit domain fronting. Tunnel audit names identify the
`CONNECT` authority confirmed against the plaintext SNI, not every encrypted application
destination. ECH is refused because its visible outer name cannot establish the encrypted
inner name.

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

This option configures runner sessions only, not the login service or verification
environment. A derived sandbox image may carry different system roots from the broker
image, so include any additional anchors in the supplied bundle, or leave this setting
unset and manage trust in the derived image. The gateway still enforces its allowlist,
destination-address and SNI checks and never terminates TLS: the file adds certificate
trust, not network destinations.

## Signing in a runner

`docker compose run --rm login …` runs Codex or OpenCode from the sandbox image to store a
login in the `octomus-runner` volume. Runner sandboxes can write that volume, so the login
is confined so that nothing they leave there gains reach:
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

Runner sandboxes can rewrite that file like everything else in the runner volume, so keep
your copy and reinstall it whenever in doubt.

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

The worst case is `OCTOMUS_SANDBOX_MAX` × `OCTOMUS_SANDBOX_MEMORY`, and a planning pass
starts eight to ten discovery agents at once, so size these for the host. Docker keeps no
sandbox logs, so runner transcripts do not accumulate on the host's disk.

The Overview displays the broker's hard lifetime. In Docker mode, saving settings,
connection checks, execution preflights and every new runner connection (including later
turns of a task in progress) reject a session timeout above it, or a command timeout whose
additional 60-second shutdown grace does not fit; the broker refuses such a request rather
than shortening it. Raise the host-owned value in `.env` and recreate `sandboxd`, or lower
the saved timeout, leaving margin for runner startup. At expiry the broker tears the
sandbox down within a separate 45-second budget, and a daemon that cannot confirm removal
leaves an infrastructure failure and keeps the slot reserved while removal is retried. The
task timeout may span several containers, and host mode keeps the ordinary timeout range
without a broker cap.

Volumes have no disk quota. Octomus checks application storage before admitting work
(see [configuration](configuration.md) and
[architecture](architecture.md#bounded-observation-and-storage)); keep an eye on free
space on a shared host. Owned-root cleanup is bounded in time, entries and depth, never
follows symlinks and reports incomplete cleanup rather than marking a record discarded
until its root is removed. An excessively deep tree may need simplifying before a retry.

## Trusted git metadata

Each owned clone keeps its git metadata in `repo.git` beside the work tree, which holds
only a `.git` pointer file. The sandbox mounts `repo.git` read-only: agents can run
`git status`, `git diff` and `git log` but cannot commit, change configuration or plant
hooks, and the orchestrator commits their changes. Every orchestrator git command names
`repo.git` explicitly and pins hooks, fsmonitor, the untracked cache and submodule
recursion off, so replacing the pointer file changes nothing it reads. A snapshot that
would add or move a submodule entry is refused, because it would publish content nobody
reviewed.

Planning agents inspect fork pull requests by SHA, which the orchestrator fetches into the
trusted checkout before cloning, since sandboxes have no route to GitHub.

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

For the unlisted-name check, the broker asks the running gateway's collector for a denied
target (`example.com` when unlisted, otherwise a reserved `.invalid` name). A networked
self-test requires a reachable `OCTOMUS_EGRESS_COLLECTOR` and fails before starting the
probe without one.

A failed check fails the connection check, names what the probe saw, and appears on the
Overview. The last result is kept with a fingerprint of the image, broker process,
Docker/API versions, runtime, limits, networks, live gateway identity and policy, and
deployment settings, read fresh from the live broker and the gateway's local collector
before and after the run. A change during the check cannot produce current proof, and a
broker or gateway restart (even of the gateway alone), changed settings or an unavailable
collector invalidates old proof and requires a new self-test.

## Extend the sandbox image

The shipped sandbox image contains Codex 0.163.0, OpenCode 1.19.4, Git, Bash, Python 3,
Node 24 LTS and common build tools. Add your project's toolchain in a derived image:

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
  images with both real pinned clients (`make test-production-images`; see
  [releasing](https://github.com/tyk-swe/octomus-agent/blob/main/docs/releasing.md#container-images)).

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
