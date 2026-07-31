# Delegation

Letting one agent hand work to another and report the result as its own.

## Behaviours

### A delegated result comes back on its own

**Given** an agent that handed work to another agent and then finished its turn
**When** the delegate finishes, minutes later
**Then** the result arrives back at the delegating agent as new work, and it
reports to the person who asked — in its own words.

Nothing is required of the delegating agent's behaviour. An agent that simply
ends its turn still gets the answer. This is what makes "I'll tell you when it's
done" a promise the system keeps rather than one the agent has to remember.

### The result survives everything dying

**Given** delegated work in progress
**When** the delegating agent's process, the connection, and the machine running
the work have all been restarted
**Then** the result still reaches the delegator.

### A result is never delivered twice

**Given** delivery that is retried after a failure
**When** it eventually succeeds
**Then** the delegator is woken exactly once for that result.

### A delegate can speak to the person mid-work

**Given** a chain of agents, one handing work to the next
**When** an agent partway down wants to say something to the human who started it
**Then** it can, and the message reaches that person rather than the agent above
it.

The return address for a *result* only needs the next hop up. Speaking mid-work
needs the person at the top of the chain, so that is carried down.

### An agent cannot delegate to itself in place

**Given** an agent running in a conversation
**When** it tries to hand work to itself in that same conversation
**Then** it is refused.

### A caller claiming to be an agent is bounded, not trusted

**Given** a caller that states which agent it is when handing off work
**When** the claim is false
**Then** the named agent must still exist, and the worst outcome is a wasted turn
on that agent.

## Invariants

- The return address lives with the work, not with the caller.
- A conversation has exactly one audience, so a result knows where to go without
  being told again.
