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
/schedule   create | list | show | pause | resume | delete
/proposals                             decide changes waiting on a person
/status     [agent]                    what it is doing right now
/workflow   [agent]                    Temporal execution state
/stop       [agent]                    kill the turn, drop the queue
/steer      instruction [agent]        interrupt and redirect
```

There is no `/admin`. Management in plain language is an ordinary agent holding a
full-scope token, not a command — see "Agent-based admin" below.

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
- **File attachments** are downloaded by `discord_files.go` and referenced by
  path in the prompt (`PromptWithAttachments`), never inlined into it. Names are
  sanitised and randomised, and size is capped while streaming, not after.

  Admission stages, the worker places. `StageAttachments` writes the bytes to
  `inbox-staging/` beside the workspaces and returns both the host paths — which
  go onto the turn row — and the `/workspace/inbox/…` paths the prompt promises.
  The worker links them into the workspace it mounts, once `resolveWorkspace`
  has chosen one (see [orchestration.md](orchestration.md)).

  The split is not incidental. At admission nobody knows which workspace will
  read the file: a conversation gets its own directory or git worktree, and only
  the worker creates it. Writing into `work/inbox/` directly — as this once did —
  put every threaded upload at a path the container had no mount for, and the
  turn ran and was billed regardless. Writing into the conversation directory
  early would be worse: `resolveWorkspace` reads an existing directory as one an
  earlier turn prepared, so it would skip the worktree and the CLAUDE.md seed.

- **Files going out** are the mirror of that (`outbox.go`). An agent writes into
  `outbox/` in its workspace and names the file when it speaks; the gateway opens
  it and hands the reader to Discord, so nothing is buffered and a 10MB
  attachment never sits in memory.

  This exists because the message text was the only way out. A long report sent
  as text is twenty Discord messages, and it stays in the session context to be
  re-sent on every later turn of that conversation — the agent pays for it again
  each time. As a file it costs one path.

  `outbox/` is **not** a boundary against a compromised agent: one with Bash can
  copy anything into it. What it stops is an accident — a workspace `.env` or a
  stray source file going out because a path was slightly wrong — and a
  prompt-injected agent that reached only the message API naming an arbitrary
  path. Symlinks are resolved before the containment check, since the agent has
  Write and `ln -s /etc/shadow outbox/notes.txt` passes every purely lexical
  test. The outbox is resolved against the workspace the *conversation* runs in,
  by the same function the worker mounts with, so a file written in one thread is
  not visible from another.

## HTTP (`http*.go`)

`net/http` `ServeMux` with Go 1.22 method+pattern routing. Bearer auth from
`http.tokens_env`, except webhooks which authenticate by signature.

There are **two token scopes**. Full tokens (`http.tokens_env`) reach every
route. Delegate tokens (`http.delegate_tokens_env`) are restricted by
`delegateAllowed` to sending a request, reading agent status, speaking in a
conversation, and managing the schedules filed under an agent — list, status,
turns, stream, workflow, `POST …/requests`, `POST …/messages` and
`…/agents/{id}/schedules…`; everything else is `403`, including an agent's
`definition` (it exposes host paths) and the fleet-wide `/v1/schedules` routes,
which take their owner from the body. Speaking is on the restricted surface because it starts
no work, spends no tokens, and cannot reach a channel the agent is not already
spoken to in: the target is resolved from that conversation's own history, never
from the request, so a prompt-injected agent cannot broadcast. This is what lets an
agent's own container carry a token to delegate to another agent (the `team`
tool, [agent-runtime.md](agent-runtime.md#registered-tools)) without being able
to reconfigure or delete the fleet — the check runs in the auth middleware,
before routing, and denies by default. Tokens are held only as SHA-256 hashes and
compared in constant time.

The agent-scoped schedule routes are the only **write** on that surface beyond
starting work, and the whitelist is not what keeps them safe — the handlers are
(`http_schedules.go`). A shared token cannot prove who is calling, so the agent
in the path is a claim, bounded the same way `notify` is: the stored `agent_id`
comes from the path and never the body, another agent's schedule is `404`
(`409` on a name collision, because ids are unique fleet-wide), and `channel_id`
must be one the named agent is already bound to, so a schedule cannot become a
timed broadcast into a channel nobody granted. Per-agent tokens would let the
gateway fill the identity in rather than believe it; that is the upgrade path if
the claim ever needs to be worth more than a wasted turn.

```
POST   /v1/agents/{agent}/requests        202 {turn_id, conversation, queue_position, duplicate}
                                          body: notify {agent, conversation} = return address,
                                                conversation_id = which conversation to run in
