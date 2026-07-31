# Conversations

Keeping separate lines of work with one agent from bleeding into each other.

A conversation — not an agent — is the unit that owns memory, a queue and a place
to work. One agent can hold many at once.

## Behaviours

### Two conversations do not see each other

**Given** an agent with two conversations in progress
**When** work runs in both
**Then** neither sees the other's history, neither waits on the other's queue,
and neither can read or overwrite the other's files.

### A chat thread is its own conversation

**Given** a thread started under a channel bound to an agent
**When** somebody works in that thread
**Then** it behaves as its own conversation, while the channel itself keeps using
the agent's main one.

### A named conversation can be opened deliberately

**Given** a program that wants several lines of work with one agent at once
**When** it names a conversation for each
**Then** they run in parallel and share nothing.

### Work areas are not deleted on their own

**Given** a conversation that has gone quiet
**When** time passes
**Then** its work area is kept until somebody removes it explicitly.

A quiet conversation usually still has unsaved work in it. Reclaiming it
automatically is how that work disappears.

### A conversation that lost its memory is caught up, not restarted

**Given** a conversation whose stored session is gone
**When** the next request arrives
**Then** the agent is given a summary of that conversation's recent work and
continues, rather than starting as if nothing had happened.

### A fresh conversation starts empty

**Given** an agent whose work area is managed rather than a checkout
**When** a new conversation opens
**Then** it starts with the agent's instructions and nothing else.

Naming a new conversation for work that expects existing files hands the agent an
empty directory. This is stated because it surprises people.

## Invariants

- One conversation, one memory, one queue, one work area.
- The default conversation cannot be confused with a thread.
