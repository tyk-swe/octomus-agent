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
   removes the container.
4. The broker reports the exit code, whether the memory limit killed it and whether a time
   limit stopped it. It then removes the container and revokes the lease.

Executors, fresh reviewers and repair turns each get a new sandbox. Persistent repair
threads resume from the runner session store (below), not from a live process. Each
verification command gets a fresh sandbox. Commands in one verification run share a home
directory, so `npm ci` in one command can serve `npm test` in the next. The home starts
empty at every new run.

OpenCode serves HTTP on the sandbox's own loopback. A helper inside the sandbox
(`octomus-agent --sandbox-init`) checks its readiness and relays its API as HTTP/2 over
the container's standard streams, so no network path exists between a sandbox and the
control plane or broker.

## What each session recorded

Task details show a sandbox record for every session and verification command:
- how many containers it ran in, and the image;
- whether the memory limit stopped one;
- the hosts the gateway let it reach, and those it refused, with counts.

A refused host is often the first sign of prompt injection, or of a registry missing from
`OCTOMUS_EGRESS_BUILD_HOSTS`.

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
- bounds open tunnels per sandbox, and logs every decision as a JSON line
  (`docker compose logs egress`).

Two allowlists come from the deployment's `.env`:

| Variable | Reachable from | Typical content |
| --- | --- | --- |
| `OCTOMUS_EGRESS_MODEL_HOSTS` | agent turns | `chatgpt.com,auth.openai.com,api.openai.com` for Codex; `models.opencode.ai` plus your providers for OpenCode |
| `OCTOMUS_EGRESS_BUILD_HOSTS` | agent turns and verification | package registries, for example `proxy.golang.org,sum.golang.org` or `registry.npmjs.org` |

Entries are host names or `*.suffix`, with an optional `:port` (default 443). List only
what the project needs. Every allowed host is a way out for data, and an allowlist is not
data-loss prevention: an allowed multi-tenant service, such as a package registry or a
model API used with someone else's key, can still carry data to an account that is not
yours.

## Resource limits

| Variable | Default | Meaning |
| --- | --- | --- |
| `OCTOMUS_SANDBOX_CPUS` | `2` | CPUs per sandbox |
| `OCTOMUS_SANDBOX_MEMORY` | `4g` | Memory per sandbox; swap is disabled |
| `OCTOMUS_SANDBOX_PIDS` | `1024` | Processes per sandbox |
| `OCTOMUS_SANDBOX_TMPFS` | `1g` | Size of `/tmp` |
| `OCTOMUS_SANDBOX_MAX` | `12` | Live sandboxes at once; more requests wait for a slot |
| `OCTOMUS_SANDBOX_RUNTIME` | Docker's default | Container runtime, for example `runsc` |

The worst case is `OCTOMUS_SANDBOX_MAX` × `OCTOMUS_SANDBOX_MEMORY`. A planning pass starts
eight to ten discovery agents at once, so size these for the host. A command killed by the
memory limit is recorded as such in its evidence. Sandbox logs are never kept by Docker,
so runner transcripts do not accumulate on the host's disk.

Volumes have no disk quota. Octomus checks application storage before admitting work (see
[configuration](configuration.md)); keep an eye on free space on a shared host.

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

**Check connection** runs the containment self-test before anything else. You can also run
it from the Sandbox panel on the Overview, or with `POST /api/sandbox/self-test`. A probe
sandbox checks, from inside:

| Check | What it observes |
| --- | --- |
| Runs as an unprivileged user | uid is not 0 |
| Holds no Linux capabilities | effective, permitted and bounding sets are empty |
| Cannot gain privileges through setuid programs | `no_new_privs` is set |
| System calls are filtered by seccomp | seccomp mode 2 |
| The image filesystem is read-only | writes to `/`, `/usr` and `/etc` fail |
| Cannot see Octomus state, secrets or the Docker socket | none of those paths exist |
| Has no direct route to the internet | direct TCP connections fail |
| Cannot resolve internet names directly | DNS lookups fail |
| Has no gateway to the host or its neighbours | there is no default route |
| Runs under memory and process limits | cgroup `memory.max` and `pids.max` are set |
| The egress gateway refuses unlisted, metadata and local targets | the gateway refuses `example.com`, `169.254.169.254` and `localhost` |

A failed check fails the connection check, names what the probe saw, and appears on the
Overview. The last result is kept with the image it ran on.

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
  sandbox loads. It can send data only through the egress allowlist.
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
- the broker against a real Docker daemon: containment from inside, kill and dead-man
  removal, memory-limit reporting, the OpenCode bridge with a 16 MB body and SSE, and
  leaving other containers alone (`OCTOMUS_DOCKER_TEST=1`);
- the shipped compose file end to end with fixture runners: the self-test, a full Run once
  delivery through sandboxes, and a control-plane crash that leaves no sandbox running
  (`make test-sandbox`).

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
