# Self-improvement

An agent changing its own tools, skills and instructions, and finding out whether
the change helped.

## Why it exists

The pieces of a self-improvement loop are already here, pointed outward. A curator
agent reads history to find where agents fail, reads versions to see what changed,
runs evaluations, compares two runs to decide whether a change helped, and files a
proposal a person approves.

This is that loop turned inward. The subject changes from "another agent" to
"me"; what does *not* change is that a person still decides whether the result
was good.

It removes nothing. The proposal queue was never a wall — an agent holding a
full-scope token could already rewrite its own instructions and register its own
tools. What was missing on that path is what this adds: a version for every tool
and skill, so a change an agent made to itself can be seen and undone.

**The safety net is the history, not a gate.** Nothing measures a self-made
change before it takes effect. What the system guarantees is that every such
change is recorded, attributable, and reversible — somebody reading the history
decides whether it helped. An earlier design held changes behind an automatic
measurement and reverted regressions unasked; that was removed deliberately, and
[Evaluation](evaluation.md) remains available to anyone who wants to measure a
version on purpose.

## Behaviours

### An agent changes itself the way a person would

**Given** an agent that has found a better way to do its work
**When** it creates, changes or removes one of its own tools, skills or
instructions
**Then** the change takes effect on its next piece of work, with nobody in the
loop and no restart.

### A self-made change is recorded like any other

**Given** a change an agent made to itself
**When** it is saved
**Then** a version is recorded naming the agent as its author.

It differs from a change a person made in authorship only, never in kind. A
history that cannot say who changed what is not one anybody can act on.

### An agent cannot change what it is measured by

**Given** an agent editing itself
**When** it tries to change its own evaluation cases
**Then** it is refused.

Nothing gates a self-made change on those cases any more, but an agent that
could rewrite the standard it is judged against would make every later
measurement worthless too.

### Deleting removes something from use, not from the record

**Given** a tool, skill or set of instructions the agent deleted
**When** it turns out to have been needed
**Then** it can be brought back from history.

### Changing a shared tool changes it for everybody holding it

**Given** a tool granted to several agents
**When** one of them changes it
**Then** the change is recorded against the tool, and every holder gets it.

### Removing a shared tool from itself ends only its own grant

**Given** a tool granted to more than one agent
**When** one agent removes it from itself
**Then** the tool, and every other agent's use of it, are untouched.

Removing something from oneself and destroying it for everybody are different
intentions, and only one of them is what an agent tidying up meant.

### A self-made change and an approved proposal do not overwrite each other silently

**Given** an agent improving itself while a proposal against it is approved
**When** both apply
**Then** both stand in the history in the order they happened, and neither
disappears.

An agent whose change vanished with no record would simply make it again.

### Credentials stay unreadable

**Given** an agent inspecting a tool of its own that carries a credential
**When** it looks
**Then** it sees that the credential is there, not what it is.

Applying a change never requires reading a credential back, so nothing here widens
what an agent can see.

## Invariants

- Every self-made change leaves a version, and every version can be returned to.
- A self-made change is always attributable: authorship is established by the
  credential, never asserted by the caller.

## Explicitly not guaranteed

**This adds no boundary and removes none.** An agent holding a full-scope token can
already register a tool naming any host path, and that together with an image and
supplementary groups is effective host root — the same trade the admin and curator
agents already make. What this capability guarantees is that a change made this way
is recorded, measured and reversible, not that it is bounded.

Bounding what an agent may change about itself needs a token scope narrower than
roundclaw issues today. That is a separate decision, and stating it here is the
difference between a control and a habit.

## Known limits

- A change to a shared tool is measured against the changing agent's cases only.
  Other holders get a change that nothing measured for them.
