# Goal

## Primary goal

Run a fleet of Claude Code agents as a service — durable on Temporal, and able to
hand work to each other.

Both halves are load-bearing. **Durable**: a crash never costs work in progress.
**Collaborative**: an agent can delegate to another agent and get the answer
back, so the fleet does things one agent could not.

## Success criteria

1. **A crash resumes the same conversation.** A worker killed mid-turn retries and
   reattaches to the same Claude session. The turn restarts; the conversation does
   not.
2. **A delegated result always comes back.** When one agent hands work to another,
   the answer reaches the delegator even if the delegator's process, its connection
   and the worker have all died in between — with nothing required of the agent's
   own behaviour.
3. **Requests are never merged.** N requests arriving during one long turn produce
   N turns and N replies.

## Out of scope

- **Customising Claude Code.** No fork, no patch, no Agent SDK. A turn is a
  `claude -p` process and the CLI is a black box.
- **Shipping roundclaw code into the agent image.** The container holds the
  `claude` binary and nothing of ours.
- **Inferring intent from message content.** Steering and stopping are explicit
  commands. Nothing reads a message and decides on its own to interrupt work.

## Why these are out of scope

The first two are the same bet stated twice: roundclaw is worth something only if
it survives Claude Code changing underneath it. Every line we would add inside
the CLI or its image is a line that breaks on the next release.

The third is about blast radius. A misread request that queues costs a wasted
turn. A misread request that interrupts destroys work that was running.
