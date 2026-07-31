# Fleet management

Adding, changing and removing agents while the system is running.

## Behaviours

### An agent appears without a restart

**Given** a running system
**When** somebody registers a new agent
**Then** it can be given work immediately, with no restart and no configuration
file edited.

### The same change can be made three ways

**Given** a change to an agent
**When** it is made through the API, through the command line, or by asking the
administrating agent in plain language
**Then** the outcome is the same. There is one meaning of "change an agent", not
three.

### A channel belongs to one agent

**Given** a channel already bound to an agent
**When** somebody binds it to a second one
**Then** they are refused, and told which agent holds it.

Otherwise a message in that channel has no unambiguous recipient.

### Deleting an agent stops its work but keeps its records

**Given** an agent with work in progress
**When** it is deleted
**Then** its running work is stopped first, and its past work and files remain.

### A deleted agent's requests fail immediately

**Given** work aimed at an agent that no longer exists
**When** it is attempted
**Then** it fails at once with the reason rather than being retried.

### The startup configuration is a seed, not a source of truth

**Given** a system that has run before
**When** it starts again
**Then** the agent list in the configuration file is ignored; the registry is
what is real.

The file seeds an empty registry once. Anyone expecting it to keep the fleet in
sync will be surprised, which is why it is stated.

## Invariants

- Registering, changing and removing an agent never requires a restart.
- One channel, one agent.
