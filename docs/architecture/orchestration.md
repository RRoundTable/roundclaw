# Orchestration

Temporal is what makes an interrupted turn resumable rather than lost. It owns
two things and only two: the request queue, and the durability of the work.
Everything a user can *observe* is served from SQLite instead
([data.md](data.md)).

## What you register: agents, schedules, workflows, evals

Four kinds of work live in the runtime registry ([registry.db](data.md#registrydb))
and can be created, changed, and removed *while roundclaw runs* — no restart, no
YAML edit. Each maps to a different Temporal execution described below.

```mermaid
flowchart TB
    API[HTTP API]
    CLI[roundclaw CLI]
    ADM[admin agent]
    API --> REG
    CLI --> REG
    ADM --> REG
    REG[(registry.db)]

    REG --> AGENT[Agent<br/>persistent · conversational]
    REG --> SCHED[Schedule<br/>cron trigger]
    REG --> WF[Workflow<br/>agent-less pipeline]
    REG --> EV[Eval set<br/>cases + marking]
    REG -.-> MOD[Tool / Secret<br/>modifiers]

    SCHED -->|runs on| AGENT
    MOD -. granted to .-> AGENT
    EV -. measures .-> AGENT

    subgraph temporal["Temporal executions (durable)"]
        SA[[SubAgent<br/>one per conversation]]
        SR[[ScheduledRequest<br/>→ default conversation]]
        RW[[RunWorkflow<br/>ordered steps]]
        ER[[EvalRun<br/>cases against one version]]
    end
    AGENT ==> SA
    SCHED ==> SR
    WF ==> RW
    EV ==> ER
```

| | **Agent** | **Schedule** | **Workflow** | **Eval set** |
|---|---|---|---|---|
| What it is | a persistent, conversational bot | a cron trigger on an agent | an agent-less pipeline of prompts | cases that measure one agent |
| Runs as | `SubAgent`, one per conversation | `ScheduledRequest` → the agent's default conversation | `RunWorkflow`, ordered steps | `EvalRun`, one case at a time |
| Session / memory | yes — resumes its Claude session | uses its agent's session | none — each step is one-shot, chained by passing outputs forward | none — each case is a fresh session in a throwaway workspace |
| Triggered by | a Discord message or an API request | a cron on a Temporal Schedule | started by hand (or, later, a schedule) | started by hand or by an agent reviewing the fleet |
| Channel | bound to one (one channel → one agent) | reports into a channel | reports the final result into a channel | none — it wakes whoever asked |
| Registry table | `agents` (+ `agent_channels`) | `schedules` | `workflows` | `eval_sets`, `eval_runs`, `eval_results` |

How they relate:

- **A schedule always runs on an agent** — it is a recurring prompt fired into
  that agent's *default* conversation, not a standalone thing. There is no
  agent-less schedule; a standalone or multi-step recurring job is a **workflow**
  instead (and a workflow can, in turn, be put on a schedule).
- **A workflow is deliberately agent-less** — no session, queue, or channel
  binding, because a scheduled or one-off pipeline needs none of what an agent
  carries. Its steps each run as their own activity, so a crash resumes at the
  step it was on rather than from the top.
- **Tools and secrets are modifiers, not work.** A registered tool
  ([agent-runtime.md](agent-runtime.md#registered-tools)) or secret is *granted*
  to an agent to widen what a turn can do; neither runs on its own.
- **An eval set measures an agent without being one.** It runs the agent's pinned
  *version* — definition and persona together — in a throwaway workspace, so a
  run says what configuration produced its numbers and two runs can be compared.
  It is the only registerable thing that produces a verdict rather than work.

Two more kinds of row are records rather than work: `agent_versions`, written
automatically on every definition or persona change, and `proposals`, changes
written down for a person to approve. Neither has a Temporal execution — a
proposal is applied through the ordinary registry calls when somebody approves
it, so an approved change mints a version exactly like a hand edit.

All three are registered the same three ways, because they are all just registry
rows — nothing needs a redeploy to appear:

- **HTTP API** — `POST/PUT /v1/agents`, `/v1/schedules/{id}`, `/v1/workflows`
  ([adapters.md](adapters.md#http-httpgo)).
- **CLI** — `roundclaw`, a thin client over those routes
  ([usage.md](../usage.md#command-line-roundclaw)).
- **The admin agent** — describe the change in plain language and it drives the
  same API itself ([adapters.md](adapters.md#admin-is-an-agent)).

The YAML `agents:` list is a one-time bootstrap only: it seeds an empty registry
at first start and is ignored afterwards ([data.md](data.md#registrydb)). The
three Temporal executions these map to are next.

## Workflows

### `SubAgent` — one per conversation

> **The name is about orchestration, not Claude.** A `SubAgent` workflow is
> roundclaw's durable executor for one conversation. It is unrelated to Claude's
> *native* subagents (`--agent NAME`), which are a CLI feature the workflow knows
> nothing about — see [agent-runtime.md](agent-runtime.md#native-subagents). One
> is a Temporal execution that owns a queue; the other is a persona the `claude`
> process runs under. They can coexist without ever touching.

`internal/temporal/workflow/subagent.go`. Long-lived, one execution per
`contract.WorkflowID(agentID, conversationID)`:

```
roundclaw-<agentID>-default          default conversation
roundclaw-<agentID>-<conversationID> a Discord thread
```

A conversation — not an agent — is the unit that owns a Claude session, a queue
and a workspace. Every ID has the same three-part shape; the default
conversation uses the `default` sentinel (`contract.DefaultConversation`) rather
than a second format. The sentinel cannot collide with a thread, whose ID is an
all-digit Discord snowflake.

State held in the workflow:

| Field | Purpose |
|-------|---------|
| `queue []core.Request` | pending requests, in order |
| `turnCount` | cumulative, reported by the status query |
| `currentTurnID` | the turn being executed |
| `lifecycle` | idle / running / stopping |

The queue is a slice, not a "messages arrived" flag. Three requests during one
long turn produce three turns and three replies — that regression is the reason
roundclaw exists.

Nothing about the Claude session is in that table, and nothing about the world
outside the workflow belongs there. The workflow held whether the session was
open until a turn that failed before the container started was read as the
session being gone; every later turn then tried to create one that already
existed, for good. Replay rebuilt the wrong value as faithfully as it would have
rebuilt the right one, and Continue-As-New carried it across. The test is whether
the workflow can check the thing it is holding: it owns its queue, and it cannot
see the filesystem, which is the signal that the session was never its decision
to make.

**Signals**

| Signal | Effect |
|--------|--------|
| `enqueue` | append to the queue |
| `steer` | cancel the running activity, put the instruction at the head of the queue, start immediately — same session, so context survives |
| `stop` | cancel the running activity and drop the queue |

**Query**

`status` returns lifecycle, queue depth and turn count — the things the
workflow genuinely owns, held in memory, so answering breaks no determinism
rule. It never reads SQLite: a query handler runs in workflow context and cannot
do I/O.

**Continue-As-New** fires at `maxTurnsPerRun = 100` or when
`GetContinueAsNewSuggested()` is true. The queue crosses the boundary, because
dropping it would lose a request. Nothing else does.

The turn count deliberately does **not**. It bounds one run's history, and the
continued run starts with none. Carrying it forward made the limit true again on
the continued run's first loop, so an agent that reached the seam with anything
queued continued as new about once a second, forever, and never ran the turn that
was waiting — the only escape, an empty queue, was the one condition the loop
never got far enough to reach. The cumulative count lives in SQLite.

**Cancellation cleanup** runs in a `workflow.NewDisconnectedContext`; a
cancelled context cannot start the activity that closes out the turn row.
`abandonQueue` marks dropped turns so they do not sit at `running` forever,
misreporting the agent as busy long after the turn is gone.

### `ScheduledRequest`

Started by a Temporal Schedule; carries only a schedule ID. The definition is
read at fire time, so editing a schedule takes effect on the next run with
nothing to recreate in Temporal. It always targets the agent's **default**
conversation: a daily report that opened a new conversation each morning could
never build on what it said yesterday.

### `RunWorkflow` — the agent-less pipeline

`internal/temporal/workflow/runworkflow.go`. A workflow is not an agent: no
session, no queue, no channel binding. It is a definition of ordered *steps* —
each a prompt with its own settings — that Temporal runs as a pipeline, feeding
each step's output to the next and delivering the final result to a channel.

- `LoadWorkflow` reads the definition at run time (an edit takes effect on the
  next run), then the workflow loops the steps, running each through
  `RunWorkflowStep` and accumulating a context string of prior outputs.
- **Each step is its own activity**, so a worker crash resumes the pipeline at
  the step it was on rather than from the top — the whole reason to model a
  pipeline in Temporal rather than one long turn.
- Started manually (`Dispatcher.RunWorkflow` → `ExecuteWorkflow`, one execution
  per run so runs never collide) or, later, by a schedule. It is agent-less
  precisely because a scheduled or one-off job needs none of what an agent
  carries; forcing one on it was ceremony.

### `EvalRun` — measuring one agent version

`internal/temporal/workflow/evalrun.go`. An eval set's cases, run against a
pinned agent version, marked, and totalled. Started by
`Dispatcher.StartEvalRun`, which writes the run row *before* starting the
execution — so the caller has something to poll immediately, the execution ID is
derived from the run ID (`roundclaw-eval-<id>`, retry-safe), and a start that
fails marks the row failed rather than leaving it at "running" forever.

```mermaid
flowchart TB
    START["Dispatcher.StartEvalRun<br/>writes the eval_runs row first"]
    START --> PLAN["LoadEvalPlan<br/>set · the pinned version's definition + persona"]
    PLAN --> ON{"set enabled?"}
    ON -->|no| FIN

    subgraph loop["one case at a time"]
        RUN["RunEvalCase<br/>throwaway workspace · secrets withheld"]
        RUN --> E{"activity or container error?"}
        E -->|yes| ZERO["zero, with the reason"]
        E -->|no| A{"must_contain / must_not_contain broken?"}
        A -->|yes| ZERO
        A -->|no| R{"rubric set?"}
        R -->|no| SMOKE["1 · it answered at all"]
        R -->|yes| JUDGE["JudgeEvalCase<br/>small model · no tools"]
        JUDGE --> JOK{"judge returned a verdict?"}
        JOK -->|no| UNMARKED["unmarked — not zero"]
        JOK -->|yes| MARK["score · passed · reason"]
        ZERO --> REC
        SMOKE --> REC
        UNMARKED --> REC
        MARK --> REC
        REC["RecordEvalCase<br/>written now, not at the end"]
    end

    ON -->|yes| RUN
    REC -.->|next case| RUN
    REC --> CLEAN["CleanEvalWorkspaces<br/>best effort"]
    CLEAN --> FIN["FinishEvalRun<br/>totals the recorded rows"]
    FIN --> NOTIFY["NotifyEvalRun<br/>wakes whoever asked"]
```

Every path down the middle of that loop ends at the same place, which is the
point: a case that errored, a case that broke an exact rule and a case a judge
never saw all leave a row behind. The only exit that skips it is a *failure to
write* one — losing a result would leave the aggregate describing cases it
cannot see, so that one does stop the run.

- `LoadEvalPlan` reads the set and the pinned version's definition **and**
  persona. Version 0 means "whatever is live" and is resolved to a concrete
  number here, because a comparison needs both sides pinned.
- Cases run **one at a time**. They are container starts against a shared host
  and the same `max_concurrent_turns` budget as real work; an eval is background
  work and running ten at once would starve the agents doing the job.
- Each case is scored in this order: an activity error is a recorded zero; a
  container error is a recorded zero; a broken `must_contain` fails without a
  judge ever being asked (an exact rule should not be subject to a model's
  opinion, and skipping the judge is also free); no rubric means a smoke test
  that passes on any answer; otherwise `JudgeEvalCase` marks it.
- **A failing case is not a failing run.** An agent that errors on one question
  has answered the other nine, and those answers are what a comparison needs. A
  case that never ran is still recorded, with the reason — a missing row would
  read as a case that was never asked, and the next comparison would call it
  removed rather than broken.
- A judge that cannot be reached leaves the case *unmarked* rather than zero:
  the answer exists and was not marked, and scoring it zero would invent a
  regression out of a judge having a bad day.
- `FinishEvalRun` aggregates from the recorded rows rather than from anything
  carried through the workflow, then `NotifyEvalRun` wakes the requester through
  the same `wakeAgent` path a delegated result uses. A run outlasts the turn
  that started it by minutes, so the requester is told rather than made to wait.

`RunEvalCase` runs the case in an isolated conversation of the agent
(`EvalConversation(runID, case)`), which reuses `resolveWorkspace` — a git
worktree for a repo-backed agent, a fresh directory for a managed one — so the
workspace is shaped like the real one without touching it. The version's own
persona is written into that workspace. Secrets, `group_add` and the agent's
identity environment are **withheld** unless the set sets `full_grants`: an eval
that can push, deploy, post or delegate is not a test. Tools and skills are
mounted either way, because an agent stripped of its capabilities is not the
agent anyone wants measured.

## Activities

`internal/temporal/activity`. Registered as one struct via
`NewActivities(cfg, stores, reg, discord, tc)` — dependencies are injected
through the constructor because `RegisterActivity` on a struct registers *every*
exported method, and a stray `SetDiscord` setter panics worker startup.

**Failing fast is explicit.** `newNonRetryable` wraps the errors no retry can
fix — a deleted agent, a missing credential, an origin this binary does not
implement — as a `temporal.ApplicationError` with `NonRetryable` set. It must be
an ApplicationError rather than a marked ordinary error: Temporal matches
`RetryPolicy.NonRetryableErrorTypes` against the failure's *type*, and an
ordinary error's type is its Go type name (`wrapErrors` for anything built by
`fmt.Errorf`), so a sentinel in the message never matches. The flag is honoured
directly, which also covers callers whose policy names no types — `deliver()`
does not.

### `RunClaudeTurn`

1. Resolve the agent definition and the conversation's workspace
   ([agent-runtime.md](agent-runtime.md)).
2. Link the turn's staged uploads into that workspace's `inbox/`
   ([adapters.md](adapters.md)) — the prompt already named those paths, and this
   is what makes them true. A hard link, so a 25MB upload is not copied, and the
   staged entry stays behind so a retried activity finds its files.
3. Build argv and `docker run` the agent container.
4. Decode stream-json line by line into `live_logs`.
5. **Heartbeat every second.**
6. On cancellation: `SIGINT`, wait `container.stop_grace`, then `SIGKILL`.
7. Write the result, cost and status to the `turns` row.

Heartbeating is not optional. An activity only learns it was cancelled through
the response to a heartbeat, so `/stop` and `/steer` are exactly as responsive
as the heartbeat interval — and the SDK throttles outbound heartbeats to ~80% of
`HeartbeatTimeout`, which is why the worker also sets
`MaxHeartbeatThrottleInterval: 1s` alongside `HeartbeatTimeout: 10s`.

**Session-loss recovery.** Transcripts are cleaned up after
`cleanupPeriodDays`, and a volume can be lost — both behind the system's back,
which is why nothing may cache whether a session exists. Any turn that opens a
session rather than continuing one builds a recap from the last N turns of the
*same conversation* (`store.RecentTurnsIn`) and starts with it. That covers a
conversation whose transcript has gone as well as one that never had a session,
because the recap is empty when there is no history to recap. Recovery costs no
turn of its own: the loss is seen before the run when the transcript is missing,
and inside the run when the CLI refuses the resume.

This is the only place the turn window is fed back into a prompt — a live session
already holds the context, so replaying it every turn would just be duplication.
A turn that continues a session therefore carries no recap, including the second
attempt after a refused create.

### `LoadWorkflow`, `RunWorkflowStep`

Housekeeping for `RunWorkflow`. `RunWorkflowStep` reuses the same container
execution as `RunClaudeTurn` — streaming, heartbeat, graceful stop — but its
config comes from the step, not an agent, and there is no session to resume:
each step is a one-shot run that receives the earlier steps' outputs as input.
The workflow's own `state.db` (under `workspace/workflows/<id>/`) records each
step as a turn, so a run is inspectable. Steps run under `bypassPermissions` by
default — a pipeline has no human to answer a permission prompt — and get the
**global** secrets injected, since a workflow has no agent scope of its own.

### `DeliverResponse`

Switches on `turns.origin`:

- `discord` — send to the channel, chunked at 2000 characters.
- `http_callback` — signed POST, retried by Temporal's retry policy, host
  re-validated before the request.
- `http_poll` — nothing to do; the row is already the answer.
- `agent` — queue the result as a new request for the agent that delegated the
  work, in its conversation, so it reports to the human in its own words. This is
  the only case that *creates* work rather than just delivering it, and the only
  one that crosses a workflow boundary: it writes the turn row and calls
  `SignalWithStartWorkflow` in the same activity, under one idempotency key
  (`notify:<agent>:<turn>`), because this activity is retried and a second signal
  would wake the delegator twice for one result. The delegator's own reply address
  is read from the last human-facing turn of that conversation rather than carried
  along, since a conversation has exactly one audience. Note this is the opposite
  choice from `origin.audience`, and deliberately so: a *result* only ever needs
  the next hop up, which the delegator's own conversation already holds, while
  speaking mid-turn needs the root of the chain and so must carry it down
  ([delegation.md](delegation.md#the-audience)).

The `agent` case is drawn end to end in [delegation.md](delegation.md), with what
is written where at each step.

That last case is what makes "I'll tell you when it's done" keepable: the return
address is on the delegated turn's row, so the result comes back even after the
delegating process, its connection and the worker have all died. Nothing in the
agent's own behaviour is required — an agent that simply ends its turn still
reports.

### `AbandonTurns`, `RemoveConversationWorkspace`

Housekeeping the workflow triggers explicitly: close out turn rows dropped by
`/stop`, and tear down a conversation's worktree when it is finished (never
automatically — a quiet thread usually still has uncommitted work in it).

## The `contract` package

`internal/temporal/contract` is a leaf holding what both sides must agree on:
workflow IDs, signal and query names, workflow type names as strings, and
activity input types. Without it, `activity` → `workflow` (to signal the agent)
and `workflow` → `activity` (to execute it) would be an import cycle.

Renaming anything in it breaks in-flight workflows, which is precisely why it
lives in a package neither side owns.
