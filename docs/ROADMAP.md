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

- _placeholder — nothing is committed here yet._

## Later

- _placeholder — nothing is committed here yet._
