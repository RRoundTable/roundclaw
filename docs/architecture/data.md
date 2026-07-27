# Data

Two kinds of SQLite file, both `modernc.org/sqlite` (pure Go, no CGO) in WAL
mode with a 5 s busy timeout.

| File | Holds | Written by |
|------|-------|------------|
| `workspace/registry.db` | which agents and schedules exist | gateway |
| `workspace/<agent>/state.db` | one agent's turns, logs, runtime state | worker **and** gateway |

## Why two processes share a file

The split is not incidental — it is what makes `/status` fast:

- The **gateway** inserts the `turns` row before signalling, because HTTP must
  return a `turn_id` in its 202 and a workflow cannot touch SQLite without
  breaking determinism. It also reads the file to answer status, turn lookups
  and SSE.
- The **worker** appends `live_logs` during a turn and closes the row out.

WAL is what lets a reader arrive mid-write without blocking, and `busy_timeout`
turns the remaining collisions into a short wait instead of an error. Writers
are capped at `SetMaxOpenConns(1)`: SQLite serialises writes anyway, and extra
connections only convert that serialisation into `SQLITE_BUSY`.

`applySchema` retries on "database is locked" because both processes apply the
schema at startup, and switching a fresh database into WAL needs a brief
exclusive lock that `busy_timeout` does not cover — without the retry, whichever
process loses the cold-start race fails to boot.

## Per-agent `state.db`

```sql
agent_runtime(agent_id PK, status, current_turn, session_id, updated_at)

live_logs(id PK, turn_id, kind, content, created_at)
  INDEX (turn_id, id)

turns(id PK, request, result, status, cost_usd, origin, error,
      conversation, attachments, queued_at, finished_at)
  INDEX (status, id), (conversation, id)

idempotency(key PK, turn_id, created_at)
```

- `origin` is a JSON discriminated union (`core.Origin`) and is the single
  source of truth for where a reply goes. A new event source adds a type here
  and a case in `DeliverResponse`, nothing more.
