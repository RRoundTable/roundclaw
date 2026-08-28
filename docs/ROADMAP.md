# Roadmap

Work on **Now**. Nothing moves up from Next until Now is empty.

Now holds two items. They are genuinely parallel — one adds a place the system
can be reached from, the other adds a thing it can do — and they touch no
common code. Two is the limit; a third means something here was not really
started.

## Now

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

- **Slack as a second place the fleet can be reached from.**

  Everything the system does is reachable from Discord or from the HTTP API.
  A team that works in Slack can only reach it through the API, which means a
  person has to hold a token and type CLI commands to ask an agent anything.

  The work is to make Slack an ordinary inbound and outbound channel: a bound
  channel admits messages as turns, results come back to the thread they were
  asked in, and every command Discord offers exists there too. Nothing about
  what an agent *is* changes — this is a surface, not a capability.

  Full parity is the committed scope, and it is the expensive half: Slack's
  Block Kit is a different shape from Discord's modals and autocomplete, so the
  agent, schedule and proposal commands are rewritten rather than ported.

  **Decided before writing code:** Socket Mode rather than the Events API
  (`adr/001`), and a self-describing channel reference rather than a platform
  column (`adr/002`).

## Next

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

  Nothing here is left open. What remains is implementation, and that waits for
  Now to empty.

## Later

- _placeholder — nothing is committed here yet._
