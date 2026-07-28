---
name: roundclaw-fleet
description: Manage a roundclaw fleet through the roundclaw CLI — create, edit, enable, disable and delete agents; write personas; register and grant tools, skills and secrets; bind Discord channels; create schedules and agent-less workflows. Use when asked to add an agent, change what an agent can do or how it behaves, give an agent a capability, set up recurring work, or set up a management agent such as admin or curator. Also use when the user mentions "에이전트 만들어", "페르소나 바꿔", "도구 붙여줘", "스케줄 걸어줘", roundclaw.yaml, or the roundclaw HTTP API.
---

# Managing a roundclaw fleet

roundclaw runs Claude Code agents as durable, addressable services. This skill is
how to change what the fleet *is*: which agents exist, what each one may do, and
what runs on a schedule.

Everything here goes through the `roundclaw` CLI, which is a thin client over the
HTTP API. If the CLI is not on PATH, the same calls work with `curl` against
`$ROUNDCLAW_URL` with `Authorization: Bearer $ROUNDCLAW_API_TOKEN`.

**These commands need a full-scope token.** A delegate token — the one an
ordinary agent carries so it can hand work to another agent — is refused on every
route in this document. If a command comes back 403, that is why.

## The mental model

Five things can be registered. They compose; none of them is a special case in
the code.

| | what it is | lives in |
|---|---|---|
| **agent** | a persistent executor with a session, a queue, a workspace and channel bindings | registry |
| **persona** | an agent's instructions — its `CLAUDE.md` | the agent's workspace |
| **tool** | a host directory plus the env it needs, mounted read-only into an agent | registry |
| **skill** | a Claude Code skill directory, mounted at `~/.claude/skills/<id>` | registry |
| **schedule** | a cron trigger that sends one agent one prompt | registry + Temporal |
| **workflow** | an agent-less pipeline of prompts | registry + Temporal |

An agent's *definition* (columns: tools, model, permission mode…) and its
*persona* (prose) are two halves of one thing. Changing either mints a new
version — see `roundclaw-curator` for what to do with those.

## Agents

```bash
roundclaw agents                      # what exists
roundclaw agent show dev              # one definition in full
```

Creating and editing go through the API, because the definition is JSON:

```bash
curl -s -X POST "$ROUNDCLAW_URL/v1/agents" \
  -H "Authorization: Bearer $ROUNDCLAW_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "id": "pr-bot",
    "description": "Reviews pull requests and diffs",
    "permission_mode": "acceptEdits",
    "allowed_tools": ["Read", "Grep", "Glob", "Bash"],
    "require_mention": true,
    "reply_in_thread": true,
    "discord_channels": ["1234567890"]
  }'
```

`PUT /v1/agents/{id}/definition` replaces it. **The PUT is a whole-object
replace, not a merge** — read the current definition, change the fields you mean,
and send the result back. Sending a partial object silently blanks everything you
left out.

Fields worth understanding before you set them:

- **`require_mention`** — answer only messages that `@`-mention the bot. Any
  shared channel needs this, or the agent replies to other people's
  conversations and bills for every one.
- **`reply_in_thread`** — a message in a plain channel opens a thread, and that
  thread is its own conversation with its own session and workspace.
- **`work_dir`** — point `/workspace` at a real host directory instead of the
  managed empty one. If it is a git repository, each conversation gets its own
  worktree; if it is not, parallel conversations have no isolation and you must
  set `share_workspace` to accept that.
- **`share_workspace`** — let every conversation use the one directory. Needed
  for a non-repo `work_dir`, and for a managed workspace the agent has filled
  with checkouts: without it each thread gets a fresh empty directory and the
  agent cannot see its own work.
- **`image`** — a per-agent container image, for an agent that needs a tool the
  fleet image lacks.
- **`group_add`** — supplementary groups for the container process. The only real
  use is the docker socket, and docker socket plus its group is effective host
  root inside that container. An agent is a prompt-injection target. Grant this
  to one trusted agent or none.
- **`model`** — move one agent to a cheaper or stronger model without touching
  the fleet.

Enabling, disabling and deleting:

```bash
curl -s -X PUT "$ROUNDCLAW_URL/v1/agents/dev/definition" ... # enabled: false
roundclaw agent rm dev                                       # keeps workspace + session
```

Deleting keeps the workspace, the database, the Claude session and the version
history. Recreating the same ID resumes the conversation. Say so when you delete
something — "deleted" usually sounds more final than it is here.

## Personas

The persona is the agent's `CLAUDE.md`, and it is where behaviour actually lives.
Prefer changing it over adding rules to every request.

```bash
curl -s "$ROUNDCLAW_URL/v1/agents/dev/persona" -H "Authorization: Bearer $TOKEN"

curl -s -X PUT "$ROUNDCLAW_URL/v1/agents/dev/persona" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -H 'X-Roundclaw-Note: always reply in Korean' \
  -d '{"persona": "You are dev...\n"}'
```

`X-Roundclaw-Note` and `X-Roundclaw-Author` are recorded on the version this
write creates. Set the note. Six weeks later it is the only thing that explains
why the agent changed.

