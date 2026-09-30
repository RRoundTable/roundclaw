# Evaluation

Measuring one agent, and deciding whether a change to it helped.

## Behaviours

### A run measures a stated configuration, not "the agent right now"

**Given** an evaluation of an agent
**When** it runs
**Then** the result says which configuration produced it, so two results can be
compared as two known things rather than as "before" and "whatever it is now".

### Measuring never touches what the agent works on

**Given** an evaluation in progress
**When** the agent's own work area is examined
**Then** it is unchanged. Each case runs against a copy shaped like the real
thing.

If isolation cannot be arranged, the case is refused rather than run. This is not
configurable.

### A failing case is not a failing run

**Given** a run where one case errored
**When** it finishes
**Then** the other cases' results still stand, and the failed case is recorded
with its reason.

An agent that broke on one question answered the other nine, and those answers
are what a comparison needs. A case with no record at all would read as a
question never asked.

### A case that could not be marked is not scored zero

**Given** a case answered but not markable
**When** the run finishes
**Then** it is recorded as unmarked rather than as a failure.

Scoring it zero invents a regression out of a marking failure.

### Exact rules are not subject to opinion

**Given** a case with a requirement stated exactly
**When** the answer breaks it
**Then** it fails, without anything being asked to judge it.

### Whether a change helped is arithmetic

**Given** two runs of the same cases on two configurations
**When** they are compared
**Then** the verdict follows one stated rule: a case that passed before and fails
now is a regression; any regression makes the change worse, or mixed if something
also improved; improvements with no regressions make it better; neither leaves it
unchanged, whatever the averages did.

The verdict is computed, not formed by reading the outputs. Reading outputs and
forming an impression is how a regression gets talked away.

### Two runs that do not measure the same thing say so

**Given** a comparison between runs of different cases
**When** it is produced
**Then** it states that the two are not comparable rather than reporting a
confident verdict.

### An evaluation cannot act as the agent

**Given** a case running
**When** the agent under test tries to use its real credentials
**Then** it cannot, unless the evaluation was explicitly set up to allow it.

An evaluation that can deploy, publish or hand out work is not a test.

### The requester is told when it finishes

**Given** somebody who asked for a run
**When** it finishes, minutes later
**Then** they are told, rather than having been made to wait.

### A routine can be measured like an agent

**Given** a [routine](routines.md) and cases its owner did not write
**When** it is measured
**Then** it runs each case in a work area of its own, on a stated version, and
two versions are compared by the same arithmetic as agents.

### A hand-off in a measurement is recorded, not carried out

**Given** an agent under measurement that hands work to another agent or a
routine
**When** it does
**Then** the hand-off is recorded against the case — what it chose and what it
passed — and not carried out, so a case can check the choice. A measurement may
instead allow routines that cannot act on the outside world to run for real, in
their own work areas; nothing that can act outside ever does.

Before this, a hand-off in a case simply failed, so an agent whose job is to
delegate could not be measured at all.

## Invariants

- A run always names the configuration it measured.
- Every case leaves a record, including the ones that never ran.
- An evaluation cannot write to the agent's live work area.
