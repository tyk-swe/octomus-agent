# Model routing

Each of the four roles, five execution tiers, and repair route independently selects
Codex or OpenCode. A task snapshots its entire configuration when accepted; later
configuration edits do not change its executor, reviewer, or repair runner. Repair
turns reuse the task's repair session, while every review starts a fresh session.

## Configuration

Codex routes retain the existing model and effort fields:

```json
{"backend":"codex","model":"gpt-6-astra","effort":"medium"}
```

OpenCode routes specify a provider and its native model ID separately. Model IDs
can contain slashes. Select IDs and variant names from the runtime catalog rather
than translating Codex effort names:

```json
{"backend":"opencode","provider":"my-provider","model":"my-model","effort":"","variant":"high"}
```

The provider/model/variant in this example must exist in your host configuration.
Omit `variant` to explicitly use the provider's configured default settings. Models
without variants are supported. Codex routes cannot contain OpenCode provider or
variant fields; OpenCode routes must leave Codex effort empty. Unknown models and
unsupported efforts/variants block readiness and execution; no fallback is applied.

`codex_binary` defaults to `codex`; `opencode_binary` defaults to `opencode`. These
are executable paths, not shell commands or argument lists. Only runners selected
by required routes must be installed. An audit requires the orchestrator, discovery,
and proposal reviewer routes; normal execution requires every configured route.

Old configuration, task snapshots, sessions, and admission records without a
`backend` load as Codex. No migration is required. Existing saved workspace paths
remain valid, while newly accepted tasks use their Octomus task IDs for directories.

## OpenCode setup and behavior

Install [OpenCode 1.18.30](https://github.com/anomalyco/opencode/releases/tag/v1.18.30)
on the service host and authenticate/configure its providers as the service user.
The integration uses the [headless server](https://opencode.ai/docs/server/) and
the release's native HTTP/SSE API. Provider credentials, custom provider options,
and custom variants remain in OpenCode's user configuration. The dashboard stores
route selection and executable paths, never provider credentials. Project-local
OpenCode configuration is disabled for managed workers.

Octomus launches its own loopback servers with ephemeral authentication, using
owned process groups. Unexpected questions and permission requests are rejected
and block the task. Cancellation aborts the active session; process cleanup also
stops descendants. Server startup, requests, turns, events, and outputs are bounded.
At the whole-task deadline, cancellation is signalled first and cleanup may take
up to eight additional seconds so OpenCode can stop detached shell process groups.
The expired task remains blocked and cannot begin publication during cleanup.
Version mismatches are reported in connection diagnostics.

Temporary process configuration supplies Octomus worker instructions and disables
sharing, automatic updates, delegated/helper agents, automatic compaction, native
snapshots, formatters, and LSP processes. This keeps auxiliary work and model
selection under the orchestrator's control. Context exhaustion fails visibly;
automatic compaction does not choose another model. These controls remain
application policy, not a host sandbox. Existing full-diff review, immutable
verification, and orchestrator-owned GitHub publication gates still apply.

## Dashboard and API

**Load Codex models** and **Load OpenCode models** inspect the executable paths in
the current form, without saving it or starting a model turn. Select a runner,
provider where applicable, model, and supported effort/variant for each route.
Changing a runner/provider clears dependent selections; a model change clears
unsupported settings. Refreshing a catalog preserves saved values, including
unavailable choices, and displays validation messages. Saving incomplete drafts
is allowed; connection checks still require valid routes.

Authenticated `POST /api/model-catalog` accepts:

```json
{"backend":"opencode","binary":"/usr/local/bin/opencode"}
```

It returns an array with `backend`, `provider`, `provider_name`, `model`,
`display_name`, `efforts`, `variants`, `available`, and `unavailable_reason`.
OpenCode models require a configured provider and text/tool-calling capability.
Provider keys, options, authentication records, and raw transcripts are excluded.

`POST /api/doctor` and `POST /api/doctor?mode=audit` inspect required backends and
return per-backend versions, protocol baselines, normalized model catalogs and
warnings. Codex version fields remain present when Codex is required. Errors
identify the failing runner or route. Catalogs and diagnostics consume no session
admissions and make no model calls; they do not prove paid inference access.

## Verification

`make test` runs deterministic OpenCode HTTP/SSE fixtures, mixed-runner workflows,
restart recovery, legacy snapshots, publication gates, and browser tests. These
fixtures are synthetic and do not use accounts or model calls.

An optional check exercises the pinned CLI's actual startup, catalog, and session
creation/resumption in temporary XDG directories with a synthetic provider:

```bash
OCTOMUS_OPENCODE_SMOKE_BINARY=/absolute/path/to/opencode \
  cargo test --locked --test runners pinned_opencode_protocol_smoke_without_model_calls -- --ignored
```

This check makes no model calls and does not validate live operation. Live
commissioning remains on the owner's dedicated VM and bot.


CI additionally runs required pinned-client contracts for Codex 0.153.4 and
OpenCode 1.18.30. They start actual clients in isolated temporary state directories
against a synthetic loopback model provider, inspect Codex-generated request schemas
and OpenCode's `/doc`, and exercise a completed turn, structured review, resumed
session and cancellation. They use no operator credentials or live inference.

```bash
OCTOMUS_CONTRACT_CODEX_BINARY=/absolute/path/to/codex \
OCTOMUS_CONTRACT_OPENCODE_BINARY=/absolute/path/to/opencode \
  cargo test --locked --test contracts -- --ignored --nocapture
```

The synthetic provider validates client protocol behavior, not model quality,
account authentication or live tool behavior. Authenticated commissioning remains
separate.
