# roundclaw

A durable orchestrator for Claude Code. Agents run in containers, work survives
worker crashes, and Discord, Slack and the HTTP API feed the same queue.

## Read these first

| Document | What it holds |
|---|---|
| [docs/GOAL.md](docs/GOAL.md) | The single goal, how we know it is met, what is out of scope |
| [docs/ROADMAP.md](docs/ROADMAP.md) | Now / Next / Later. Work on **Now** |
| [docs/specs/README.md](docs/specs/README.md) | What the system does, as user-observable behaviour |
| [docs/architecture/README.md](docs/architecture/README.md) | How it is built — layers, import rules, the constraints that are easy to break |
| `git log` on `adr/*` and `spec/*` tags | Decisions already made, and why |

`docs/specs/` is prescriptive (what it should do). `docs/architecture/` is
descriptive (how it is put together). Neither replaces the other.

## Working rules

1. **Single goal.** Every change serves `docs/GOAL.md`. If a request does not
   obviously connect to it, ask why before writing code.
2. **YAGNI.** No speculative abstraction. Three similar lines beat a premature
   one.
3. **Context-aware decisions.** No dependency or pattern is adopted because it is
   common. Record the reason as an ADR.
4. **High cohesion, low coupling.** `internal/core` imports no adapter. The
   `internal/temporal/contract` package exists so `activity` and `workflow` never
   import each other. Renaming anything in it breaks in-flight workflows.
5. **Fail fast.** No `TODO`, `FIXME`, `HACK`, `XXX` in committed code. A swallowed
   error is a bug — handle it or propagate it.

## Two invariants that override convenience

- **Status and stop never go through an LLM.** They have to work when an agent is
  wedged, which is exactly when someone asks.
- **Steering is always explicit.** Nothing infers "interrupt" from message
  content. The worst case for a misrouted request is that it waits.
