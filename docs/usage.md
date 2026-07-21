# Using roundclaw

This is the user guide: how to talk to an agent from **Discord** or the **HTTP
API**, watch it work, steer or stop it, and set up recurring runs. For how any of
it works underneath, see [architecture/](architecture/README.md).

An **agent** is a named Claude Code worker with its own workspace and session. It
keeps context across requests, so you can follow up without repeating yourself.
Someone with admin rights creates agents (see [Managing agents](#managing-agents));
everyone else just sends them work.

---

## Discord

### Send a request

There are three ways, depending on how the channel is set up:

| Situation | What you do |
|-----------|-------------|
| Channel is **bound to an agent** | Just type your message. The whole message is the request. |
| A **shared channel** (agent set to require a mention) | Mention the bot: `@agent deploy the staging branch`. Messages without a mention are ignored. |
| Any channel, pick the agent explicitly | `/ask prompt:<what you want> agent:<name>` |

`/ask` also takes a **file**: `/ask prompt:"summarise this" file:<attachment>`. The
file is saved into the agent's workspace and referenced by path in the prompt, so
the agent can open it.

If you don't know the agents, run **`/agents`** — it lists every agent you can
call and what each is for.

### Threads are separate sessions

Reply in a **Discord thread** and that thread becomes its own Claude session with
its own isolated workspace. Two threads off the same channel run in parallel
without sharing context or stepping on each other's files. The main channel is
the agent's default session; each thread is a fresh, separate conversation.

Use a thread when you want to explore something without disturbing the channel's
main line of work.

### While it's working

- **`/status [agent]`** — what the agent is doing *right now*: its current tool
  call, queue length, and today's spend. Answers instantly even when the agent is
  busy or stuck, because it reads the record directly rather than asking the
  running job.
- **`/workflow [agent]`** — the lower-level execution state (alive, waiting,
  retrying). Useful when something looks wedged.

Requests that arrive while the agent is busy **queue up** — each gets its own
reply when its turn runs. They are never merged into one answer.

### Stop or redirect

- **`/stop [agent]`** — kill the current turn and drop everything queued behind
  it. The conversation is kept; the agent just goes idle.
- **`/steer instruction:<do this instead> [agent]`** — interrupt the current turn
  and point the agent at something else, **keeping the conversation context**. Use
  this when the agent is heading the wrong way — you don't lose what it already
  knows, only the unfinished work of the interrupted turn.

In a thread, `/stop` and `/steer` act on **that thread's** conversation only.

> The `[agent]` argument is optional everywhere: leave it off in a bound channel
> and it targets that channel's agent.

### Managing agents

Admin-gated commands (your server decides who can run them):

```
/agent create              open a form to define a new agent
/agent edit    agent:<name>    edit its definition (pre-filled form)
/agent show    agent:<name>    print the full definition
/agent enable  agent:<name>    let it accept requests again
/agent disable agent:<name>    stop new requests; stops running turns too, keeps the conversation
/agent delete  agent:<name>    remove the definition (workspace + conversation are kept)
```

`disable` and `delete` stop **every** running turn the agent has — the main
channel and all its threads — so nothing keeps running after you take it out of
service. `delete` keeps the workspace and session on disk, so recreating an agent
with the same name resumes where it left off.

### Schedules

Run an agent automatically on a cron schedule (a daily report, an hourly check):

```
/schedule create               open a form (agent, cron, timezone, prompt)
/schedule list                 schedules with their next run times
/schedule show   schedule:<id>
/schedule pause  schedule:<id>  stop it firing, keep the definition
/schedule resume schedule:<id>
/schedule delete schedule:<id>
```

Scheduled runs use the agent's **default** session, so a daily job builds on what
it did yesterday. Editing a schedule takes effect on its next run.

### A note on permissions

Two gates decide who can do what. Discord's own permissions filter commands
before they reach roundclaw, and roundclaw can additionally restrict write
commands to specific roles or people. Read-only commands (`/status`, `/agents`,
`/workflow`) are open to anyone who can see the channel. If a command doesn't
appear or is refused, you don't have rights for it — ask whoever runs the bot.

---

## HTTP API

Same agents, same queue as Discord — an API request and a Discord message land in
the same place and wait behind each other. Every route below is under `/v1` and
needs a bearer token except webhooks, which are signed instead.

```
Authorization: Bearer <your-token>
```

### Send a request

```bash
curl -X POST https://host/v1/agents/pr-reviewer/requests \
  -H "Authorization: Bearer $TOKEN" \
  -H "Idempotency-Key: 2f9c-...-a1" \
  -H "Content-Type: application/json" \
  -d '{"text": "review PR #482"}'
```

Returns **202** immediately — it does not wait for the agent:

```json
{
  "agent_id": "pr-reviewer",
  "turn_id": 123,
  "status": "queued",
  "queue_position": 2,
  "duplicate": false
}
```

- **`Idempotency-Key`** (optional, recommended): retry with the same key and you
  land on the same `turn_id` instead of starting a second run (`duplicate: true`).
  Without it, a network retry becomes a duplicate request.
- **`queue_position`** tells you how many turns are ahead — the queue is shared
  with Discord, so it can be non-zero through no fault of yours.
- Add **`"steer": true`** to interrupt the running turn and jump the queue, the
  API equivalent of `/steer`.

### Get the result

Three ways, with different durability guarantees:

**1. Poll** (simplest, always works):

```bash
curl https://host/v1/agents/pr-reviewer/turns/123 \
  -H "Authorization: Bearer $TOKEN"
```

```json
{ "agent_id": "pr-reviewer", "turn_id": 123, "status": "done",
  "result": "…", "cost_usd": 0.42, "error": "" }
```

`status` is `running`, `done`, `stopped` or `error`.

**2. Stream** live progress as it happens (Server-Sent Events):

```bash
curl -N https://host/v1/agents/pr-reviewer/turns/123/stream \
  -H "Authorization: Bearer $TOKEN"
```

**3. Callback** — have roundclaw POST the result to you when it finishes:

```bash
-d '{"text": "review PR #482", "callback_url": "https://you.example/hook"}'
```

The callback is delivered by a durable, retried job, so it survives a gateway
restart. The URL must be publicly reachable (internal/loopback addresses are
rejected), and the POST is signed so you can verify it came from roundclaw.

**Convenience: `?wait=true`.** Add it to the request POST to hold the connection
until the turn finishes and get the result in one call. It is *not* durable — if
it takes longer than the server's wait timeout, you get the 202 back and fall
back to polling the `turn_id`. Good for quick calls, not for long jobs.

| Way | Durable across a gateway restart? |
|-----|-----------------------------------|
| Poll / stream | Yes — the result is stored |
| `callback_url` | Yes — retried automatically |
| `?wait=true` | No — convenience only |

### Status and execution

```
GET /v1/agents/{agent}            what it is doing now (state, queue, recent log, budget)
GET /v1/agents/{agent}/workflow   execution state: alive, waiting, retrying, absent
```

### Manage agents

```
GET    /v1/agents                        list
POST   /v1/agents                        create (definition in the body)
GET    /v1/agents/{agent}/definition     read
PUT    /v1/agents/{agent}/definition     replace
DELETE /v1/agents/{agent}                delete (workspace + session kept)
```

Delete stops every conversation first, same as the Discord command.

### Schedules

```
GET    /v1/schedules                     list
GET    /v1/schedules/{schedule}          read one
PUT    /v1/schedules/{schedule}          create or replace
DELETE /v1/schedules/{schedule}          delete
POST   /v1/schedules/{schedule}/pause    stop firing, keep it
POST   /v1/schedules/{schedule}/resume   let it fire again
```

A schedule carries a `cron` expression, a `timezone` (default `UTC`), and the
`prompt` to run.

### Inbound webhooks

Let an external system (GitHub, CI) trigger an agent without a bearer token:

```
POST /v1/webhooks/{agent}
```

- The body is **signed**, not bearer-authenticated: HMAC-SHA256 over the raw
  body in either `X-Roundclaw-Signature` or GitHub's `X-Hub-Signature-256`.
- The payload is handed to the agent as-is (pretty-printed if it's JSON).
- The delivery ID is used as an idempotency key, so a sender's retry lands on the
  original turn rather than firing twice.
- If no webhook secret is configured, the endpoint returns 503 rather than
  accepting unsigned calls.

---

## Quick reference

| I want to… | Discord | HTTP |
|------------|---------|------|
| Send a request | message / `@bot` / `/ask` | `POST /v1/agents/{a}/requests` |
| See what it's doing | `/status` | `GET /v1/agents/{a}` |
| Get a result | reply arrives | poll `…/turns/{id}`, SSE, or `callback_url` |
| Interrupt & redirect | `/steer` | `POST …/requests` with `"steer": true` |
| Stop and clear the queue | `/stop` | — |
| A separate, parallel session | reply in a **thread** | — |
| Schedule recurring work | `/schedule create` | `PUT /v1/schedules/{id}` |
| Trigger from an external system | — | `POST /v1/webhooks/{a}` (signed) |
