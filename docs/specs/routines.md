# Routines

Work the fleet already knows how to do, written down once as a graph of steps,
that any agent can hand work to — admitted, improved and retired by the system
on evidence.

An agent is a model and a harness. A routine is agents and a graph: a
**contract** (what it is for, what it takes, what it returns, what counts as
done, and the most it may do to the outside world) and a graph of **steps**
(nodes of the graph) that carries the contract out.

## Why it exists

An agent solves a new problem once, in conversation. When the same problem comes
back it should not have to solve it again: it should hand it to something that
already knows how, and spend its own turns on what is actually new.

Workflows were the first attempt at that and could not carry it. They were
straight lines of prompts with no versions, no way to see past runs, no way to
tell whether a step had actually done its job, and nowhere to send a result but a
chat channel. A routine replaces them.

Three ideas run through everything below:

- **A step states what it is for and how to tell it is done, not how to do it.**
  How much a step fixes varies with how well its work is understood: a fixed
  program where the method is settled, a fixed instruction where the wording is,
  an open goal with completion conditions where only the outcome is known, and a
  step that plans its own sub-graph where not even the shape is.
- **Agents propose; arithmetic decides.** Drafting a routine and fixing a step
  are creative work, done by agents. Admitting a routine, promoting a new
  version and archiving an old one are decided by stated rules on recorded
  figures — never by an agent's opinion, and never by the party being judged.
- **Handing work off costs the delegator only the result.** The delegate's
  progress never enters the delegator's context.

## Behaviours

### Admission

#### roundclaw admits routines on its own

**Given** the same work succeeding again and again — the same sequence of
actions, finished, not redone, across different conversations
**When** it has happened often enough within a stated period
**Then** a routine is drafted from that work and put on trial, without anybody
asking. An agent can also submit a routine it wrote. Neither needs a person's
approval.

Recurrence is recognised from what agents *did* — the shape of their actions —
never from what anybody wrote.

#### A routine is admitted only through a trial

**Given** a drafted or submitted routine
**When** it is tried
**Then** it redoes the work it was drawn from, in isolation, and is marked by
checks written by something other than its author, which never saw it. It is
admitted only if it did that work at least as well as the work was done before,
for no more than it cost before. The same checks are applied to the original
results, so "as well as before" is a count, not an impression.

A routine that fails its trial is not offered to anybody. It stays in the record
with the figures that rejected it.

#### A routine is checked in full before its trial

**Given** a routine with anything in it that cannot work
**When** it is submitted
**Then** it is refused, with every problem named by the step it is in, and
nothing is recorded. That covers: an input no earlier step produces; a cycle; a
routine that contains itself, directly or through another; an agent, routine or
image that does not exist; a loop with no bound; an open step with no stated way
to tell it is done; a step beyond what its owner may do; an effect beyond the
contract's ceiling; and a worst case too large to run durably.

Failing at admission costs nothing. Failing three hours into a run costs the
three hours.

#### A routine's trial cases become what it is measured by

**Given** an admitted routine
**When** it is measured from then on
**Then** its trial cases and their checks are the independent standard it is
held to, and nothing that changes the routine can change them.

#### An admitted routine never changes under a run

**Given** a run in progress
**When** the routine gets a new version
**Then** the run finishes on the version it started with.

#### A run states exactly what it ran

**Given** a finished run
**When** somebody looks at it
**Then** they see the routine's version and the version of every agent, model,
tool and image its steps used.

### Steps

#### An open step decides how, and is done only when its conditions hold

**Given** a step stated as an intent and completion conditions
**When** it runs
**Then** the agent running it is told the intent and the conditions and chooses
its own method, and the step is complete only when the conditions hold.

#### Each step is held to exactly as much as it states

**Given** a routine that mixes a fixed program, a fixed instruction, an open goal
and a planning step
**When** it runs
**Then** the program runs as written with no model involved, the instruction is
carried out as given, the goal is pursued however the agent chooses until its
conditions hold, and the planning step writes the graph for its own part of the
work.

#### Done is decided by stated rules before opinion

