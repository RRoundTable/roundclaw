# Agent runtime

An agent turn is one `claude` process inside one container. roundclaw ships no
code into that container and does not use the Agent SDK — it drives everything
through argv and stream-json on stdout.

## Session identity

```go
sessionNamespace = 6f0a1c2e-9d4b-5a7f-8c31-0b2d4e6f8a10   // fixed forever
SessionID(workflowID) = uuid.NewSHA1(sessionNamespace, workflowID)
```

This is the load-bearing trick of the design. Because the session UUID is a pure
function of the workflow ID, a retried activity — or one picked up by a
different worker after a crash — reconnects to the same Claude session. Nothing
has to capture a session ID from process output and persist it, so there is no
window in which that write can be lost.

Consequences:

- `sessionNamespace` and `contract.WorkflowID`'s format must not change.
  Either orphans every existing session. The format was unified once, on
  purpose, accepting that one-time loss — `roundclaw-<agentID>-<conv>` for every
  conversation, with a `default` sentinel for the default one — and must not
  drift again.
- Whether to pass `--session-id` (create) or `--resume` (continue) is decided by
  `sessionReady` — *observed* session establishment, not turn count. An early
  failure once wedged an agent permanently because `turnCount > 0` claimed a
  session existed that never had.

## Container invocation

`internal/claude/args.go` builds the argv:

```
docker run --rm --name roundclaw-<agent>-<turn>
  --workdir /workspace
  -v <agent>/work:/workspace
  -v <agent>/claude-home:/home/node/.claude
  -e CLAUDE_CODE_OAUTH_TOKEN            # by name, never by value
  [-e SECRET_NAME] ...                  # registered secrets, also by name
  [-v /dev/null:<denied path>:ro] ...
  [-v <dir>:/mnt/<base>:ro] ...
  <image> claude -p "<prompt>"
    (--session-id <uuid> | --resume <uuid>)
    --output-format stream-json --verbose
    [--agent NAME] [--permission-mode MODE]
    [--allowedTools a,b,c] [--add-dir /mnt/x] ...
```

Details that are load-bearing rather than stylistic:

- **The prompt sits immediately after `-p`.** `--allowedTools` is variadic; a
  prompt after it is swallowed and `claude` exits with "Input must be provided",
  having never seen the request.
- **Credentials pass by name (`-e NAME`), not value**, so the secret appears
  neither in the process table nor in a Temporal history event.
- **`--verbose` is required** for stream-json to emit anything before the final
  result. `--include-partial-messages` is deliberately omitted: a token-level
  delta per event would turn one turn into thousands of SQLite inserts.
- **Deny paths are `/dev/null` mounts** layered over the workspace mount. Docker
  orders nested mounts by path depth, so listing them after is enough;
  `/dev/null` is read-only and reads as empty. `ValidateDenyPath` rejects
  escapes.
- **The container name is deterministic** (`agent` + `turn`), and
  `RemoveArgs` force-removes a leftover before a retry. That is what makes
  reusing the name safe after a crash.
- **Stopping is explicit**: `SIGINT`, wait `container.stop_grace`, then
  `SIGKILL`.

## Native subagents

`--agent NAME`. **This is Claude Code's own subagent feature — the same `.claude/agents/*.md`
personas you would use from the CLI directly.** roundclaw does not wrap it,
reimplement it, or invent a subagent system of its own; it uses the CLI's as-is.
Its entire involvement is a single argv flag:

```go
if s.AgentName != "" {
    args = append(args, "--agent", s.AgentName)   // args.go
}
```

The name comes from the agent's `agent_name` field in `registry.db`. That is the
whole integration, and the boundary is deliberate:

