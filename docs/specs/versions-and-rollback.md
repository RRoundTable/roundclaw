# Versions and rollback

Knowing what an agent, a tool or a skill was when it did something, and being able
to put it back.

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

### Tools and skills are versioned like agents

**Given** a registered tool or skill that changes
**When** the change is saved
**Then** a version is recorded automatically, and it can be put back the same way
an agent can.

### A tool version records what the tool was, not where it was

**Given** a tool version
**When** somebody reads it
**Then** it names the identity of the thing the tool pointed at — its contents,
its package version, its image, whatever that tool declares itself by — and not
only the path or address it was found at.

A version recording a pointer alone describes nothing. The thing at the end of the
pointer changes without leaving a version behind, which is the case a version
existed to catch.

### An agent's version records the tools and skills it was holding

**Given** a recorded version
**When** somebody reads it
**Then** it names which version of each tool and skill was in play.

A version that omits them describes a configuration that never existed, for the
same reason a definition without its instructions does. It also lets two
evaluations "of the same configuration" measure different things, and that
comparison is what versions exist to support.

### A rollback says whether it actually put the tool back

**Given** a tool rolled back
**When** the earlier version's content is no longer what it was
**Then** the rollback reports the configuration restored and the content not,
rather than reporting success.

### Rolling an agent back changes its tools for nobody else

**Given** an agent rolled back
**When** it next works
**Then** it uses the tool and skill versions its old version named, while every
other agent holding those tools is unaffected.

## Invariants

- Nothing changes an agent, a tool or a skill without leaving a version behind.
- A version is always internally consistent: its definition, its instructions, and
  the tool and skill versions it named were true at the same moment.
- A version names what a tool was, not only where it was found.
