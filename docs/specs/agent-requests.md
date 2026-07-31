# Requests

Asking an agent to do something, and getting an answer. Requests arrive from a
chat channel or from a program; both land in the same queue and are treated
identically once accepted.

## Behaviours

### An idle agent answers

**Given** an agent that is not working
**When** somebody sends it a request
**Then** it starts work immediately and answers when it is done.

### A busy agent queues rather than merges

**Given** an agent already working on something
**When** three more requests arrive before it finishes
**Then** each one is worked separately and answered separately — three answers,
in the order the requests arrived.

This is the behaviour roundclaw exists for. A system that collapses "things
arrived while busy" into a single flag answers three questions once.

### The answer finds the asker

**Given** a request that named how to answer it — a chat channel, a callback, or
simply a place to look
**When** the work finishes
**Then** the answer is delivered there, even if the asking program exited, its
connection dropped, and the machine doing the work was restarted in between.

### A long answer is not truncated into nonsense

**Given** an answer longer than the channel will carry in one message
**When** it is delivered to a chat channel
**Then** it arrives in several messages split at character boundaries, so no
message ends mid-character.

### A crashed run resumes the conversation, not the turn

**Given** work in progress
**When** the machine running it dies
**Then** the work is retried and continues the same conversation with its context
intact. The interrupted attempt's unfinished output is lost; the conversation is
not.

### An agent that does not exist fails immediately

**Given** a request naming an agent that was deleted or never existed
**When** it is submitted
**Then** it fails at once with the reason, rather than being retried.

## Invariants

- Accepting a request and answering it are separate events. Nothing holds a
  connection open for the duration of the work.
- Two requests never share an answer.
- The order requests are answered in is the order they were accepted.

## Not guaranteed

- That a request is answered within any particular time. Work takes as long as it
  takes; [turn control](turn-control.md) is how somebody intervenes.
