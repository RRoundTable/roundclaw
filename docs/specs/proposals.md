# Proposals

A change to the fleet, written down but not made, waiting for a person to decide.

This is the only place today where the system asks a human to judge something.
That narrowness is the subject of the current roadmap item, so this document
states what exists precisely — including what it does *not* do.

## Why it exists

The thing writing proposals is an agent reviewing the fleet on a schedule. An
agent that edits the fleet unattended is one prompt injection away from editing
it badly. Writing the change down turns an unattended edit into an attended one,
and leaves a record: who approved what, on what evidence, and what it produced.

## Behaviours

### A proposal states what, why, and on what evidence

**Given** somebody or something wants to change an agent
**When** it files a proposal
**Then** the proposal names what it acts on, carries the change itself, and
carries a reason — and is refused if the reason is missing.

A change with no stated reason cannot be judged, only rubber-stamped, and being
judged is the entire point.

### Nothing happens until a person decides

**Given** a filed proposal
**When** nobody has decided it
**Then** the fleet is unchanged in every respect.

### Approving applies the change and records what it produced

**Given** a pending proposal
**When** a person approves it
**Then** the change is applied, a new version of the affected agent is created,
and the decision is recorded with who made it and which version resulted.

### Rejecting changes nothing and says so

**Given** a pending proposal
**When** a person rejects it
**Then** nothing is applied, and the rejection is recorded.

### An approved change that fails to apply is not left pending

**Given** a proposal a person approved
**When** applying it fails
**Then** it is recorded as failed with the reason, not returned to pending.

A person already said yes. Leaving it pending invites a second person to say yes
to the same broken change.

### The same proposal cannot be decided twice

**Given** two people looking at the same pending proposal
**When** both decide it at the same moment
**Then** one decision takes effect and the other is told it was already decided,
by whom, and how. Nothing is applied twice.

### The decision is made on the change, not only on the reason

**Given** a proposal presented to a person
**When** they look at it
**Then** they see the change itself, not just the proposer's summary of it.

The reason is written by the thing asking for approval. A person approving on the
reason alone is approving the proposer's self-assessment.

### Deciding is restricted to the people allowed to decide

**Given** a proposal shown in a channel everyone can see
**When** somebody not permitted to decide tries to
**Then** they are refused.

Visibility is not permission. The check cannot live only on the command that
displayed the proposal.

### An approval can be undone

**Given** an applied proposal
**When** it turns out to be wrong
**Then** the agent can be returned to the version it was on before, because the
proposal records which version applying it produced.

### A proposal can add the cases that measure an agent

**Given** an agent that should be judged against new cases
**When** those cases are proposed
**Then** they take effect only on approval, like any other change.

The cases are what "better" means. An agent that could quietly rewrite its own
marking scheme would be grading its own homework.

## Invariants

- A pending proposal has no effect on anything.
- Every decision names a person.
- Every applied proposal names the version it produced.
- Approving a change made this way is recorded, atomic, and reversible.

## Explicitly not guaranteed

**The queue is a convention, not a wall.** Nothing prevents something holding
full credentials from changing an agent directly and skipping proposals
entirely. What the system guarantees is that a change made *through* proposals is
recorded, atomic and reversible — not that no other path exists.

Stated here because it is the difference between a control and a habit, and
because anyone generalising this mechanism needs to know which one they are
generalising.

## Known limits

These are the reasons the roadmap item exists, not defects to be worked around:

- A proposal has exactly two answers, apply and don't. There is no way to ask a
  person to choose among several candidates.
- Deciding a proposal always *applies* something. There is no way to record a
  human judgement that changes nothing and exists only as a label.
- A proposal is about the fleet's configuration. There is no way to ask a person
  about an agent's *output*.
