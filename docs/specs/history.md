# History

Looking at what was asked of an agent and what happened.

## Behaviours

### What was asked and what came back are both kept

**Given** a piece of finished work
**When** somebody looks it up
**Then** they see the request, the result, whether it succeeded, and what it
cost.

### History can be narrowed to what went wrong

**Given** an agent that has been misbehaving
**When** somebody looks at its history
**Then** they can ask for a period, a number of entries, or only the failures.

Looking for failures is the common case, and reading everything to find three
errors is not a way to work.

### Long entries are shortened unless asked for in full

**Given** entries with long requests or results
**When** they are listed
**Then** they are shortened by default, and can be requested whole.

### Reading history never depends on an agent

**Given** an agent that is wedged
**When** somebody reads its history
**Then** they get it.

### Old detail is cleaned up on a stated schedule

**Given** work from long ago
**When** the retention period has passed
**Then** its detail is removed on a schedule that is configured, not arbitrary.

## Invariants

- History is readable without any model being involved.
- Nothing is removed before its retention period.