**Given** a step whose completion can be checked exactly — an exit status, text
that must or must not appear, an output that must exist, a check program that
must succeed
**When** it finishes
**Then** those rules decide, and nothing is asked to judge. A judgement is asked
for only when a step names one, and a judgement that cannot be obtained leaves
the step unmarked, not failed.

#### A step that falls short tries again, told why

**Given** an open step whose conditions do not hold
**When** it has attempts left
**Then** it runs again, told what fell short, remembering what it already tried.
When it has none, it fails with that reason. A fixed program is not retried for
giving the same answer twice.

#### A routine can be a step of another

**Given** a routine used as a step
**When** the outer routine runs
**Then** the inner one runs as its own run, its result becomes the step's output,
and its run is reachable from the outer one.

#### A step can write the graph for its own part of the work

**Given** a planning step
**When** it writes out a graph
**Then** that graph is checked exactly as a submitted routine is, refused with
reasons if it fails, and otherwise run as an inner routine within the outer run's
remaining bounds.

#### Reading steps run together; steps that change the work area take turns

**Given** independent steps
**When** they are ready
**Then** those that only read run at the same time, those that change the work
area take turns, and a step that depends on another waits for it.

#### A step never waits for a person

**Given** any step
**When** it runs
**Then** it never stops to ask anybody anything. A routine has nobody to answer a
prompt.

#### A step hands nothing onward by itself

**Given** a step running inside a routine
**When** it tries to hand work to an agent, start a routine, or change the fleet
**Then** it cannot. Every hand-off inside a routine is a step of its graph, so
the graph is the whole of what the routine did.

### Running

#### A crash resumes the run at the step it was on

**Given** a run partway through
**When** the machine running it dies
**Then** it continues from the step it had reached, not from the beginning.

#### Runs do not collide, and carry no conversation

**Given** a routine run twice at once, or run again tomorrow
**When** both runs are in progress, or the second one starts
**Then** neither interferes with the other, and neither remembers the other.

#### A run works in a work area of its own

**Given** a run
**When** its steps change files
**Then** they change the run's own copy, never any agent's live work area.

#### A result outlives whoever asked for it

**Given** an agent that handed work to a routine and ended its turn
**When** the run finishes — or fails, is stopped, or could not start at all
**Then** the result reaches that agent as new work, even if the agent's process,
its connection and the machine running the work have all died in between.

#### The result goes wherever the request said

**Given** a run started from a chat channel, a program, or an agent
**When** it finishes
**Then** its result is delivered there — any return address a request can have,
not only a channel.

#### Stopping reaches every running step

**Given** a run with steps and inner runs in progress
**When** somebody stops it
**Then** every running step is asked to stop, given a grace period, and then
forced, and nothing is left reported as running. Stopping never depends on a
model.

#### Every run is bounded

**Given** a run
**When** it starts
**Then** its attempts, depth, time and spend are bounded. Time is a hard bound.
Spend is checked before each step, so a single step can overrun it, and the
record says so when it does.

### Observation

#### Every step leaves a record from the moment it starts

**Given** a run in progress or finished
**When** somebody looks at it
**Then** for every attempt of every step they see its state, what it was given,
what it produced, a verdict for each of its completion conditions, the feedback
it passed on, its cost and duration, which model ran it and how many tokens of
each kind it used. A step that never ran is recorded with the reason, and a
running step can be watched as it works.

Cost is the figure the model reported. The tokens behind it are kept beside it,
so the figure can be checked.

#### A record names the step and the version that produced it

**Given** failures across many runs
**When** they are counted
**Then** failures of the same step of the same version count together, so a
fault can be laid against the part of the graph that produced it.

#### What a step changed can be seen

**Given** a step that changed the work area
**When** somebody looks at it
**Then** they see what it changed.

#### A run shows its whole tree

**Given** a run with inner runs
**When** somebody looks at it
**Then** they see every step, every inner run, and who asked for it; and an agent
can find the runs it asked for.

#### Reading a run never depends on a model or on the run being healthy

**Given** a run that is stuck
**When** somebody asks for its status or its record
**Then** they get it.

### Selection and delegation

#### An agent can see what it needs to choose

