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
      conversation, queued_at, finished_at)
  INDEX (status, id), (conversation, id)

idempotency(key PK, turn_id, created_at)
```

- `origin` is a JSON discriminated union (`core.Origin`) and is the single
  source of truth for where a reply goes. A new event source adds a type here
  and a case in `DeliverResponse`, nothing more.
- `conversation` is empty for the agent's default conversation and the Discord
  thread ID otherwise. `RecentTurnsIn` scopes the recap window to one
  conversation — recapping a thread with another thread's history would hand the
  agent a conversation it never had.
- `cost_usd` doubles as the accounting source for spend limits
  (`UsageSince`), which is why it must be committed rather than held in the
  workflow.
- `turns` is **not** replayed into prompts. The live Claude session already
  holds the context; the window exists for user-facing summaries and for
  session-loss recovery.

## `registry.db`

```sql
agents(id PK, description, agent_name, permission_mode, allowed_tools,
       additional_dirs, work_dir, deny_paths, require_mention,
       share_workspace, enabled, created_at, updated_at)

agent_channels(channel_id PK, agent_id → agents.id ON DELETE CASCADE)

schedules(...)   -- see internal/registry/schedules.go
```

`channel_id` is the primary key of the binding table, so one Discord channel can
never map to two agents. Enforcing it in the schema rather than in a config
check means it holds even when two processes write concurrently.

List columns (`allowed_tools`, `additional_dirs`, `deny_paths`) are JSON arrays
in a TEXT column. They are read as a unit and never queried into, so a join
table would cost more than it explains.

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

Live logs are the bulk of the data and the least valuable after the fact — they
exist to answer "what is it doing *right now*".

## Operational note

Deleting an agent's workspace directory while a process holds the database open
does not reset it: the cached handle keeps pointing at the deleted inode and
goes on serving stale data. Restart both processes, or use a fresh agent ID.