Write a persona as instructions to a colleague, not as configuration. State what
the agent is responsible for, what it must not do, what language to answer in,
and where its work lives.

## Tools, skills and secrets

A **tool** is a host directory plus env, mounted read-only. Registering one names
a host path, so it is an operator act; granting one to an agent only names an id.

```bash
roundclaw tool set outline --path /opt/outline-cli \
  --env OUTLINE_URL=http://outline:3000 \
  --desc 'Outline wiki CLI' \
  --instructions 'Use `outline search <q>` before answering wiki questions.'
roundclaw tool ls
```

A **skill** is a Claude Code skill directory — a `SKILL.md` and whatever it
needs — mounted where the CLI discovers it natively:

```bash
roundclaw skill set roundclaw-fleet --path /path/to/roundclaw/skills/roundclaw-fleet
```

Grant either by adding its id to the agent's `tools` or `skills` list in the
definition. A **secret** is injected as an environment variable:

```bash
roundclaw secret set OUTLINE_API_KEY --agent dev      # value from stdin
roundclaw secret ls --agent dev                       # names only, never values
```

Without `--agent` a secret is global and every agent sees it.

## Schedules and workflows

A schedule sends one agent one prompt on a cron. It lands in that agent's normal
queue and its default session, so a daily job builds on yesterday's.

```bash
curl -s -X PUT "$ROUNDCLAW_URL/v1/schedules/dev-standup" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{
    "agent_id": "dev",
    "cron": "0 9 * * 1-5",
    "timezone": "Asia/Seoul",
    "prompt": "Summarise yesterday'"'"'s commits.",
    "channel_id": "1234567890",
    "suppress_if": "nothing to report",
    "enabled": true
  }'
```

`suppress_if` matters more than it looks: a daily job that posts "nothing to
report" every morning trains everyone to ignore the channel.

A **workflow** is agent-less — a sequence of prompts, each receiving the earlier
outputs, with the final result posted to a channel. Use it for
collect → analyse → report, and an agent for anything conversational.

```bash
curl -s -X PUT "$ROUNDCLAW_URL/v1/workflows/news" ... # {"steps": [{"name","prompt","model"}]}
roundclaw send ... # (workflows are run with POST /v1/workflows/news/run)
```

## Setting up a management agent

`admin` and `curator` are conventions, not features. Nothing in roundclaw knows
those names: they are ordinary agents that happen to hold a full-scope token and
the CLI, so they manage the fleet by driving the same API you are driving now.

To build one — substitute any id you like:

1. **Register the CLI as a tool** so the agent can run it.

   ```bash
   roundclaw tool set admin-cli --path /opt/roundclaw-cli \
     --env ROUNDCLAW_URL=http://gateway:8080 \
     --desc 'roundclaw CLI, full scope'
   ```

2. **Give it a full-scope token** as a per-agent secret.

   ```bash
   printf '%s' "$FULL_TOKEN" | roundclaw secret set ROUNDCLAW_API_TOKEN --agent admin
   ```

3. **Create the agent**, granting the tool and the skills it needs.

   ```json
   {
     "id": "admin",
     "description": "Manages the fleet in plain language",
     "permission_mode": "acceptEdits",
     "allowed_tools": ["Read", "Grep", "Glob", "Bash"],
     "tools": ["admin-cli"],
     "skills": ["roundclaw-fleet"],
     "require_mention": true,
     "discord_channels": ["<a PRIVATE channel>"]
   }
   ```

4. **Write its persona**, saying what it manages and what it must refuse.

**Lock it down.** This agent holds a token that can create and delete every other
agent, so its safety rests entirely on who can reach its channel:

- Bind it to a **private** channel only trusted operators can post in, and keep
  `require_mention` on.
- Apply the roundclaw allow-list (`discord.allowed_roles` / `allowed_users`).
- Do **not** give it web access, and do not let other agents delegate to it.
  Either one becomes a path from an untrusted prompt to a full-access token.

For a management agent that reviews and improves the fleet rather than just
editing it on request, add the `roundclaw-curator` skill and read that document —
it covers versions, evals and the approval queue.

## Things that will bite you

- **A definition PUT replaces everything.** Read, modify, write.
- **Edits take effect on the next turn.** A turn already running keeps the
  arguments it started with; that is deliberate.
- **One channel, one agent.** Binding a channel that is already bound is a
  conflict, not a silent takeover.
- **The YAML agent list is a one-time bootstrap.** Once the registry has any
  agent, editing `roundclaw.yaml` does nothing. The registry is the truth.
- **A managed workspace starts empty in a new conversation.** An agent asked to
  fix a checkout it has never cloned lands in an empty directory. Give it a git
  `work_dir` first.
- **`allowed_tools` does not restrict anything.** It pre-approves tools so they
  run without a prompt, and agents run headless, where nothing prompts. An agent
  whose list names four tools is offered all twenty-nine the CLI has; so is one
  whose list is empty. This has been measured against the image, under every
  permission mode, including `acceptEdits`. Write the list to record what an
  agent is *for* if you like, but do not read it as a limit, and do not answer
  "can this agent run arbitrary shell commands?" from it — the answer is yes.
  What withholds a tool is a deny rule, which no agent field exposes yet.