**Given** an agent deciding whether to hand work off
**When** it looks at the routines on offer
**Then** it sees each one's intent, what it takes and returns, its effect ceiling
and bounds, whether it can run right now and why not if it cannot, and its record
for the version that would run, for each version, and overall: how many runs,
how often it passed and by whose standard, how long and how much it typically
takes, what one success costs, how recent runs went, and which steps fail most.

#### A record is computed, never described

**Given** a routine's record
**When** it is shown
**Then** every figure is counted from recorded runs. Unmarked runs and
infrastructure errors are never counted as failures. A routine with no completion
conditions shows how often it finished, not a pass rate. A small sample is shown
as small.

#### Choosing a routine is always an explicit act

**Given** a request arriving
**When** it reads like something a routine does
**Then** nothing sends it to that routine. An agent or a person names the routine.
The chat router may pick an agent; it never picks a routine.

A misrouted request to an agent costs a turn, because the agent can answer that
it is the wrong one. A misrouted request to a routine does work nobody asked for.

#### An agent can hand work to a routine and carry on

**Given** an agent with more to do
**When** it hands work to a routine
**Then** it is told the run has started and carries on. It is woken once, when
the work ends. None of the routine's progress enters its context.

#### What reaches the delegator is bounded

**Given** a routine or agent that produced a long result
**When** the delegator is woken
**Then** it receives the outcome, the declared outputs or the beginning of the
answer up to a stated limit, the failing step and reason if there was one, and a
handle. The full result is kept in the delegator's work area to open on purpose.
Nothing summarises it on the way.

#### Only its owner or a person runs a routine that acts on the outside world

**Given** a routine whose contract lets it act outside roundclaw
**When** an agent other than its owner tries to run it
**Then** it is refused. Any agent can run a routine that cannot act outside.

### Evolution and archival

#### A routine keeps evolving after admission

**Given** an admitted routine whose failures gather at one step
**When** enough of them have gathered
**Then** a fix for that step is drafted and put on trial, without anybody asking.

#### A new version replaces the current one only if it did no worse

**Given** a new version on trial
**When** it has run the routine's independent cases and the recent failures it
was drafted for
**Then** it replaces the current version only if no case that passed before now
fails, it fixed something it was drafted to fix, and one success does not cost
materially more. Otherwise it is recorded and never used. Returning to an earlier
version is immediate.

#### A routine is re-measured when anything it depends on changes

**Given** a routine that uses an agent, tool or image
**When** that dependency gets a new version, or a regular interval passes
**Then** the routine is measured again on its independent cases, and a case that
newly fails counts as a failure.

#### A routine is archived when it stops earning its place

**Given** a routine that goes unused for a stated period, keeps failing after its
fixes were rejected, is outdone by another routine with the same contract, or can
no longer run because something it needs is gone
**When** that has held long enough
**Then** it is archived: no longer offered, taking no new work, its runs in
progress allowed to finish, its record kept. It can be restored.

#### A person can archive, restore or roll back at any time

**Given** a routine doing harm
**When** a person archives it, restores it, or returns it to an earlier version
**Then** it happens at once. That never waits on a measurement.

#### Every lifecycle decision is recorded with its figures

**Given** a routine admitted, promoted, rejected or archived
**When** somebody asks why
**Then** the record shows the rule that decided it and the figures it decided on.

#### The lifecycle's own spend is recorded and capped

**Given** drafting, examining and trials
**When** they run
**Then** what they spend is recorded against the routine, shown beside what the
routine itself costs, and capped per day; work past the cap waits.

### Change and safety

#### Whoever improves a routine cannot change its contract

**Given** a change to a routine
**When** it would alter what the routine is for, what it takes or returns, what
counts as done, or the most it may do outside
**Then** it is refused. A different contract is a different routine, and it
starts with no record.

A record belongs to a promise. If the promise could move, the record would mean
nothing.

#### A change is attributable

**Given** a new version
**When** it is recorded
**Then** it names who made it, and an agent's authorship is established by its
credential, never asserted.

#### A routine can do no more than its owner could