POST   /v1/agents/{agent}/messages        200 — say one thing, no turn created
                                          body: turn_id = the turn I am running, so the
                                                audience is read off that row not inferred;
                                                files = names in the conversation's outbox/
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
GET    /v1/agents/{agent}/turns           request history (?since=&status=&full=)
GET    /v1/agents/{agent}/versions        definition+persona history
GET    /v1/agents/{agent}/versions/{n}    one snapshot
POST   /v1/agents/{agent}/versions/{n}/rollback   apply an old version as a new one
GET|POST /v1/evals  ·  GET|PUT|DELETE /v1/evals/{eval}
POST   /v1/evals/{eval}/run               202 {run_id} — body: version, notify
GET    /v1/evals/runs  ·  GET /v1/evals/runs/{run}
GET    /v1/evals/compare?base=&candidate= what regressed, what improved
GET|POST /v1/proposals  ·  GET /v1/proposals/{proposal}
POST   /v1/proposals/{proposal}/approve  ·  /reject
POST   /v1/webhooks/{agent}               signature-authenticated
```

Two headers apply to every write: `X-Roundclaw-Author` and `X-Roundclaw-Note` are
recorded on the agent version the write mints. Neither is authenticated — a
bearer token says what may be done, not who is doing it — so they are a
changelog, not an audit log. That is the honest reading: "curator, because eval
run 12 regressed" is useful to whoever reads the history later, and nothing
depends on it being true.

The delegate whitelist (`delegateAllowed`) refuses all of the routes above. One
agent must not read another's request history — those turns are other people's
requests — and evals spend money while proposals change the fleet. The
single-turn route stays open because a delegator has to be able to read the
result it asked for.

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

`admin` is a **convention, not a feature** — no roundclaw code knows the name.
The recipe for building one ships as a Claude Code skill in the repository
(`skills/roundclaw-fleet`), which is both readable by a person and grantable to
the agent itself through the ordinary skill machinery.

## Proposals — where automation meets a person

`http_proposals.go` and `discord_proposals.go` are two front ends on one thing:
a queue of changes an agent wrote down and a person decides. The work itself
lives on the **dispatcher** (`proposals.go`), not in either handler, because both
edges approve — a button in Discord, or the CLI against the API — and two
implementations of "what approving means" would drift.

Applying goes through the ordinary `registry.Create` / `Update` / `Snapshot`
calls rather than writing rows directly. That is what makes an approved change
indistinguishable from a hand edit afterwards: same validation, same version
snapshot, same rollback. The response carries the version it produced and the
command that undoes it.

Two ordering rules are load-bearing:

- **The persona is written before the definition.** The registry snapshots
  whatever the persona source reads at write time, so the other order would mint
  a version pairing the new definition with the old instructions — a combination
  that never existed. Rollback does the same thing for the same reason.
- **Apply, then record.** A failed application marks the proposal `failed` with
  the reason rather than leaving it pending; a person already said yes, and a
  pending row would invite a second person to approve the same broken change.
  `DecideProposal` compare-and-sets on `pending`, so simultaneous clicks cannot
  apply a change twice.

The Discord buttons check the allow-list themselves rather than relying on the
command that posted them: the message they sit on is visible to the whole
channel. They carry the proposal **id**, not an index into the list — by the time
anyone clicks, another proposal may have been filed or decided, and "approve the
second one" would then mean something else than when it was rendered.

**What this is not.** Nothing prevents a full-scope token from calling
`PUT /v1/agents/{id}/definition` and skipping the queue. The gate is a convention
the curator's instructions keep, not a permission the server enforces — the same
trade the admin agent already makes. What the queue guarantees is that a change
made through it is recorded, atomic, and reversible. Enforcing it would need a
token scope narrower than roundclaw issues today.

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
