# Specification

What roundclaw does, stated as behaviour someone can observe from outside. This
is prescriptive: if the system does not do what is written here, the system is
wrong. How it is built is [docs/architecture/](../architecture/README.md), and
that is descriptive — the two are complementary, not duplicates.

Behaviours are written in outcome language. "The request waits and is answered
separately", not "the handler returns 202".

## Capabilities

| Capability | What a person gets from it |
|---|---|
| [Channels](channels.md) | Reach the fleet from the chat tool the team already works in |
| [Requests](agent-requests.md) | Ask an agent to do something and get an answer back |
| [Turn control](turn-control.md) | See what an agent is doing; stop it; redirect it |
| [Conversations](conversations.md) | Keep separate lines of work from bleeding into each other |
| [Delegation](delegation.md) | Let an agent hand work to another agent and report the result |
| [Schedules](schedules.md) | Have an agent do something on a recurring basis |
| [Workflows](workflows.md) | Run a multi-step job that belongs to no agent |
| [Fleet management](fleet-management.md) | Add, change and remove agents while the system runs |
| [Versions and rollback](versions-and-rollback.md) | Know what an agent was when it did something, and undo a change |
| [Grants](grants.md) | Widen what an agent is allowed to do |
| [Evaluation](evaluation.md) | Measure an agent, and compare two of its configurations |
| [Proposals](proposals.md) | Have a person decide before a change to the fleet takes effect |
| [Files](files.md) | Send an agent a file, and get files back |
| [History](history.md) | Look at what was asked and what happened |
| [Webhooks](webhooks.md) | Let an outside system that holds no token deliver an event |

## System-wide invariants

These hold across every capability. A change that breaks one is a change to this
document first.

1. **Status and stop never depend on a model.** They must answer when an agent is
   wedged, which is exactly when somebody asks.
2. **Interruption is always explicit.** Nothing reads the content of a request and
   decides to interrupt work. The worst outcome of a misrouted request is that it
   waits.
3. **Requests are never merged.** N requests that arrive while an agent is busy
   produce N pieces of work and N answers.
4. **A conversation owns exactly one session, one queue and one workspace.** Two
   conversations of the same agent share none of them.
5. **A result outlives the thing that asked for it.** Once work is accepted, its
   answer is delivered even if the caller, its connection and the machine running
   the work have all died since.
6. **Measuring an agent never touches what it works on.** An evaluation runs
   against a copy; it cannot write to the agent's live workspace.
7. **An answer returns to the channel that asked for it.** Which chat tool a
   request arrived through decides where its answer goes, and no capability is
   available in one chat tool and missing from another.

## Depth

These documents are not uniformly detailed, on purpose. [Proposals](proposals.md)
is the deepest because the current roadmap item generalises it. Requests, turn
control, conversations and delegation carry the goal's success criteria. The rest
state their behaviours and stop.
