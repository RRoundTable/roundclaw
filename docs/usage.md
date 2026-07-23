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

### Managing in plain language — the `admin` agent

Management by conversation is done by an **agent**, not a slash command. `admin`
is an ordinary agent given a full-scope API token and the roundclaw CLI as a
tool, so it manages the fleet by driving the API itself — with a real session,
tools, and multi-step reasoning a fixed action set could not do. Bind it to a
**private** channel and talk to it:

```
(in the admin channel, @-mention the bot)
"create an agent called pr-bot for reviews, bound to #pull-requests"
"show me dev's settings and its persona"
"disable pm for now"
"change dev's persona so it always replies in Korean"
"build a 2-step workflow: collect the news, then summarise it, post here"
"왜 director가 어제 실패했는지 로그 보고 원인 알려줘"     ← open-ended, multi-step
```

Because it is an agent, it can chain steps, investigate, and remember the
conversation — not just emit one predefined action. It manages agents, tools,
secrets, workflows and schedules, and reads/writes any agent's persona through
`GET|PUT /v1/agents/{id}/persona`.

**This is a powerful role — lock it down.** The admin agent holds a token that
can create and delete agents, so its safety rests entirely on who can reach its
channel:

- Bind it to a **private** channel only trusted operators can post in, and keep
  `require_mention` on.
