# Channels

Reaching the fleet from the chat tool a team already works in.

A **channel** is a place in a chat tool that is bound to an agent. Discord and
Slack are both channels in this sense; the HTTP API is not, because nobody is
sitting in it. Every other capability in this document set is written for "a
chat channel" without saying which one — this document is what that phrase
means, and what holds no matter which one it is.

## Behaviours

### A bound channel belongs to one agent

**Given** a channel bound to an agent
**When** somebody binds it to a second agent
**Then** the binding is refused. A channel names exactly one agent, and this
holds across chat tools — a Slack channel and a Discord channel are two
different channels, but neither can be shared.

### An unbound channel does not start work

**Given** a channel bound to no agent
**When** somebody speaks in it
**Then** nothing is queued and nothing is billed, unless an agent is named
explicitly or routing is switched on for that channel.

### The answer goes back to the tool it was asked in

**Given** an agent bound to channels in more than one chat tool
**When** it is asked something in one of them
**Then** the answer arrives there and nowhere else. This holds for a result, for
anything the agent says while still working, and for a file it sends.

### Every capability is reachable from every chat tool

**Given** something a person can do from one chat tool — ask, check status,
stop, steer, manage agents and schedules, decide a proposal
**When** they are working in another one
**Then** they can do the same thing there, with the same effect.

The wording and the widgets differ, because chat tools do not offer the same
building blocks. What a person can achieve does not.

### A thread is a line of work wherever it is

**Given** a conversation started in a thread
**When** work continues in it
**Then** it keeps its own history, queue and work area, exactly as
[conversations](conversations.md) describes — and a thread in one chat tool is
no more or less a conversation than a thread in another.

### A message limit is the tool's, not the fleet's

**Given** an answer longer than the chat tool will carry in one message
**When** it is delivered
**Then** it is split to that tool's limit, not to some other tool's. A reply
that fits in one Slack message is not cut into three because Discord would have
needed three.

### Being reachable in a new tool is not being reachable to new people

**Given** an agent that a person could not command before
**When** a second chat tool is connected
**Then** they still cannot. Who may run what is decided per channel and per
person, and connecting a tool grants nobody anything.

### A chat tool that is down does not lose work

**Given** work accepted from a channel
**When** the chat tool is unreachable when the answer is ready
**Then** the answer is still recorded and still retried. Delivery is how an
answer is seen, not where it lives.

## Invariants

- A channel is bound to at most one agent, across all chat tools.
- A reply is delivered to the channel the request came from. There is no
  configuration that sends it somewhere else.
- Connecting a chat tool changes what the fleet can be reached *from*, never
  what it can *do*. No capability exists in one tool only.
- A chat tool being unavailable can never fail an agent's work.

## Not guaranteed

- That two chat tools present a capability the same way. A Discord modal and a
  Slack dialog ask for the same things; they do not look alike.
- That a chat tool is connected at all. Running with only the HTTP API is a
  supported configuration, and so is running with one chat tool and not the
  other.
