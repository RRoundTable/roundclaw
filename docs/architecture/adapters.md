# Adapters

`internal/adapter` is every edge roundclaw has: the two inbound sources
(Discord, HTTP), the read paths that answer questions about running work, and
the admission logic they share. Outbound delivery lives in the worker
([orchestration.md](orchestration.md)) because it must be retried durably.

## Dispatcher — the shared admission path

`dispatch.go` is what both adapters call. Neither one talks to Temporal or
SQLite on its own.

| Method | Does |
|--------|------|
| `SubmitIn(ctx, agentID, conversationID, text, origin, idempotencyKey)` | check limits → insert the `turns` row → `SignalWithStartWorkflow(enqueue)` |
| `SteerIn(...)` | same, with the `steer` signal — jumps the queue and interrupts the running turn |
| `StopIn(ctx, agentID, conversationID, reason)` | `SignalWorkflow(stop)` for one conversation |
| `StopAll(ctx, agentID, reason)` | `stop` to every conversation the agent has (below) |
| `Status(ctx, agentID, tail)` | SQLite only: `agent_runtime` + last N `live_logs` |
| `Workflow(ctx, agentID)` | `DescribeWorkflowExecution` — alive, waiting, retrying, or absent |
| `RunWorkflow(ctx, id)` | start one run of an agent-less workflow (`ExecuteWorkflow`) |
| `ExecuteAdmin(ctx, action)` | apply a planned management action (below) |

`Submit`/`Steer` are thin wrappers that pass an empty conversation ID, i.e. the
agent's default conversation.

**`StopAll` — used by disable and delete.** Stopping only the default
conversation would leave every Discord thread's turn running; after a delete
they would then fail one turn at a time once the definition is gone. `StopAll`
enumerates the agent's conversations from its own `state.db`
(`SELECT DISTINCT conversation FROM turns`, always including the default) and
signals `stop` to each. It reads stored identity rather than matching workflow-ID
prefixes on purpose: the agent/conversation separator `-` is also legal inside an
agent ID, so a prefix of agent `foo` would also match agent `foo-bar`. A signal
to an already-idle conversation is expected and logged, never fatal.

The turn row is written **before** the signal, in the same transaction that
claims the idempotency key. That ordering is what lets the HTTP API return a
`turn_id` in its 202 without asking the workflow anything, and what makes a
client retry idempotent (`store.CreateTurn` returns `existed=true`).

`ResolveAgent(ctx, reg, explicitID, channelID)` is the shared "which agent?"
rule: an explicit ID wins, otherwise the agent bound to the channel.

## Limits

`limits.max_concurrent_turns` caps how many turn containers run at once across
all agents. It is applied to the Temporal worker as
`MaxConcurrentActivityExecutionSize`, so excess turns wait in the queue rather
than fail. It defaults to 5.

## Discord (`discord*.go`)

One `discordgo` session in the gateway. Commands are registered with a bulk
overwrite at startup — never deleted, because a delete-then-create races a
restart and leaves the guild with no commands.

```
/ask        prompt [agent] [file]      send a request
/agents                                list callable agents
/agent      create | edit | show | enable | disable | delete
/admin      request                    manage in plain language (below)
/schedule   create | list | show | pause | resume | delete
/status     [agent]                    what it is doing right now
/workflow   [agent]                    Temporal execution state
/stop       [agent]                    kill the turn, drop the queue
/steer      instruction [agent]        interrupt and redirect
```

Notable behaviour:

- **Create/edit use modals**, not twelve slash-command options. Agent and
  schedule pickers are autocomplete-backed, fed from the registry.
- **Two permission gates.** `discord.command_permission` sets
  `default_member_permissions` so Discord filters before the interaction
  arrives; `allowed_roles` / `allowed_users` filter again inside roundclaw,
  which can express "these three people" where Discord cannot. Read-only
  commands (`/status`, `/agents`, `/workflow`) skip the second gate.
- **`require_mention`** makes a bound channel answer only messages that mention
  the bot. Without it, every message in the channel is a billable request.
- **3-second ack.** `/status` answers inline because it is a DB read; `/stop`
  and `/steer` defer and follow up.
- **File attachments** are downloaded by `discord_files.go`, saved through
  `SaveAttachments` into the agent's `work/inbox/`, and referenced by path in
  the prompt (`PromptWithAttachments`). Names are sanitised and randomised, and
  size is capped while streaming, not after.

## HTTP (`http*.go`)

`net/http` `ServeMux` with Go 1.22 method+pattern routing. Bearer auth from
`http.tokens_env`, except webhooks which authenticate by signature.

