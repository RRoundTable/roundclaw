# Schedules

Having an agent do something on a recurring basis.

## Behaviours

### A schedule is recurring work for one agent

**Given** a schedule on an agent
**When** its time comes
**Then** the agent does the work in its main conversation, so today's run can
build on yesterday's.

A daily report that opened a fresh conversation each morning could never refer to
what it said the day before.

### Scheduled work waits its turn like anything else

**Given** an agent already busy
**When** a schedule fires
**Then** the work joins the same queue as human requests, under the same ordering
and the same limits.

### Editing a schedule takes effect on the next run

**Given** a schedule with work already scheduled
**When** somebody changes what it says or when it runs
**Then** the next run uses the new definition, with nothing to recreate.

### A schedule can be paused and resumed

**Given** a schedule that should stop for now
**When** it is paused
**Then** it stops firing, and resuming starts it again without redefining it.

### A run with nothing to say can stay quiet

**Given** a schedule whose result says there is nothing to report
**When** it finishes
**Then** the result can be recorded without being announced.

People stop reading a channel that says "nothing to report" every morning.

### An unknown time zone is refused when set, not at run time

**Given** a schedule given a time zone that does not exist
**When** it is saved
**Then** it is refused immediately.

Accepting it would mean the schedule silently runs on a different clock, hours
from when anybody expected.

## Invariants

- A schedule always belongs to an agent. There is no agent-less schedule; that is
  a [workflow](workflows.md).
- Scheduled work and human requests share one queue, one ordering and one set of
  limits.
