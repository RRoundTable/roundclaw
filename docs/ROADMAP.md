# Roadmap

Work on **Now**. Nothing moves up from Next until Now is empty.

Now holds one item. Two is the limit; a third means something here was not
really started.

## Now

- **An agent improves itself.**

  The pieces of a self-improvement loop already exist, pointed outward: a curator
  agent reads history and versions, runs evaluations, compares two runs, and
  files a proposal a person approves. Turned inward — the agent as its own
  subject, and a measurement where the curator has a person — the same four
  pieces let an agent improve without waiting on anybody.

  Two things are missing before that is possible. The tools and skills an agent
  depends on have no history at all, so there is nothing to put back and no way
  to say what an agent was when it did something. And those dependencies carry
  live state that does not always survive a session, which an agent currently
  discovers by failing a turn.

  Specified in [specs/self-improvement.md](specs/self-improvement.md) and
  [specs/tool-state.md](specs/tool-state.md) (`spec/003`).

  **Decided before writing code:**

  - An agent's container carries a credential that identifies it, and the write
    surface it opens is bounded by subject as well as by path, so an agent may
    change itself and nothing else (`adr/003`).
  - A tool's restore contract declares what reachable means rather than carrying
    a command to run: the registry has never stored a command, and a restore
    runs ahead of the gate that would judge one (`adr/004`).
  - A tool declares the set of things it is made of, or it has no version.
    roundclaw cannot tell from the row whether a mount is the tool itself or a
    client's configuration, so it does not guess (`adr/005`).

  Nothing here is left open. What remains is implementation.

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