- **roundclaw does not define the subagent.** It passes a *name*, never a
  definition. The persona must already exist where the CLI looks for it (the
  mounted `claude-home` or the workspace's `.claude/`). roundclaw never writes
  those files, so `--agents` JSON is not passed either.
- **roundclaw does not track its history.** A native subagent's transcript lives
  inside the Claude session, which `--resume` already restores. There is nothing
  for roundclaw to persist.

Native subagents already do definition, history and resumption correctly, so
reimplementing any of it would only add a way to disagree with the CLI.

### Relationship to Temporal: none

A native subagent lives **entirely below the Temporal boundary.** It exists only
as argv handed to the `claude` process that `RunClaudeTurn` starts inside the
agent container. The `SubAgent` *workflow* — which drives that activity and does
own queue, signals and Continue-As-New ([orchestration.md](orchestration.md)) —
never learns which persona is running, and does not need to:

| | Native subagent (`--agent NAME`) | `SubAgent` workflow |
|---|---|---|
| What it is | a Claude Code persona | a Temporal execution |
| Where it runs | inside the activity's container | in the worker, as workflow code |
| Owned by | the CLI | Temporal |
| Named "subagent" because | it is one | historical coincidence |

So changing `agent_name` changes only the argv of the next turn — the queue, the
session identity, cancellation, retries and Continue-As-New are all unaffected.
The name collision between the two is exactly that: a collision, not a link.

## Streaming

`internal/claude/stream.go` decodes stream-json into `core.LogKind` values:
text, tool use, tool result, API retry, system, and `rate_limit` — the last
added after the real CLI began emitting `rate_limit_event` per turn and the raw
JSON leaked into `/status`. An unrecognised event becomes `KindOther` and is
preserved verbatim, so a newer CLI cannot silently drop information.

## Workspaces and conversation isolation

`internal/temporal/activity/workspace.go`. Conversations run in parallel, so
they cannot share a working tree.

| Agent's workspace | A conversation gets |
|-------------------|---------------------|
| managed directory (`work_dir` empty) | its own subdirectory — it starts empty anyway |
| a git repository | `git worktree add --detach` |
| a non-repo `work_dir` | refused, unless `share_workspace` is set on the agent |

The worktree is **detached** on purpose: a worktree checked out on a branch
locks that branch, so two conversations — or a person working in the original
checkout — would fight over it.

Removal goes through `git worktree remove`, not `rm -rf`, or the repository
keeps an administrative entry pointing at a directory that no longer exists. It
is never automatic: a quiet thread usually still has uncommitted work in it.

The **default conversation** always uses the agent's workspace directly. It is
what `/ask`, schedules and webhooks share, and it is what existed before
conversations did.

## Authentication

`container.oauth_token_env` (from `claude setup-token`) wins over
`container.api_key_env`. Reusing `~/.claude/.credentials.json` would be a
mistake: those credentials belong to an interactive session, and a container
refreshing them can rotate the refresh token out from under the human still
using it.

## Registered secrets

An agent can carry extra secrets — a `GITHUB_TOKEN`, a tool's API key — stored
encrypted in the registry ([data.md](data.md#registrydb)) and injected as
container environment variables. The activity calls `SecretsForAgent`, which
merges global and per-agent secrets, and puts them on the `RunSpec`.

They are injected the **same way as the credential**: `Args` emits `-e NAME`
(name only), and the activity sets the value on the runtime subprocess's
environment. So a registered secret never appears in argv, the host process
table, or a Temporal history event — only inside the container, where it is
meant to be. Values are sorted by name for a deterministic argv.

Two guards: the credential's `-e` precedes the secrets', and the activity drops
any secret whose name collides with the credential env — authentication cannot
be broken by a stray secret. When no master key is configured, `SecretsForAgent`
returns nothing and the turn runs exactly as it did before secrets existed.

## Registered tools

A **tool** is a named local capability — a CLI and its config on the host — that
an agent can be granted. It bundles three things: a host directory (mounted
read-only at `/mnt/<basename>`), the environment it needs, and a note on how to
use it. Tools live in the registry ([data.md](data.md#registrydb)); an agent's
grants are the `tools` array on its definition.

`RunClaudeTurn` resolves the grants each turn through `resolveTools`, which for
each granted ID:

- appends the tool's host path to `RunSpec.AdditionalDirs` (so `Args` emits the
  `:ro` mount and `--add-dir`),
- merges the tool's env into the same map that carries secrets (injected `-e
  NAME`, value on the subprocess env — never in argv), and
- prepends a short note to the prompt so the agent knows the capability is there.

The split between **registering** and **granting** is a security boundary:
registering names a host path and is an operator act (CLI/HTTP with a bearer
token); granting only references a registered ID, so it is safe to do in natural
language from `/admin` (`attach_tool` / `detach_tool`) — a hallucinated name
resolves to no tool rather than mounting an arbitrary path. A granted tool that
was since deleted is skipped with a warning, not a turn failure: an agent should
still answer when a capability is pulled out from under it.

Reaching a service on a local docker network (an Outline at `http://outline:3000`)
also needs `container.network` — see [infrastructure.md](infrastructure.md).

## Registered skills

A **skill** is a Claude Code skill an agent can be granted: a `SKILL.md`
directory on the host, listed by id in the agent's `skills`
([data.md](data.md#registrydb)). `RunClaudeTurn` resolves the grants through
`resolveSkills` into a map of id → host path, which `Args` mounts read-only at
`~/.claude/skills/<id>` — one `-v` per skill.

The mount is what makes this work with no other wiring. It is *nested* under the
ClaudeHome mount (`/home/node/.claude`), and Docker orders overlapping mounts by
path depth, so the skill lands on top of the volume exactly like the deny-path
`/dev/null` shadows land on top of the workspace. The CLI then discovers the
skill from its normal `~/.claude/skills` location — no `--add-dir`, no flag, no
prompt injection, because a `SKILL.md` is self-describing.

A skill is therefore **per-agent by construction**: `~/.claude` is a per-agent
mount, so a granted skill reaches every one of that agent's conversations
(threads included) and no other agent. Unlike a tool it carries no env; unlike
the persona it is not copied into thread workspaces, because it rides the
per-agent ClaudeHome rather than the per-conversation workspace. A grant to a
since-deleted skill is skipped with a warning, not a turn failure.

> Note the `--bare` exception: the router skips skill discovery
> ([adapters.md](adapters.md#discord-discordgo)), but an ordinary agent turn does
> a full startup, so granted skills are always seen.

## The image

`container/Dockerfile`: `node:22-slim` + `git`, `ca-certificates`, `ripgrep`,
and `npm install -g @anthropic-ai/claude-code`. Nothing else.

`container/fake/claude` is a scripted stand-in used by CI to assert the argument
order the real CLI requires — the `-p` positioning bug above was invisible until
an agent had tools configured, so the ordering is now a test.
