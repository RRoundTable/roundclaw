# roundclaw

A durable orchestrator for Claude Code. It runs agents in containers, keeps
their work alive across worker crashes, and accepts requests from Discord and an
HTTP API at the same time.

roundclaw does not use the Agent SDK. Each agent turn is a `claude -p
--output-format stream-json` process inside a container that holds nothing but
the `claude` binary — roundclaw ships no code into the image.

## Why it exists

Two problems with running Claude Code as a service:

- **Merged replies.** A queue that collapses "messages arrived while busy" into
  a single flag answers a burst of three requests with one merged reply.
  roundclaw keeps a real queue, so three requests produce three turns and three
  replies.
- **Lost work.** If the host dies mid-run, the container goes with it and the
  progress is gone. roundclaw runs each turn as a Temporal activity, so a
  crashed worker retries and reattaches to the same Claude session.

## Architecture

```
[inbound]                     [core]                       [outbound]

Discord ─┐                                                 ┌─ Discord
HTTP API ┼→ Request{agentID, text, origin} → workflow → ─→ ┼─ HTTP poll / SSE
         │                                                 └─ callback POST
         │
         └ status and control bypass the core entirely:
             /status, GET /v1/agents/{id}  → SQLite directly   [no LLM, no Temporal]
             /stop, /steer                 → Temporal signal   [no LLM]
```

Two invariants hold everywhere:

1. **Status and stop never go through an LLM.** They must work when an agent is
   wedged — which is exactly when someone asks.
2. **Steering is always explicit.** Nothing infers "interrupt the agent" from
   message content, so a misread can never destroy work in progress. The worst
   case for a misrouted request is that it waits.

### How durability actually works

The Claude session UUID is derived from the workflow ID
(`claude.SessionID`, UUIDv5). Because it is a pure function, a retried activity
— including one picked up by a different worker after a crash — passes the same
`--session-id`/`--resume` and reconnects to the same conversation. Nothing has
to capture a session ID from process output and persist it, so there is no
window in which that write can be lost.

The turn itself is retried from the start; the *conversation* survives, the
interrupted turn's unfinished work does not.

## Layout

| Path | Purpose |
| --- | --- |
| `cmd/worker` | Temporal worker: runs turns, delivers responses |
| `cmd/gateway` | Discord listener + HTTP API + status reads |
| `internal/core` | Types shared by every adapter; imports no adapter |
| `internal/claude` | CLI argv, stream-json decoder, session derivation |
| `internal/store` | Per-agent SQLite (WAL) |
| `internal/adapter` | Discord and HTTP inbound edges |
| `internal/temporal` | Workflow and activities |
| `container/` | Production agent image |
| `container/fake/` | Scripted `claude` stand-in for end-to-end tests |

## Running it

```bash
docker build -t roundclaw/claude:latest container   # the agent image
cp roundclaw.example.yaml roundclaw.yaml            # then edit the agents section

# Compose needs these in .env; ROUNDCLAW_ROOT must be this directory, absolute.
cat >> .env <<EOF
ROUNDCLAW_ROOT=$(pwd)
ROUNDCLAW_UID=$(id -u)
ROUNDCLAW_GID=$(id -g)
DOCKER_GID=$(stat -c '%g' /var/run/docker.sock)
ROUNDCLAW_HTTP_PORT=8099
EOF

docker compose up -d --build
docker compose logs -f gateway worker
```

Postgres-backed Temporal, plus the worker and gateway. `docker compose down`
keeps everything; add `-v` to wipe Temporal's database.

Two things in `compose.yaml` are load-bearing:

- **`ROUNDCLAW_ROOT` is mounted at the same absolute path inside the
  containers.** The worker starts agent containers through the host's Docker
  daemon, and that daemon resolves `-v` sources against the *host*
  filesystem. A tidy in-image path like `/app/workspace` would give every agent
  an empty mount from a host directory that does not exist — silently.
- **Temporal runs on Postgres, not the dev server's in-memory store.** That is
  the difference between surviving a restart and losing every running agent.

The worker mounts the Docker socket, so agent containers are *siblings* of the
worker rather than children. `group_add` grants the socket's group without
running anything as root.

### Without compose

```bash
temporal server start-dev                    # or point at your own cluster
# set temporal.host_port to localhost:7233 and workspace_root to a local path

# Either credential works; the OAuth token wins when both are set.
export CLAUDE_CODE_OAUTH_TOKEN=$(claude setup-token)   # preferred, headless
# export ANTHROPIC_API_KEY=...                         # or an API key

export DISCORD_TOKEN=...                     # omit to run HTTP-only
export ROUNDCLAW_API_TOKENS=some-secret
export ROUNDCLAW_CALLBACK_SECRET=another-secret

go run ./cmd/worker  -config roundclaw.yaml
go run ./cmd/gateway -config roundclaw.yaml
```

Both binaries retry the Temporal connection while it comes up, so start order
does not matter.

## Agents

Agents live in a runtime registry (`<workspace_root>/registry.db`), not in the
config file. Creating one takes effect immediately — no restart, no redeploy:

```bash
curl -X POST localhost:8080/v1/agents -H "Authorization: Bearer $TOKEN" \
  -d '{"id":"pr-reviewer","description":"Reviews pull requests",
       "permission_mode":"acceptEdits","allowed_tools":["Read","Grep","Bash"],
       "discord_channels":["123456789"]}'
```

The config file's `agents:` list is a **one-time bootstrap**: it seeds an empty
registry on first start and is ignored from then on. Startup logs say when that
happens, because a silently-ignored YAML block would be a nasty surprise.

