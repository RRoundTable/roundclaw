# Workflows

A multi-step job that belongs to no agent.

## Behaviours

### Steps run in order, each building on the last

**Given** a workflow of several steps
**When** it runs
**Then** each step runs in turn and receives what the earlier steps produced.

### A crash resumes at the step it was on

**Given** a workflow partway through
**When** the machine running it dies
**Then** it continues from the step it had reached rather than from the
beginning.

This is the whole reason a pipeline is modelled as steps rather than as one long
piece of work.

### A workflow carries no conversation

**Given** a workflow that ran yesterday
**When** it runs again today
**Then** it remembers nothing of yesterday. Each step is a one-shot run.

### Editing a workflow takes effect on the next run

**Given** a workflow whose steps change
**When** it next runs
**Then** it uses the new steps.

### Runs do not collide

**Given** a workflow started twice
**When** both are in progress
**Then** neither interferes with the other.

### A run is inspectable afterwards

**Given** a workflow that has finished
**When** somebody looks at it
**Then** they can see what each step produced.

### The final result is reported where the workflow says

**Given** a workflow that names somewhere to report
**When** it finishes
**Then** the final result is delivered there. A workflow that names nowhere
records the run and delivers nothing, which suits a job whose steps are their own
effect.

## Invariants

- A workflow has no memory, no queue and no channel binding of its own.
- A step never waits for a person. A pipeline has nobody to answer a prompt.
