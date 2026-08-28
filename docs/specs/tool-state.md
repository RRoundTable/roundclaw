# Tool state

Knowing which of an agent's tools work right now, and which are gone.

## Why it exists

A tool is the agent's surrounding dependencies — a package, an API server, a
database. Some of them hold state that does not survive an interruption. An agent
that assumes its database is where it left it spends a turn finding out otherwise,
and the turn is the expensive part.

## Behaviours

### A tool says whether its state survives

**Given** a registered tool
**When** it is defined
**Then** it states whether its state can be restored after an interruption, and
how — or states that it cannot.

A tool that says nothing about restoration is treated as unrestorable. The
dangerous default is the optimistic one.

### A session begins by putting tools back

**Given** an agent starting work after an interruption
**When** its tools are resolved
**Then** every tool that declares itself restorable is restored before the agent's
first piece of work.

### The agent is told what it got, before it tries to use it

**Given** tools resolved at the start of a session
**When** the agent begins work
**Then** for each tool it holds, it knows whether the tool is usable, was brought
back, or is unavailable.

Discovering a dead database by failing a query mid-turn wastes the turn.

### A tool that could not be brought back says so, with the reason

**Given** a tool whose state could not be restored
**When** the agent starts work
**Then** the tool is reported unavailable with the reason, and is never reported
fine.

### Restoring never invents state

**Given** a tool whose state was restored
**When** the agent uses it
**Then** it holds the state that was actually recovered.

An empty database presented as restored is worse than one reported unavailable.
The second is known before the agent acts; the first is discovered after.

### A tool that no longer matches its recorded version says so

**Given** a tool whose content changed without a change being recorded
**When** the agent starts work
**Then** it is reported as having drifted from the version it is supposed to be.

A configuration that cannot be trusted to be what it claims cannot be measured
against another one.

### Unavailable is not the same as ungranted

**Given** a tool that is granted but not working
**When** the agent looks at what it holds
**Then** it can tell that apart from not holding the tool at all. The grant
stands; the tool is down.

### Work does not stop because a tool is down

**Given** an agent holding an unavailable tool
**When** it has work to do
**Then** the work runs, with the agent knowing the tool is unusable, rather than
the request being refused.

One dead dependency refusing every request is a larger failure than the dependency
being dead.

## Invariants

- An agent is never told a capability works when it does not.
- Every tool an agent holds has a stated condition before the agent's first piece
  of work, rather than after its first failure.