There are **two token scopes**. Full tokens (`http.tokens_env`) reach every
route. Delegate tokens (`http.delegate_tokens_env`) are restricted by
`delegateAllowed` to sending a request and reading agent status — list, status,
turns, stream, workflow, and `POST …/requests`; everything else is `403`,
including an agent's `definition` (it exposes host paths). This is what lets an
agent's own container carry a token to delegate to another agent (the `team`
tool, [agent-runtime.md](agent-runtime.md#registered-tools)) without being able
to reconfigure or delete the fleet — the check runs in the auth middleware,
before routing, and denies by default. Tokens are held only as SHA-256 hashes and
compared in constant time.

```
POST   /v1/agents/{agent}/requests        202 {turn_id, queue_position, duplicate}
GET    /v1/agents/{agent}/turns/{turn}    turn state and result
GET    /v1/agents/{agent}/turns/{turn}/stream   SSE tail of live_logs
GET    /v1/agents/{agent}                 status
GET    /v1/agents/{agent}/workflow        Temporal execution state
GET    /v1/agents                         list
POST   /v1/agents                         create
GET    /v1/agents/{agent}/definition      read definition
PUT    /v1/agents/{agent}/definition      replace definition
GET    /v1/agents/{agent}/persona         read CLAUDE.md (instructions)
PUT    /v1/agents/{agent}/persona         replace CLAUDE.md
DELETE /v1/agents/{agent}                 delete definition
GET|PUT|DELETE /v1/secrets[/{name}]                 global secrets
GET|PUT|DELETE /v1/agents/{agent}/secrets[/{name}]  per-agent secrets
GET    /v1/schedules  ·  GET|PUT|DELETE /v1/schedules/{schedule}
POST   /v1/schedules/{schedule}/pause  ·  /resume
POST   /v1/webhooks/{agent}               signature-authenticated
```

Three ways to get a result, with different durability:

| Mode | Mechanism | Survives a gateway restart |
|------|-----------|----------------------------|
| Poll / SSE | client reads `GET .../turns/{id}` | yes — the DB is the record |
| `callback_url` | `DeliverResponse` activity POSTs | yes — Temporal retries it |
| `?wait=true` | gateway holds the connection | **no** — convenience only |

`?wait=true` degrades to a 202 with the `turn_id` after
`http.wait_timeout`, so a caller behind a long queue is never hung indefinitely.
SSE connections are capped per agent (`http.max_sse_per_agent`) so a client
cannot exhaust the gateway's file descriptors.

## Secrets

`http_secrets.go` exposes the encrypted secret store ([data.md](data.md#registrydb))
over the API. The shape of every handler follows one rule: a value goes **in** on
a `PUT` and never comes back out. `GET` lists names and timestamps; there is no
route that returns a value, and the handlers log the name and scope but never the
value. A write with no master key configured returns `503` rather than storing
plaintext (`ErrSecretsDisabled`), and a per-agent write for an unknown agent is a
`404`. The `roundclaw` CLI is just another client of these routes.

## Admin is an agent

Managing roundclaw in plain language is done by an **agent**, not a stateless
planner. `admin` is an ordinary agent given (a) a full-scope `ROUNDCLAW_API_TOKEN`
as a per-agent secret and (b) the roundclaw CLI as an `admin-cli` tool
(`ROUNDCLAW_URL` in its env). It manages the fleet by *driving the API itself* —
so it has a real session, tools, and multi-step reasoning, and is not confined to
a fixed action set. There is no bespoke subsystem: it is the agent runtime plus
the tool and secret machinery, pointed at roundclaw's own API.

This trades one safety model for another, deliberately. The earlier stateless
planner was safe *by construction* — the LLM could only emit one of a fixed set
of actions and held no token. An agent that drives the API holds a powerful token,
so its safety rests on **channel access control**: bind it to a private channel,
keep `require_mention`, apply the allow-list, and give it neither web access nor
delegated requests from other agents (either is a prompt-injection path to a
full-access token). The blast radius is bounded only by who can talk to it.

The persona routes (`GET|PUT /v1/agents/{id}/persona`) exist for this: an agent
in an isolated container cannot reach another agent's workspace file, so editing a
persona — which the old planner did on the gateway's filesystem — is exposed as an
API the admin agent calls like anything else.

Because the admin agent replaces a whole definition through
`PUT /v1/agents/{id}/definition`, it can also set another agent's **container
image** and **`group_add`** ([data.md](data.md#registrydb)) — the fleet's own way
to give, say, a dev agent a docker-capable image and the docker group so it can
manage images. This is the same class of host reach `work_dir` already grants
(both are why a delegate token cannot read a definition), one step larger: an
image plus `group_add` for the docker group plus the socket mount is effective
host root inside that agent's container, and that agent is a prompt-injection
target. Which is exactly why the admin agent's own safety rests on channel access
control — the power to hand out that reach is bounded by who can talk to it.

> The previous stateless planner (`internal/claude/admin.go`, `adapter/admin.go`,
> the `/admin` slash command, and the `discord.admin_channel` config) has been
> removed. Management now flows entirely through the ordinary agent path.

## Webhooks

`http_webhooks.go` sits outside the bearer-auth mux: a GitHub or CI sender
cannot hold a rotating token, so it signs instead.

- HMAC-SHA256 over the **raw** body, compared in constant time. Parsing and
  re-encoding would verify something the sender never sent.
- Accepts `X-Roundclaw-Signature` or GitHub's `X-Hub-Signature-256`.
- No configured secret (`http.webhook_secret_env`) → 503, not "open".
- Body capped at 1 MiB.
- The delivery ID becomes the idempotency key, so a sender's timeout retry lands
  on the original turn.
- The payload is handed to the agent verbatim (pretty-printed if it is JSON);
  roundclaw does not know any sender's schema well enough to summarise it.

## Callback SSRF

A `callback_url` is attacker-supplied by definition. `core.AssertPublicCallbackHost`
rejects loopback, private, link-local, CGNAT and IPv6-local addresses, and is
called **at admission as well as at delivery** — checking only at delivery
leaves a window where the URL is already accepted and stored. Redirects are not
followed, and the POST is signed with `http.callback_secret_env` so the receiver
can verify it.