**Given** a routine drawn from an agent's work, or written by one
**When** any of its steps would use an agent, tool or image beyond what its owner
may use
**Then** it is refused, and no change ever widens what the routine may do.

#### Only the owner, the steward, or a person submits a new version

**Given** an agent that neither owns a routine nor is the fleet's steward
**When** it wants the routine changed
**Then** it proposes the change for a person to decide.

#### A routine outlives its owner

**Given** a routine whose owning agent is removed
**When** it is next used
**Then** it still works, and from then on only people and the steward change it.

### Reproduce

#### A run can be repeated under the same conditions

**Given** a finished run
**When** somebody repeats it
**Then** the new run uses the same version, pins and inputs, says which run it
repeats, and states what it could not put back — a tool that has changed since,
model steps that may answer differently. It never claims to be a reproduction.

#### A run can be resumed from a step on a changed routine

**Given** a run that went wrong at one step, and a newer version
**When** somebody reruns it from that step
**Then** earlier steps that did not change are taken from the record rather than
done again, and that step and everything after it run afresh. If an earlier step
changed, the rerun is refused.

#### A rerun never repeats an effect on the outside world

**Given** a step that sent, published or deployed something
**When** a rerun takes it from the record
**Then** it is not done again. A rerun does such a step afresh only when it was
explicitly allowed to act.

### Evaluating delegation

#### Every figure says whose standard it measures

**Given** a routine's record
**When** it is shown
**Then** its figures are kept apart by who set the standard: the routine's own
conditions, the conditions its callers attached, and its independent cases. They
are never blended into one score.

#### A hand-off can state what the caller needs back

**Given** an agent handing work to a routine or to another agent
**When** it states what it needs back
**Then** the result is checked against that by rule before the agent is woken,
separately from the delegate's own checks, and the agent is told both verdicts.

#### A caller can say a result went unused

**Given** a result the caller did not use
**When** it hands the same work off again in its place
**Then** the replaced run is recorded as redone. Nothing infers this from what
anybody wrote.

#### A routine is measured on cases its owner did not write

**Given** a routine
**When** it is measured
**Then** it runs cases its owner did not write and cannot change, in its own work
area, and two of its versions are compared by arithmetic.

#### A delegating agent's choices can be measured

**Given** cases that say which routine a request should go to, or that it should
not be handed off at all
**When** an agent is measured on them
**Then** its hand-offs are recorded, not carried out, and the case checks what it
chose and what it passed.

#### Whether handing off pays can be measured

**Given** the same cases
**When** they run with routines available and without
**Then** the two are compared on results, on total cost including the
delegator's, and on how much the delegator's own context grew. In a measurement,
only routines that cannot act on the outside world run for real.

### Migration

#### Every workflow keeps working as a routine

**Given** a workflow registered before routines existed
**When** the system is upgraded
**Then** it keeps working as a routine with the same steps in the same order,
reporting where it reported before. It starts admitted, with no independent cases
until it is measured, and its record says so. Registering and running workflows
is replaced by routines.

## Invariants

- A routine can do no more than its owner could.
- A step hands nothing onward by itself.
- Every run is bounded before it starts.
- A run's record is written as the work happens and read without a model.
- A routine's contract and its independent cases never change under the one
  improving it.

## Explicitly not guaranteed

- **A trial proves a routine can redo the work it was drawn from.** It does not
  prove it handles everything its intent covers.
- **The examiner is a model, and its checks can be wrong.** They are exact rules
  wherever possible, fixed once written, and visible to anybody reading the
  routine.
- **An effect declaration is a claim.** It is enforced only as far as the
  mounts, network and credentials roundclaw controls. An agent step always
  reaches its model, so for it the declaration is enforced through credentials
  alone.
- **A routine's record spans changes to the agents it uses.** What each run used
  is recorded, so the two can be told apart; the headline figures do not.

## Known limits

- Recurrence is recognised by identical action shapes. Work done the same way
  with small variations can go unrecognised until the shapes are compared more
  loosely.
- Only a person, a routine's owner or the steward can change a routine; other
  agents propose. The steward is a role an agent is given, and until one is
  configured nothing drafts or fixes routines automatically.
