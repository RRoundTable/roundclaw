# Webhooks

Letting an outside system that holds no credential deliver an event to an agent.

## Behaviours

### A sender that cannot hold a token can still be heard

**Given** an outside system that cannot be given a credential and will not be
updated when one is rotated
**When** it delivers an event
**Then** the event is accepted on the strength of a shared secret it signs with.

This is why webhooks are a separate door: the callers are a different kind of
caller.

### An event that is not properly signed is refused

**Given** an event whose signature does not match
**When** it arrives
**Then** it is refused and the agent never sees it.

### The signature covers exactly what was sent

**Given** an event body
**When** its signature is checked
**Then** it is checked against the bytes received.

Interpreting the body first and re-deriving it would verify something the sender
never sent.

### An oversized payload is refused

**Given** an event far larger than events are
**When** it arrives
**Then** it is refused rather than processed.

### An accepted event becomes ordinary work

**Given** a valid event for an agent
**When** it is accepted
**Then** the agent works on it like any other request, under the same queue and
the same limits.

## Invariants

- An unsigned or wrongly signed event never becomes work.
- Webhook delivery grants no ability an ordinary request does not have.
