# Slack and Discord chat

Octomus can live in your Slack workspace and Discord server as a bot you
talk to: ask for status, file work, pause or resume repositories, and
receive the attention alerts there instead of (or alongside) the webhook.
Both integrations are optional, disabled by default, and operator-only:
only the chat user IDs you allowlist can drive Octomus, and everything a
command does goes through the same engine actions, planning gates,
budgets and lifecycle rules as the dashboard. Filing work from chat files
an ordinary work request — planning still reviews it like any discovered
work.

Both transports are outbound-only (Slack Socket Mode, the Discord
Gateway), so the private listener model is unchanged: no public request
URL is exposed and the dashboard stays on loopback. The control plane
needs outbound HTTPS/WSS to `slack.com` and/or `discord.com`, which both
supplied deployments already have for GitHub.

**Trust note.** An allowlisted chat account has operator authority.
Anyone who can act as that Slack or Discord user can pause repositories,
file requests and cancel tasks (state-changing commands ask for an
in-chat confirmation first). Treat those accounts like the operator
token: strong authentication, and remove IDs here the moment an account
is retired.

## Slack setup

1. [Create a Slack app](https://api.slack.com/apps?new_app=1) in your
   workspace (from scratch is fine).
2. Under **Settings → Socket Mode**, enable Socket Mode and create an
   app-level token with the `connections:write` scope. This is
   `OCTOMUS_SLACK_APP_TOKEN` (`xapp-…`).
3. Under **OAuth & Permissions**, add bot scopes `commands`,
   `chat:write`, and (only for @mentions) `app_mentions:read`. Install
   the app to the workspace and copy the bot token
   (`OCTOMUS_SLACK_BOT_TOKEN`, `xoxb-…`).
4. Under **Slash Commands**, create `/octomus`. With Socket Mode there is
   no request URL to fill in.
5. Under **Interactivity & Shortcuts**, turn interactivity on (Socket
   Mode carries the button clicks). To use @mentions, under **Event
   Subscriptions** subscribe the bot to `app_mention`.
6. Invite the app to the channels you will use (`/invite @Octomus`) and
   find the channel IDs (channel details → copy ID) and your own user ID
   (profile → ⋮ → Copy member ID).

## Discord setup

1. Create an application in the
   [Discord developer portal](https://discord.com/developers/applications)
   and note the application ID (`OCTOMUS_DISCORD_APP_ID`).
2. Under **Bot**, add a bot and copy its token
   (`OCTOMUS_DISCORD_BOT_TOKEN`). No privileged gateway intents are
   needed; turn them off. Octomus never reads message content.
3. Under **OAuth2 → URL Generator**, select scopes `bot` and
   `applications.commands` with permissions to view channels and send
   messages, and invite the bot to your server.
4. Enable Developer Mode (User Settings → Advanced) to copy IDs: your
   user ID, the server (guild) ID, and the channel IDs you will use.
   Octomus registers `/octomus` on startup — as a guild command when
   `OCTOMUS_DISCORD_GUILD_ID` is set (instant), otherwise globally
   (can take up to an hour to appear).

## Configuration

Everything is host-level environment, like the other deployment secrets;
nothing is dashboard-editable. Tokens accept the `*_FILE` secret-file
forms (`OCTOMUS_SLACK_APP_TOKEN_FILE`, `OCTOMUS_SLACK_BOT_TOKEN_FILE`,
`OCTOMUS_DISCORD_BOT_TOKEN_FILE`). A provider is enabled when its tokens
are set; the operator allowlist is then mandatory and startup refuses to
run chat without one.

| Variable | Meaning |
| --- | --- |
| `OCTOMUS_SLACK_APP_TOKEN`, `OCTOMUS_SLACK_BOT_TOKEN` | Slack app-level and bot tokens |
| `OCTOMUS_SLACK_OPERATOR_IDS` | Comma-separated Slack user IDs allowed to command Octomus |
| `OCTOMUS_SLACK_CHANNELS` | Comma-separated `CHANNEL_ID=OWNER/REPOSITORY` bindings |
| `OCTOMUS_SLACK_ALERT_CHANNEL` | Channel for proactive alerts and the digest |
| `OCTOMUS_DISCORD_BOT_TOKEN`, `OCTOMUS_DISCORD_APP_ID` | Discord bot token and application ID |
| `OCTOMUS_DISCORD_GUILD_ID` | Guild for instant command registration (recommended) |
| `OCTOMUS_DISCORD_OPERATOR_IDS` | Comma-separated Discord user IDs allowed to command Octomus |
| `OCTOMUS_DISCORD_CHANNELS` | Comma-separated `CHANNEL_ID=OWNER/REPOSITORY` bindings |
| `OCTOMUS_DISCORD_ALERT_CHANNEL` | Channel for proactive alerts and the digest |
| `OCTOMUS_CHAT_DIGEST_TIME` | Daily digest time `HH:MM`; empty disables the scheduled digest |
| `OCTOMUS_CHAT_TIMEZONE` | IANA time zone for the digest (default UTC) |

In the Docker deployment the non-secret variables live in `.env` (see
[env.example](../deploy/docker/env.example)); give the tokens as
secret files under `secrets/` in a compose override for `octomus`, the
same way as the
[attention webhook URL](deployment.md#optional-attention-webhook), then
run `docker compose up -d --force-recreate octomus`. Under systemd, set
them in `/etc/octomus/agent.env` and restart the service. Chat tokens are
never passed to runner or verification children.

## Commands

Slack: `/octomus <command>`, or `@Octomus <command>` in a channel the app
belongs to (mention answers are posted in-thread, visible to the
channel; slash-command answers are ephemeral). Discord: `/octomus
<command>` with the same verbs as subcommands; answers are ephemeral.

| Command | What it does |
| --- | --- |
| `status` | Mode, cycle number, active and attention tasks, pending requests, today's sessions |
| `digest [page]` | The 7-day fleet summary on demand; large fleets are paged at repository boundaries |
| `tasks [N]`, `task [id]` | Latest tasks (default 5, at most 20); one task's detail (default: the remembered task), with Retry/Cancel buttons when allowed |
| `cycles [N]`, `prs [N]` | Latest cycles; observed pull requests |
| `requests [page]` | Pending and five recent work requests, with complete IDs on numbered pages. Long target names are shortened for display |
| `request <title> :: <description>` | File a work request (target: the default branch) |
| `pause`, `resume`, `cycle`, `audit` | Operating controls; ask for confirmation |
| `retry <task-id>`, `cancel <task-id>`, `withdraw <request-id>` | Task and request actions; ask for confirmation |
| `repos`, `help` | Served repositories; this list |

Which repository a command acts on: a `--repo OWNER/REPOSITORY` flag
anywhere in the command (the `repo` option on Discord), else the
repository last used in that thread, else the channel binding, else the
primary repository. `task` without an ID re-shows the task last discussed
in the thread. Confirmations expire after five minutes, work only for
the operator who asked, and are forgotten on restart.

## Alerts and the digest

When an alert channel is configured, the same attention events as the
webhook's — newly blocked/failed or published tasks, failed planning
cycles, completed audits, error-paused service episodes — are posted
there through the same durable outbox (one delivery per destination,
same retries and 24-hour expiry; the 1,000 pending cap now covers all
destinations together). Task alerts carry **View task** and **Retry
task** buttons. Changing or removing an alert channel cancels only that
destination's pending alerts; the webhook and the other chat provider
are unaffected.

With `OCTOMUS_CHAT_DIGEST_TIME` set, a daily digest goes to every
configured alert channel: each repository's mode, cycle, attention and
pending-request counts, and its 7-day task/cycle/PR outcome counts.
Large fleets are split into consecutive messages at repository
boundaries. A missing digest means the service is down — treat it as
the heartbeat.

Chat failures never stop the maintainer: connections reconnect with
backoff, delivery problems appear as `WARN chat: …` in the service log,
and provider redeliveries are deduplicated so a command never executes
twice.

## Live validation checklist

Run this once per provider against your own workspace/server (this is
operator validation; the release test suites use fake providers):

1. `/octomus status` answers; a non-allowlisted account gets silence
   (Slack) or an operator-only notice (Discord).
2. `request …` files a request visible in the dashboard's Requests view.
3. `pause` asks for confirmation; Confirm pauses; `resume` restores
   continuous operation.
4. A blocked or published task posts an alert with working buttons.
5. The digest arrives at the configured time in the configured zone.
