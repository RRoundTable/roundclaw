# Grants

Widening what an agent is allowed to do: tools, skills and secrets.

## Behaviours

### A grant changes what an agent can do, and does nothing by itself

**Given** a registered tool, skill or secret
**When** it exists but is granted to nobody
**Then** nothing happens. It runs no work and has no schedule of its own.

### A granted capability is available on the agent's next piece of work

**Given** an agent granted a new tool or skill
**When** it next works
**Then** it can use it, with no restart.

### A secret reaches the agent without being readable back

**Given** a secret granted to an agent
**When** somebody lists what it holds
**Then** they see that it holds it, not what it is.

### An agent's own secrets override the shared ones

**Given** a secret defined both globally and for one agent
**When** that agent works
**Then** its own value is the one used.

### Removing a grant takes effect on the next piece of work

**Given** an agent that loses a grant
**When** it next works
**Then** it no longer has it.

## Invariants

- Tools, skills and secrets are modifiers. None of them is work.
- An agent never receives a credential it was not granted.