- `origin.audience` rides inside that JSON — no column of its own, so no
  migration — and answers a different question: not "where does the result go"
  but "who is watching this work". They diverge only under delegation, where the
  result goes one hop back to the delegator while the person is at the root of
  the chain. It is stamped at admission and inherited hop by hop, which is what
  lets an agent three delegations deep report progress into the thread the work
  was actually asked in ([delegation.md](delegation.md#the-audience)).
- `attachments` is the JSON list of host paths for a turn's uploads, written at
  admission and read by the worker once it has chosen the workspace to link them
  into. See [adapters.md](adapters.md#discord-discordgo).
- `conversation` is empty for the agent's default conversation and the Discord
  thread ID otherwise. `RecentTurnsIn` scopes the recap window to one
  conversation — recapping a thread with another thread's history would hand the
  agent a conversation it never had.
- `cost_usd` is the per-turn accounting figure, recorded when the CLI reports
  it on completion.
- `turns` is **not** replayed into prompts. The live Claude session already
  holds the context; the window exists for user-facing summaries and for
  session-loss recovery.

## `registry.db`

```sql
agents(id PK, description, agent_name, permission_mode, allowed_tools,
       additional_dirs, work_dir, deny_paths, require_mention,
       share_workspace, reply_in_thread, tools, skills, image, group_add,
       model, enabled, created_at, updated_at)

agent_channels(channel_id PK, agent_id → agents.id ON DELETE CASCADE)

schedules(...)   -- see internal/registry/schedules.go

secrets(scope, name, ciphertext, created_at, updated_at, PK(scope, name))

workflows(id PK, description, channel_id, steps, enabled, created_at, updated_at)

tools(id PK, description, host_path, env, instructions, created_at, updated_at)

skills(id PK, description, host_path, created_at, updated_at)

agent_versions(agent_id, version, definition, persona, note, author, created_at,
               PK(agent_id, version))          -- deliberately no FK; see below

eval_sets(id PK, agent_id, description, cases, full_grants, enabled, ...)
eval_runs(id PK AUTOINCREMENT, eval_set_id, agent_id, version, status, score,
          passed, total, cost_usd, error, notify_agent, notify_conversation, ...)
eval_results(run_id, case_name, output, score, passed, reason, cost_usd,
             duration_ms, PK(run_id, case_name))

proposals(id PK AUTOINCREMENT, kind, target, payload, rationale, evidence,
          status, created_by, created_at, decided_by, decided_at, decision,
          applied_version)
```

`channel_id` is the primary key of the binding table, so one Discord channel can
never map to two agents. Enforcing it in the schema rather than in a config
check means it holds even when two processes write concurrently.

**`turns.origin` is the return address, and one of its four types creates work.**
`discord` posts to a channel, `http_poll` records and stops, `http_callback` POSTs
to a URL — and `agent` hands the result to another agent as a new request in a
named conversation of theirs. That last one is how a delegated turn reports back
without the delegator waiting: the address is on the row, so the result is
delivered even if the caller, its shell and the worker have all died in between
([orchestration.md](orchestration.md)). Because it *queues* rather than just
delivers, admission refuses the one shape that cannot end — an agent naming itself
in the conversation it is already running in.

That address is one edge of the delegation tree, which is all a *result* needs —
it travels back a hop at a time, each hop reading the next address out of its own
conversation. It is not enough to speak mid-turn, which has to reach the person
at the root; that is what `origin.audience` carries, resolved once at admission
rather than searched for later.

**`secrets` holds values, encrypted.** `scope` is an agent ID, or `''` for a
global secret every agent sees; a per-agent row of the same name overrides the
global one. `ciphertext` is `base64(nonce ‖ AES-256-GCM)`, sealed with a key
derived from `container.secrets_key_env` — a value that never touches the
database. No FK on `scope`: a global secret has no agent to reference, and the
row deliberately outlives an agent delete just as the workspace does. `List`
returns names only; the sole path that decrypts is `SecretsForAgent`, called by
the activity to build a container's environment
([agent-runtime.md](agent-runtime.md#registered-secrets)). Without a configured
key the store is off and every write fails closed rather than storing plaintext.

**`workflows` are agent-less pipelines.** `steps` is a JSON array of
`{name, prompt, permission_mode, allowed_tools, model}`, read as a unit like the
other list columns. `channel_id` is where the final result is posted, empty to
record the run and deliver nowhere. A workflow keeps its own `state.db` under
`workspace/workflows/<id>/`, separate from any agent, where each run's steps are
recorded as turns ([orchestration.md](orchestration.md#runworkflow--the-agent-less-pipeline)).

**`tools` are grantable local capabilities.** A row bundles a `host_path`
(mounted read-only at `/mnt/<basename>`), an `env` JSON map, and `instructions`.
An agent's `tools` column lists the IDs it is granted; the activity resolves them
each turn ([agent-runtime.md](agent-runtime.md#registered-tools)). Registering a
row names a host path and is operator-only; granting an ID to an agent is safe in
plain language, since only registered IDs resolve. No FK from `agents.tools` to
`tools`: it is a JSON list like the other list columns, and a grant referencing a
since-deleted tool is skipped at turn time rather than blocked at write.

**`skills` are grantable Claude Code skills.** Simpler than a tool — just an id,
`description`, and a `host_path` to a `SKILL.md` directory — because a skill
carries no env and no injected prompt. An agent's `skills` column lists the IDs it
is granted; the activity mounts each read-only at `~/.claude/skills/<id>`, a
nested mount over ClaudeHome where the CLI discovers it
([agent-runtime.md](agent-runtime.md#registered-skills)). Same operator/grant
boundary and same skip-if-deleted behaviour as tools.

**`image` and `group_add` override the container an agent runs in.** `image`
(empty by default) points one agent at a purpose-built image instead of the
global `container.image` the fleet shares — a dev agent on an image with the
docker CLI, say — while every other agent stays on the default. The image must
already exist on the host; the worker starts it by name and builds nothing.
`group_add` (a JSON array, empty by default) lists supplementary groups the
container process joins via docker `--group-add`. Its one real use is reaching a
host socket a tool mounts but the container's user is not in the owning group of
— `/var/run/docker.sock`, owned by the host's docker group. The two together
plus the socket mount are what let an agent drive the host daemon; each is inert
without the others, and the pair is effective host root, so they belong on a
single trusted agent (see [adapters.md](adapters.md#admin-is-an-agent)).

**`model` overrides which model an agent runs on.** Empty (the default) means the
fleet-wide `container.model`, and an empty setting there leaves the choice to the
CLI's own default — which is whatever the agent image's version ships, so two
agents on different images can silently disagree. Naming a model makes that an
operator decision instead of an image detail. The value is passed straight to
`claude --model`, so it must be a name the CLI accepts (`claude-opus-5`). A
workflow step's `model` plays the same role for a step
([orchestration.md](orchestration.md#runworkflow--the-agent-less-pipeline)).

List columns (`allowed_tools`, `additional_dirs`, `deny_paths`, `group_add`) are
JSON arrays in a TEXT column. They are read as a unit and never queried into, so
a join table would cost more than it explains.

**`agent_versions` has no foreign key, on purpose.** Deleting an agent keeps its
workspace and session so that recreating the ID resumes it; a cascade would take
the history on the same delete, destroying the record of what the agent was
exactly when somebody needs to put it back. Numbering continues across a delete
and recreate, so v1 keeps meaning the first thing that ID ever was.

A version captures the definition **and** the persona together, because they are
one artefact in practice: an agent whose `CLAUDE.md` was rewritten is a different
agent even though every column is unchanged. The persona is a file in the
workspace, which the registry does not own — so it is read through an injected
`PersonaSource` (`registry.PersonaFromWorkspace(cfg.WorkDir)`, set by both
binaries at startup) rather than by teaching the registry the workspace layout.
Snapshots happen inside the same transaction as the definition write; a version
that could be lost in between would leave a hole in the history, which reads as
"nothing changed then".

A write whose content matches the current version records nothing. Toggling an
agent off and on, or a client PUTting back what it just read, would otherwise
mint versions that say nothing, and a list where most rows are noise is one
nobody scrolls through to find the row that matters.

**`eval_runs.version` pins what was measured.** A run asked for version 0 ("live")
resolves it to a concrete number when it starts: a run that cannot say what it
measured is worthless the moment the agent changes again. Results are written per
case as each finishes, so a run that lost a worker halfway still shows what it
learned, and the aggregate is computed from the rows rather than carried through
the workflow.

**`proposals.applied_version`** records the version an approval produced, which is
what makes the undo hint (`roundclaw version rollback <target> <n-1>`) possible.
`DecideProposal` compare-and-sets on `status = 'pending'`, so two people pressing
Approve at the same moment cannot apply the same change twice.

## Migrations

`CREATE TABLE IF NOT EXISTS` does nothing to a table that already exists, so a
new column is invisible to any deployment that has run before — every query then
fails with `no such column` and the process crash-loops. Both packages therefore
keep a `migrations` slice applied on open, whose "duplicate column name" error
is treated as success:

```go
var migrations = []string{
    `ALTER TABLE agents ADD COLUMN work_dir TEXT NOT NULL DEFAULT ''`,
    ...
}
```

**Only ever append.** Rewriting or reordering an entry diverges migrated
databases from fresh ones.

## Retention

`cmd/worker/retention.go` prunes on `retention.interval`:

- `live_logs` older than `retention.transcript_days`
- finished `turns` older than `retention.turn_days`
- staged uploads in `inbox-staging/` older than `retention.upload_days`

Live logs are the bulk of the data and the least valuable after the fact — they
exist to answer "what is it doing *right now*".

The upload sweep touches roundclaw's own copy, not the agent's. An attachment is
hard-linked into the workspace when its turn runs
([adapters.md](adapters.md#discord-discordgo)), so while that link lives the
staged entry shares the inode and removing it frees nothing — it is kept so a
retried activity can still find its files. It earns its keep once a
conversation's workspace is torn down and the staged entry becomes the last link
holding the bytes.

The agent's own `inbox/` and `outbox/` are never swept. Those are documents
someone sent it and work it produced, sitting in its working directory; deleting
them on a timer is not roundclaw's call to make.

## Operational note

Deleting an agent's workspace directory while a process holds the database open
does not reset it: the cached handle keeps pointing at the deleted inode and
goes on serving stale data. Restart both processes, or use a fresh agent ID.
