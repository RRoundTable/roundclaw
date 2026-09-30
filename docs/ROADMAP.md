# Roadmap

Work on **Now**. Nothing moves up from Next until Now is empty.

Now holds one item. Two is the limit; a third means something here was not
really started.

## Now

- **Routines: known work handed off, admitted and improved on evidence.**

  An agent solves a new problem once. The second time it arrives, the agent
  should hand it to something that already knows how and spend its own turns on
  what is new. A routine is that something: a graph of steps with a frozen
  contract, run durably, observable step by step, admitted by roundclaw itself
  when the same work keeps succeeding, improved when its failures gather at one
  step, and archived when it stops earning its place. It replaces workflows.

  Two things make it trustworthy. Every lifecycle decision is arithmetic on
  standards the routine did not set — its trial cases are written by something
  that never saw it, and its callers can state what they need back. And handing
  work off costs the delegator only the result, never the delegate's progress.

  Self-improvement, the previous item, is done. Its loose ends are prerequisites
  here, because a routine's owner improves it with the same credential: a
  rollback does not restore the grants its version named; an unmarked evaluation
  case is stored as zero; a self token can roll back tools it does not hold; the
  CLI prefers the wider token and asserts a reserved author; no skill teaches an
  agent to change itself; and the self credential is switched off.

  Specified in [specs/routines.md](specs/routines.md) (`spec/004`).

  **Decided before writing code:**

  - A routine is a graph document one human-owned interpreter runs; steps are activities keyed by stable ids, recorded to SQLite as they start and finish, and a result is delivered on every terminal path (`adr/006`).
  - A process step is an allowlisted image and a literal argv in a container with only the run's work area mounted, which amends adr/004 rather than bending it (`adr/007`).
  - A routine is compiled at admission — graph, references, authority, bounds, effects — and refused whole with every problem named; a run pins what it resolves in a manifest (`adr/008`).
  - A routine's authority is its owner's, its contract is frozen, and its steps carry no roundclaw credential, so every hand-off is a step of the graph (`adr/009`).
  - Routine steps run on their own queue with their own container budget, so a busy library cannot starve interactive turns (`adr/010`).
  - A step is a function of declared inputs with a declared effect; reruns reuse only what the record can stand in for, report what they could not put back, and never repeat an external effect (`adr/011`).

## Next

- **Human-in-the-loop as a general capability.**

  Today there is exactly one place the system can ask a person to decide:
  `proposals`, which only ever answers "apply this fleet change, or not". Every
  other kind of human judgement — which of these candidates is better, is this
  output good enough to keep, should this run continue — has nowhere to live.

  The work is to make "wait for a person to decide" something the system supports
  generally, rather than a feature of one table with one verb.

  **Open question, decide before writing code:** whether that is one mechanism
  that covers both approving a change and choosing among candidates, or a second
  mechanism beside `proposals`. They differ in a way that may not be papered
  over — approving a proposal *applies* something, while choosing a candidate
  applies nothing and only records a label. Settle it with
  `/propose-architecture`.

## Later

- _placeholder — nothing is committed here yet._
