# Turn control

Seeing what an agent is doing, stopping it, and redirecting it without losing
what it already knows.

## Behaviours

### Status answers when the agent cannot

**Given** an agent that is stuck, looping, or otherwise not responding
**When** somebody asks what it is doing
**Then** they are told its state, how much is waiting, and how much it has done —
promptly, and without any of it depending on the agent being able to answer.

An agent being unresponsive is the situation status exists for. It is useless if
it needs the agent to work.

### Stopping abandons the queue

**Given** an agent working, with more requests waiting
**When** somebody stops it
**Then** the running work is cancelled and the waiting requests are dropped, and
nothing is left reported as still running.

### Steering redirects without starting over

**Given** an agent working on the wrong thing
**When** somebody steers it with a new instruction
**Then** the current work is cancelled, the new instruction is taken up
immediately ahead of anything queued, and the agent still remembers the
conversation so far.

Steering differs from stop-then-ask exactly here: the context survives.

### Interruption is never inferred

**Given** a request whose content reads like "stop" or "no, do this instead"
**When** it arrives as an ordinary request
**Then** it is queued like any other request and interrupts nothing.

Only an explicit stop or steer interrupts. A misread request costs a wasted turn;
a misread interrupt costs work that was running.

### Work in progress is visible while it runs

**Given** work that takes minutes
**When** somebody watches it
**Then** they see it progressing as it happens rather than only at the end.

### A stopped agent shuts down its work gracefully

**Given** work being cancelled
**When** the shutdown begins
**Then** the work is first asked to stop and given a grace period, and only
forced afterwards.

## Invariants

- Status and stop never depend on a model answering.
- A cancelled piece of work is always closed out. Nothing stays reported as
  running after it has been dropped, or the agent looks busy forever.
- Responsiveness to stop and steer is bounded, not best-effort.