- Apply the command/message allow-list (see [permissions](#a-note-on-permissions)).
- Do **not** give it web access or accept delegated requests from other agents —
  either becomes a prompt-injection path to a full-access token.

The token it carries is a normal per-agent secret (`ROUNDCLAW_API_TOKEN` = a
full token), and the CLI + `ROUNDCLAW_URL` come from an `admin-cli` tool — so the
whole thing is built out of the tool and secret machinery above, nothing bespoke.

### Workflows — agent-less pipelines

A **workflow** is a standalone, multi-step job — no agent needed. Each step is a
prompt; a step receives the earlier steps' outputs, and the final result is
posted to a channel. Use it for automation that is a sequence of tasks rather
than a conversation (collect → analyse → report).

Create and run one in plain language via `/admin` ("build a workflow…", "run the
news workflow now"), or over the API:

```
POST   /v1/workflows                    create (id, channel_id, steps[])
GET    /v1/workflows                    list
POST   /v1/workflows/{id}/run           run one now
DELETE /v1/workflows/{id}
```

Each step can set its own `permission_mode` and `model` — a cheap model for a
mechanical step, a stronger one for analysis. Steps run non-interactively, so
they never wait on a permission prompt. Scheduling a workflow to run on a cron is
coming; for now runs are started by hand or from `/admin`.

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
GET /v1/agents/{agent}            what it is doing now (state, queue, recent log)
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

## Command line (`roundclaw`)

`roundclaw` is a terminal client for a running gateway. It is a thin wrapper over
the same HTTP API, so it needs the gateway's URL and a bearer token:

```bash
export ROUNDCLAW_URL=https://host          # default http://127.0.0.1:8099
export ROUNDCLAW_API_TOKEN=your-token      # one of http.tokens_env
```

```bash
roundclaw agents                       # list agents
roundclaw agent show pr-reviewer       # print a definition
roundclaw status pr-reviewer           # what it's doing now
roundclaw send pr-reviewer "review PR #482"
roundclaw send pr-reviewer "..." --wait          # block for the result
roundclaw send pr-reviewer "..." --steer         # interrupt and redirect
roundclaw send pr-reviewer "..." --key deploy-42 # idempotent retry
roundclaw turn pr-reviewer 123         # a turn's state and result

roundclaw tool set outline --path /path/to/outline-cli \
  --env OUTLINE_CONFIG=/mnt/outline-cli/config.json --desc "local Outline wiki"
roundclaw tool ls                      # registered tools
roundclaw tool rm outline
```

`--url` and `--token` override the environment on any command.

## Secrets

An agent's container often needs a credential of its own — a `GITHUB_TOKEN` for a
pull-request reviewer, an API key for a tool it calls. Register it once and
roundclaw injects it as an environment variable on every turn.

```bash
# Per-agent — only pr-reviewer's containers get it. Value read from stdin so it
# never lands in your shell history or the process table:
printf %s "$GH_PAT" | roundclaw secret set GITHUB_TOKEN --agent pr-reviewer

# Global — every agent sees it. A per-agent secret of the same name overrides it:
roundclaw secret set SENTRY_DSN            # prompts on stdin

roundclaw secret ls --agent pr-reviewer    # names only, never values
roundclaw secret rm GITHUB_TOKEN --agent pr-reviewer
```

The same over HTTP:

```
PUT    /v1/agents/{agent}/secrets/{name}   {"value": "..."}   per-agent
PUT    /v1/secrets/{name}                  {"value": "..."}   global
GET    /v1/agents/{agent}/secrets                             list names
DELETE /v1/agents/{agent}/secrets/{name}
```

How it works, and its limits:

- **Encrypted at rest.** Values are stored encrypted in the registry with a
  master key the server holds only in its environment
  (`container.secrets_key_env`, default `ROUNDCLAW_SECRET_KEY`). Set it to a
  strong random value — `openssl rand -base64 32`.
- **No master key, no secrets.** If the key is unset the store is off: registering
  a secret returns `503` rather than storing plaintext, and agents that use none
  run exactly as before.
- **Never read back.** No command or endpoint returns a stored value — only names.
  The value travels one way: in when you set it, into a container at turn time.
- **The Claude credential wins.** A secret named the same as the agent's login
  token (`CLAUDE_CODE_OAUTH_TOKEN` / `ANTHROPIC_API_KEY`) is ignored, so you
  cannot accidentally break authentication.
- Rotating the master key makes existing secrets unreadable — by design. Re-set
  them after a rotation.

## Tools — granting a local capability

A **tool** gives an agent a local capability: a CLI and its config that live on
the host, exposed to the agent's container as a read-only mount plus the
environment it needs. The worked example is Outline — a `tool` that lets `dev`
and `pm` read and write the local Outline wiki.

Registering and granting are deliberately separated:

- **Registering** a tool names a **host path**, which is sensitive, so it is an
  operator act — done over the CLI/HTTP with a bearer token.
- **Granting** a registered tool to an agent is then safe to do in plain
  language from Discord `/admin`, because admin can only pick from tools that
  already exist; it can never mount an arbitrary path.

```bash
# Operator registers the tool once (the host dir mounts read-only at
# /mnt/<basename>, and each --env is injected into the granted agents' turns):
roundclaw tool set outline \
  --path /home/you/.config/roundclaw/outline-cli \
  --env OUTLINE_CONFIG=/mnt/outline-cli/config.json \
  --desc "local Outline wiki (CLI)" \
  --instructions - <<'EOF'
Use `/mnt/outline-cli/outline collections list`, `... docs create ...`, etc.
Config (URL + token) is pointed to by $OUTLINE_CONFIG. No setup needed.
EOF
```

Then, from the Discord admin (`/admin` or the admin channel):

```
dev에 outline 도구 붙여줘          → grants the tool to dev
pm에서 outline 떼줘                → revokes it
등록된 도구 목록 보여줘            → lists registered tools
```

On the agent's next turn, its container gets the mount and env, and a short note
is prepended to the prompt so the agent knows the capability is there. The same
over HTTP:

```
PUT    /v1/tools/{id}     {"host_path":"...","env":{...},"instructions":"..."}
GET    /v1/tools                                          list
DELETE /v1/tools/{id}
```

Granting is part of the agent definition — a `tools` array of registered IDs —
so it can also be set directly with `PUT /v1/agents/{id}/definition`.

Notes and limits:

- **Registered tools only.** `attach_tool` refuses an ID that is not registered,
  so a hallucinated name does nothing rather than mounting a wrong path.
- **Read-only mount.** The host directory is mounted `:ro`; a tool cannot be a
  way for an agent to write outside its workspace.
- **A deleted tool is skipped, not fatal.** If a granted tool is removed, the
  agent still runs — it just no longer has that capability, logged as a warning.
- Reaching a service that lives on a local docker network (Outline at
  `http://outline:3000`) also needs `container.network` set — see the
  infrastructure guide.

## Skills — granting a Claude Code skill

A **skill** is a [Claude Code skill](https://docs.claude.com/en/docs/claude-code/skills)
— a `SKILL.md` and whatever it references — that an agent can be granted. Where a
tool adds a mounted CLI and env, a skill adds a capability the model invokes
natively: roundclaw just mounts the skill's directory into the agent's
`~/.claude/skills/<id>`, and the CLI discovers it. No env, no prompt wiring — a
`SKILL.md` is self-describing.

Registering and granting split the same way tools do — registering names a host
path (operator, over CLI/HTTP); granting only references a registered id, so it
is per-agent and safe:

```bash
# Operator registers the skill once — --path is a directory containing SKILL.md:
roundclaw skill set pptx --path /path/to/skills/pptx --desc "build .pptx decks"
roundclaw skill ls

# Grant it by adding the id to an agent's skills list (via the definition, the
# admin agent, or PUT /v1/agents/{id}/definition):
#   {"skills": ["pptx"]}
```

The same over HTTP:

```
PUT    /v1/skills/{id}     {"host_path": "...", "description": "..."}
GET    /v1/skills                                          list
DELETE /v1/skills/{id}
```

Notes:

- **Per-agent by construction.** Each agent's `~/.claude` is its own mount, so a
  granted skill is visible only to that agent — across all its conversations
  (threads included), because that directory is per-agent, not per-conversation.
- **The id is the skill's directory name.** It becomes `~/.claude/skills/<id>`, so
  keep the `SKILL.md` frontmatter `name:` in step with the id.
- **Read-only, and skipped-if-deleted.** The directory mounts `:ro`; a grant to a
  since-deleted skill is dropped with a warning, not a turn failure.
- **Tools vs skills.** A tool is a mounted CLI plus env at `/mnt/<name>`; a skill
  is a `SKILL.md` the model invokes. A skill cannot be delivered through the tool
  mount (wrong path), which is why it is its own registry.

## Agents working together — delegation

An agent can delegate work to another agent and use the answer, because roundclaw
is itself reachable over its own HTTP API. It is a special case of a tool: the
`team` tool mounts the `roundclaw` CLI into the container, and the agent runs
`roundclaw send <other> "..." --wait` to hand a task to a teammate.

The token an agent carries for this is deliberately **weaker** than the operator
token. `http.delegate_tokens_env` names a set of tokens restricted to exactly two
things — sending a request to an agent and reading agent status. Managing agents,
secrets, tools, workflows or schedules is `403`. So even if one agent is talked
into misbehaving by a message in its channel, it cannot delete or reconfigure the
others; the blast radius is "it can send another agent a request".

Setting it up (operator, once):

```bash
# 1. A restricted token the gateway recognises (add to the env it names):
#    ROUNDCLAW_DELEGATE_TOKENS=<openssl rand -hex 24>
# 2. Build the CLI for the container's platform into a directory, then register
#    it as a tool. The gateway must be reachable from the agents' network
#    (container.network) by the alias in ROUNDCLAW_URL.
roundclaw tool set team \
  --path /path/to/team-cli \
  --env ROUNDCLAW_URL=http://roundclaw-gateway:8099 \
  --desc "delegate work to another agent" \
  --instructions - <<'EOF'
Use `/mnt/team-cli/roundclaw send <agent> "..."` to delegate: it waits for the
teammate's turn to finish and prints the result, so you can use the answer in the
same turn (add `--no-wait` to just queue and move on). `... status <agent>` shows
what a teammate is doing. Delegate only when needed; never call an agent that is
calling you (no loops).
EOF
# 3. Give each collaborating agent the restricted token (encrypted) and the tool:
printf %s "$DELEGATE_TOKEN" | roundclaw secret set ROUNDCLAW_API_TOKEN --agent pm
#    then from /admin: "pm에 team 도구 붙여줘"  (or set the agent's tools list)
```

After that, from Discord: ask `pm` to *"dev에게 X를 위임해서 결과를 알려줘"* and pm
runs a turn that calls dev, waits for dev's own turn, and relays the answer.

Guard rails and limits:

- **Loops burn money.** Nothing structurally prevents A→B→A. The tool
  instructions tell agents not to form loops — keep that line, since there is no
  spend backstop behind it.
- **`--wait` blocks.** The delegating agent's container is held while the teammate
  runs, so a deep chain can occupy several `max_concurrent_turns` slots at once.
  Delegation is best kept shallow.
- **Same queue.** A delegated request waits behind whatever that agent is already
  doing (Discord included), exactly like any other request.

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
| Give an agent a secret | — | `roundclaw secret set` · `PUT /v1/agents/{a}/secrets/{name}` |
| Give an agent a local tool | register: `roundclaw tool set` · grant: `/admin` "붙여줘" | `PUT /v1/tools/{id}` · agent `tools` list |
| Give an agent a skill | register: `roundclaw skill set` · grant: agent `skills` list | `PUT /v1/skills/{id}` · agent `skills` list |
| Let agents delegate to each other | ask pm to "dev에게 위임해줘" | the `team` tool + a delegate-scoped token |

The `roundclaw` CLI covers the HTTP column from a terminal — see
[Command line](#command-line-roundclaw).