Two behaviours worth knowing:

- **A channel belongs to exactly one agent**, enforced by a primary key rather
  than a config check, so it holds even when two writers race.
- **Deleting an agent keeps its workspace, database and Claude session.** The
  definition is cheap to recreate; the conversation is not. Recreating the same
  ID resumes where it left off. Reclaiming that disk is a separate, explicit
  step. Deleting also stops the agent first, so queued work does not run on and
  fail one turn at a time.

Editing a definition takes effect on the agent's **next** turn. The turn already
running keeps the arguments it started with.

## HTTP API

Turn IDs come from a per-agent SQLite `AUTOINCREMENT`, so they are unique only
within an agent. Every turn route is therefore agent-scoped.

```
POST /v1/agents/{agent}/requests             queue a request
GET  /v1/agents/{agent}                      status + recent transcript
GET  /v1/agents/{agent}/turns/{turn}         turn state and result
GET  /v1/agents/{agent}/turns/{turn}/stream  SSE live transcript

GET    /v1/agents                            list agents
POST   /v1/agents                            create an agent
GET    /v1/agents/{agent}/definition         read a definition
PUT    /v1/agents/{agent}/definition         replace a definition
DELETE /v1/agents/{agent}                    delete a definition
```

`GET /v1/agents/{agent}` is runtime state; the definition is nested under
`/definition` because state is asked for far more often.

```bash
curl -X POST localhost:8080/v1/agents/pr-reviewer/requests \
  -H "Authorization: Bearer $ROUNDCLAW_API_TOKENS" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"text":"review the diff on main"}'
# => 202 {"agent_id":"pr-reviewer","turn_id":7,"status":"queued","queue_position":0}
```

Three ways to get the result, with different durability:

| Mode | How | Survives a gateway restart |
| --- | --- | --- |
| Poll / SSE | `GET .../turns/{turn}` or `/stream` | Yes — SQLite is the source of truth |
| `callback_url` | roundclaw POSTs to you when done | Yes — retried as a Temporal activity |
| `?wait=true` | connection held until the turn ends | **No** — a convenience only |

`?wait=true` demotes to `202` at the timeout and hands back a `turn_id` to poll.
Discord and the HTTP API share one queue per agent, so a wait can be long
because of someone else's request; `queue_position` says how long.

Supply `Idempotency-Key` on every POST. Without it a client retry becomes a
second agent run.

## Discord

```
/ask     agent:<name> prompt:<...>   call any agent from any channel
/agents                              list the agents and what each is for
/status  [agent:<name>]              what an agent is doing right now
/stop    [agent:<name>]              cancel the current turn, clear the queue
/steer   instruction:<...> [agent:]  interrupt and redirect, keeping context
```

The `agent` argument has autocomplete, and is optional on everything except
`/ask`. Omitted, it means the channel's bound agent; given, it addresses that
agent from anywhere — including a channel bound to nothing.

Binding a channel to an agent in the config is the ergonomic shortcut: a plain
message in a bound channel is queued as a request, no command needed.

### Routing unbound channels (optional)

A plain message in an unbound channel is ignored unless `router.enabled` is set.
With it on, a stateless `claude -p --bare --output-format json --json-schema`
call classifies the message as dispatch / ignore / clarify and names an agent.

Three properties are deliberate:

- **Stateless.** No session, so concurrent routing calls do not queue behind
  each other. A router with a session would reintroduce head-of-line blocking
  one layer above the agent queues that exist to prevent it.
- **It can only dispatch.** Stop and steer stay explicit commands, so a routing
  mistake costs a wasted dispatch, never lost work.
- **It defaults to ignoring.** Unbound channels carry ordinary conversation, and
  a router failure stays silent rather than replying to every message.

An agent ID the model invents is downgraded to `clarify` rather than acted on.

## Testing

```bash
go test ./...
```

For an end-to-end run without an Anthropic credential, build the fake agent
image and point `container.image` at it:

```bash
docker build -t roundclaw/claude-fake:test container/fake
```

It speaks scripted stream-json and understands `sleep:<n>` and `fail` markers in
the prompt, so cancellation, crash recovery, and error delivery can all be
exercised against the real code path.

## Credentials

Use `claude setup-token` for a long-lived headless token, or an API key. Do
**not** point roundclaw at `~/.claude/.credentials.json`: those credentials
belong to an interactive session, and a container that refreshes them rotates
the refresh token out from under whoever is still using that session.

## Two settings that are easy to get wrong

- **`MaxHeartbeatThrottleInterval`** (`cmd/worker`). An activity only learns it
  was cancelled through a heartbeat response, and the SDK throttles outbound
  heartbeats to ~80% of `HeartbeatTimeout`. Calling `RecordHeartbeat` every
  second is not enough on its own — without the cap, `/stop` takes up to ~24
  seconds to reach the container.
- **The `~/.claude` mount.** Session transcripts live there. The container is
  `--rm`, so if that directory is not a persistent host mount, `--resume` fails
  on the second turn.
- **One Temporal namespace per environment.** A workflow ID is
  `roundclaw-agent-<id>` and encodes nothing about the deployment, so two
  configs sharing a namespace collide on any agent ID they have in common.
  `SignalWithStart` then finds the *existing* workflow — pinned to whatever task
  queue created it — and signals that instead of starting a new one. Nothing
  errors: requests are simply accepted and never run, because no worker is
  polling that queue. Give staging and production separate namespaces, and do
  not reuse agent IDs between a scratch config and a real one.
