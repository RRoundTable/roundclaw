# Files

Sending an agent a file, and getting files back.

## Behaviours

### An attached file is where the agent was told it would be

**Given** a request with a file attached
**When** the agent works on it
**Then** the file is present at the path the request named.

### An attachment survives a retry

**Given** work with attached files
**When** it is retried after a crash
**Then** the files are still there for the retried attempt.

### A large attachment is not copied around

**Given** a large file
**When** it is handed to the agent
**Then** it is made available without being duplicated.

### An agent can return a file instead of many messages

**Given** an agent with a long or structured result
**When** it wants to deliver it
**Then** it can send a file rather than splitting the content across many
messages.

### Files land in the work area the work actually uses

**Given** a file sent into a thread
**When** the agent works on it
**Then** the file is in that conversation's work area, not another one's.

### Staged files are reclaimed only once nothing can want them

**Given** files staged for work that has finished
**When** they are cleaned up
**Then** it happens only after no retry could still need them.

## Invariants

- A file named in a request is present when the work runs.
- Cleanup never removes something a pending retry would need.
