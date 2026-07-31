# Versions and rollback

Knowing what an agent was when it did something, and being able to put it back.

## Behaviours

### Every change to an agent is recorded as a version

**Given** an agent whose definition or instructions change
**When** the change is saved
**Then** a new version is recorded automatically, without anybody asking for one.

### A version is a definition and its instructions together

**Given** a recorded version
**When** somebody reads it
**Then** they get both what the agent was configured to be and what it was told
to do.

An agent whose instructions were rewritten is a different agent, even if nothing
else moved. Recording one without the other describes a configuration that never
existed.

### Two changes made together are one version

**Given** a definition and instructions changed in the same action
**When** they are saved
**Then** one version results, not two.

### An agent can be put back

**Given** a change that turned out badly
**When** somebody rolls the agent back to an earlier version
**Then** it returns to that configuration, definition and instructions together.

### Rolling back is itself a change

**Given** a rollback
**When** it completes
**Then** it is recorded like any other change, so the history says what happened
rather than appearing to rewind.

## Invariants

- Nothing changes an agent without leaving a version behind.
- A version is always internally consistent: its definition and its instructions
  were true at the same moment.
