---
name: roundclaw-routines
description: Use FIRST, before doing any task yourself — check whether the fleet already has a routine for it, and if one fits, hand the work to it instead of doing it. Covers finding a routine, reading its record by whose standard it measures, handing work off with what you need back, answering from the result when you are woken, checking a trace only when a run fails, and improving a routine you own. Skip only for conversation that is not a task.
---

# Handing work to routines

A routine is work the fleet already knows how to do: a frozen contract (what it
is for, what it takes, what counts as done, what it may do outside) and a graph
of steps that carries it out. Handing work to one costs you only its result, so
spend your own turns on what is new.

You reach routines over the roundclaw API at `$ROUNDCLAW_URL`, with
`$ROUNDCLAW_SELF_TOKEN` — the credential roundclaw puts in your container for
each turn. It names you, so the routines you start are recorded as yours.

## 1. Look before you do it yourself

```bash
curl -s -H "Authorization: Bearer $ROUNDCLAW_SELF_TOKEN" $ROUNDCLAW_URL/v1/routines
```

Each entry gives `intent`, `inputs`, `effects`, whether it is `runnable` (and
why not), and `current` — its record for the version that would run. Read the
record by standard, and never as one number:

- `own_pass_rate` is measured against the routine's own conditions, which its
  author wrote. It is the weakest signal.
- `caller_met_rate` over `caller_stated` is how often it gave callers what they
  said they needed. Prefer this.
- `redone` counts results callers had to redo.
- `runs` is the sample. Three runs are not a trend.
- `cost_per_passed_usd` is what one success really costs, retries included.

If nothing fits, do the work yourself. Work that keeps succeeding the same way
is what the next routine is drawn from.

## 2. Hand off, say what you need back, carry on

```bash
curl -s -X POST -H "Authorization: Bearer $ROUNDCLAW_SELF_TOKEN" \
  $ROUNDCLAW_URL/v1/routines/<id>/runs -d '{
    "inputs": {"repo": "…"},
    "expect": {"must_contain": ["branch:"]},
    "notify": {"agent": "'$ROUNDCLAW_AGENT_ID'", "conversation": "'$ROUNDCLAW_CONVERSATION_ID'"}
  }'
```

- `expect` is **your** acceptance check, applied by rule to the result before
  you are woken. State it whenever you can: it is the fairest record a routine
  gets.
- With `notify` you are woken once, when the run ends, with a bounded result.
  If the result was long, the full text is in your work area at the path the
  message names — open it only if you need it.
- **Do not poll.** Status checks put the run's progress into your context,
  which is exactly what handing off was meant to avoid.
- A routine that acts on the outside world runs only for its owner or a person.
- Map the request onto the routine's `inputs` exactly as named. If the request
  does not supply one, do the work yourself rather than guess it.

Then end your turn: say which routine you handed the work to, and the run id.

## When you are woken

The message starts `[routine passed]` or `[routine failed]`, says whether
**your** conditions were met, and gives the result. Answer the original request
from it, as if you had done the work — the person asked you, not the routine.
If your conditions were not met, either redo it with `instead_of` or do the
work yourself; never present a result that failed your own check as done.

## 3. When it fails

The wake-up names the failing step and why. Only then read the trace:

```bash
curl -s -H "Authorization: Bearer $ROUNDCLAW_SELF_TOKEN" $ROUNDCLAW_URL/v1/routine-runs/<run>
```

Read the failing step's `verdicts` — which rule did not hold and the detail —
not its output. If you redo the work, say so, so the record counts it:

```bash
… /v1/routines/<id>/runs -d '{"inputs": {…}, "instead_of": <run>}'
```

To stop a run, POST `/v1/routine-runs/<run>/stop`. Stopping is always explicit.

## 4. Routines you own

A routine you submit with your own credential is yours: you may change how it
works (`PUT /v1/routines/<id>`), roll it back, or archive it. You cannot change
its contract — a different promise is a new routine, starting with no record.
A submission is compiled on admission; a refusal lists every problem by step.

When a routine you own keeps failing at one step:

1. Read that step's verdicts across the failures.
2. Change that step only. Sharpen its instruction or its check before rewiring
   the graph.
3. Choose how much the step fixes by how well its work is understood: a process
   step where the method is settled, a fixed task where the wording is, an open
   goal with conditions where only the outcome is known.
