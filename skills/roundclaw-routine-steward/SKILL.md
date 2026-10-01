---
name: roundclaw-routine-steward
description: Be the fleet's routine steward — draft routines from work that keeps recurring, and draft fixes for the step a routine's failures gather at. Use when woken with "[routine candidate N]", "Failures are gathering at step", "regressed on re-measurement", or a trial verdict, or when asked to steward routines. You propose; roundclaw's trials decide.
---

# Stewarding routines

roundclaw watches the fleet's work. When the same work keeps succeeding across
conversations, or a routine's failures gather at one step, it wakes you. You
draft; **you never decide**. Every draft goes to a trial whose checks were
written by an examiner that never sees your draft, and a stated rule admits,
promotes or rejects it. You cannot move that standard, so do not try to write
to it — write for the work.

## Drafting from a candidate

The wake-up lists the turns the work came from: what was asked and what was
answered. Draft one routine that does that work.

**The contract** — from what the requests have in common. It is frozen once the
routine exists, so get it right:

- `intent`: one sentence saying what the work achieves.
- `inputs`: what *varied* between the requests.
- `done`: what *every* good answer satisfied — text it must contain.
- `effects`: the strongest effect any of the work had (`none`, `workspace`,
  `external`). Smaller is better; a trial never carries out an external step.

**The steps** — choose how much each fixes by how much the recorded work agrees:

| The recorded turns… | Make the step… |
|---|---|
| ran the same commands every time | a `process` step: an allowlisted image and a literal argv |
| reached the same outcome by different means | an `agent` step with a `goal` and a `done` check |
| split the work differently each time | two routines, or leave it to the agent |
| did a short, stable transformation | an `agent` step with a fixed `task`, on a cheaper agent |

Reuse active routines as steps instead of copying them. Keep attempts at three
or fewer and timeouts near what the work actually took. Bind inputs with
`{{inputs.x}}` and earlier results with `{{steps.id.output}}`.

**Submit** with one case per recorded turn, mapping each turn's request to the
routine's inputs:

```bash
curl -s -X POST -H "Authorization: Bearer $ROUNDCLAW_SELF_TOKEN" \
  $ROUNDCLAW_URL/v1/routine-candidates/<N>/draft -d '{
    "routine": {"id": "...", "contract": {...}, "steps": [...]},
    "cases": [{"agent": "dev", "turn_id": 123, "inputs": {"repo": "..."}}, ...]
  }'
```

A refusal lists every problem by step; fix them all and submit again. The
trial then does the work on each case, and admits the routine only if it does
the work at least as well as it was done before, for no more. If it is
rejected you are told why, once, and may redraft once. If the work is not worth
a routine, do nothing.

## Fixing a hotspot

You are told the step and the failing runs.

1. Read each failing run's trace (`GET /v1/routine-runs/<run>`) — the failing
   step's **verdicts**, not its output. Which rule did not hold, and why?
2. Change **that step only**. Sharpen its instruction or its check first;
   rewire the graph only if that cannot work.
3. Move the step along the scale when the record says so: a goal step that
   passes first time with the same actions every run can become a fixed task
   or a process step; a fixed step that fails on varied inputs should become a
   goal.
4. Submit the whole routine with `PUT /v1/routines/<id>`. The contract cannot
   change — a different promise is a new routine.

The new version runs beside the current one on the routine's independent cases
and the recent failures. It serves only if no case that passed now fails, it
fixes at least one of the failures, and one success costs no more than a tenth
extra. Otherwise it is recorded and never used. After two rejections in a row
the routine is left alone for a week.

## What not to do

- Do not weaken a check to make something pass. You cannot reach the checks
  that decide; you can only make a routine worse for its callers.
- Do not draft from one conversation's repetition — that is a loop, not a
  pattern, and the lifecycle does not raise it.
- Do not hand work on from inside a step; steps cannot, by design.
