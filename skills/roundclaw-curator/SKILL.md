---
name: roundclaw-curator
description: Review a roundclaw fleet and propose improvements — read request history to find where agents fail, see what changed between agent versions, write and run eval sets, compare two eval runs to decide whether a change helped, and file proposals that a human approves before anything is applied. Use when asked to review how agents are doing, work out why an agent got worse, test an agent change before shipping it, evaluate a new version, set up a recurring fleet review, or set up a curator agent. Also use when the user mentions "에이전트 평가", "eval 돌려줘", "회귀", "버전 비교", "제안 승인", roundclaw eval, or roundclaw proposal.
---

# Reviewing and improving a roundclaw fleet

This is the loop for making agents better on evidence rather than on impression:

```
history  →  what is going wrong
versions →  what changed, and when
eval     →  measure the agent as it is
change   →  propose a new version
eval     →  measure the new one
compare  →  did it actually help
proposal →  a person decides
```

You do not need the whole loop every time. Reading history and filing one
proposal is a complete, useful piece of work.

**You need a full-scope token.** For managing agents, tools and schedules, use
the `roundclaw-fleet` skill; this document covers only the review side.

## The rule that matters most

**You do not decide whether a change was an improvement. `eval compare` does.**

It is arithmetic over case-level pass/fail:

- any case that passed before and fails now → **regression**
- a regression makes the verdict `worse`, or `mixed` if something also improved
- improvements with no regressions → `better`
- neither → `unchanged`, whatever the average score did

Read the verdict; do not re-derive one from the outputs. Reading outputs and
forming an impression is exactly how a regression gets talked away, and you are
the thing most likely to do it. When you report, quote `verdict`, `regressions`,
`improvements`, and name the regressed cases.

A score that moved without any pass/fail changing is judge variance, not
progress. Do not propose anything on it alone.

## 1. Read the history

```bash
roundclaw history dev --since 168h --limit 50
roundclaw history dev --since 720h --status error       # what actually broke
roundclaw history dev --full --limit 5                  # read a few whole
```

Requests and results are truncated by default; `--full` opts out. Ask for the
failures first — they are where the work is — then skim the successes for
requests the agent answered badly rather than not at all.

What you are looking for:

- turns that errored, and whether they share a cause
- requests the human had to repeat or correct
- work the agent refused or misrouted
- kinds of request nobody has an eval case for

## 2. See what changed

```bash
roundclaw version ls dev                # newest first: note, author, when
roundclaw version show dev 7            # one version: definition + persona
```

Every definition or persona write mints a version; identical writes do not, so
every row is a real change. Line the history up against the versions: an agent
that started failing on 12 March and was edited on 11 March is telling you
something.

To undo a change:

```bash
roundclaw version rollback dev 6 --note 'v7 regressed on cites-the-file'
```

Rollback applies the old version as a **new** version. The history is
append-only, so the change you undid is still on the record.

## 3. Write eval cases from real failures

An eval set is a list of cases attached to one agent. A case is:

```json
{
  "name": "cites-the-file",
  "prompt": "Review this diff and tell me what is wrong with it: ...",
  "rubric": "Does the answer name the specific file and line, rather than giving general advice?",
  "must_contain": ["main.go"],
  "must_not_contain": ["I cannot"],
  "weight": 2
}
```

- **`must_contain` / `must_not_contain`** are checked exactly, in code, before
  any judge runs. Put the non-negotiables here — they cost nothing and cannot be
  argued out of their verdict. Matching is case-insensitive.
- **`rubric`** is handed to a judge model. Write it as the question a reviewer
  would ask, and make it answerable from the output alone.
- A case with neither is a smoke test: it passes if the agent answers at all.
- **`name` is the key** two runs are lined up by. Never reuse or rename one —
  a renamed case looks like one case removed and another added.

Derive cases from real turns. A case built from a request somebody actually made
measures something that matters; an invented one measures your imagination.

**Adding cases is a proposal, not an edit.** The cases are what "better" means,
and an agent that could quietly rewrite its own marking scheme is grading its own
homework:

```bash
roundclaw proposal new --kind eval_case_add --target dev-basic \
  --why 'three of last month'"'"'s failures had no case covering them' \
  --evidence 'turns 481, 502, 517 of dev' \
  --payload - <<'JSON'
{"agent_id": "dev", "cases": [{"name": "cites-the-file", "prompt": "...", "rubric": "..."}]}
JSON
```

Cases are appended, never replaced, and a duplicate name is refused.

## 4. Run an eval

```bash
roundclaw eval run dev-basic --version 7 --notify
```

`--version` pins which agent version is measured. Without it the run uses
whatever is live and records the version number it resolved to — fine for a
one-off, useless as a baseline you will compare against later, because "live"
will have moved.

`--notify` returns immediately and wakes you with a new turn when the run
finishes. **Use it.** A run is minutes to tens of minutes of container work, and
a turn that sits polling burns its budget waiting. When the notification arrives,
that is a fresh turn — go straight to `eval compare`.

`--wait` polls instead, which suits a person at a terminal.

How a case runs, so you can reason about the numbers:

- a throwaway workspace, shaped like the agent's real one (a git worktree if the
  agent works in a repository)
- the version's own persona, written into that workspace
- the agent's tools and skills mounted — an agent stripped of its capabilities is
  not the agent anyone wants measured
- **no secrets, no supplementary groups, no agent identity**, unless the set was
  created with `full_grants`. So an eval cannot push, deploy, post to Discord or
  delegate, and a tool needing a credential will fail. That is the intended
  trade: the capability is present, the credentials are not.

If a case genuinely needs credentials, that set needs `full_grants: true` — and
say so out loud when you propose it, because a scheduled eval with full grants is
a scheduled agent doing real work.

## 5. Compare

```bash
roundclaw eval compare 12 13     # base first, candidate second
```

Read: `verdict`, `regressions`, `improvements`, `score_delta`, `cost_delta`, and
the `cases` array, which is sorted regressions-first.

Check `comparable` before you believe any of it. It is false when the two runs
used different eval sets, different agents, or when one did not finish. There is
also a note when both runs measured the *same* version — that is a variance
check, not a comparison.

A case marked `base_only` did not run in the candidate. It is usually a case that
was removed, and it is worth mentioning next to whatever removed it.

## 6. Propose, and stop

File what you would change. Do not apply it.

```bash
roundclaw proposal new --kind persona_update --target dev \
  --why 'v7 regressed on cites-the-file; the persona stopped asking for line numbers' \
  --evidence 'eval compare 12 13' --evidence 'turn 517 of dev' \
  --payload - <<'JSON'
{"persona": "You are dev...\n"}
JSON
```

Kinds and their payloads:

| kind | target | payload |
|---|---|---|
| `agent_create` | new agent id | an agent definition, optionally with `"persona"` |
| `agent_update` | agent id | an agent definition, optionally with `"persona"` |
| `persona_update` | agent id | `{"persona": "..."}` |
| `agent_delete` | agent id | none |
| `eval_case_add` | eval set id | `{"agent_id": "...", "cases": [...]}` |

`--why` is required. A change nobody can judge only gets rubber-stamped, which
defeats the queue.

A person approves through `/proposals` in Discord or
`roundclaw proposal approve <id>`. Approving applies the change through the
ordinary registry calls, so it mints a version and can be rolled back like any
hand edit.

**Do not approve your own proposals.** Your token can — nothing in the server
stops it — and that is exactly why this is a rule rather than a permission. An
agent that files a proposal and approves it has not added a safeguard, it has
added a step to changing the fleet unattended. Post the proposal, say what it
does, and wait.

Post your findings in the channel too. A proposal sitting in a queue nobody
looked at is not a review.

## Running as a scheduled review

The loop runs long — an eval alone outlasts a turn — so split it across turns
rather than trying to hold one open. The notification from `--notify` starts the
next turn for you.

```
turn 1  history + versions + eval run --notify   →  ends
turn 2  (woken by the run) eval compare, then propose  →  ends
turn 3  (a human approves in Discord)
```

A weekly schedule to drive turn 1:

```bash
curl -s -X PUT "$ROUNDCLAW_URL/v1/schedules/curator-weekly" \
  -H "Authorization: Bearer $ROUNDCLAW_API_TOKEN" -H 'Content-Type: application/json' \
  -d '{
    "agent_id": "curator",
    "cron": "0 9 * * 1",
    "timezone": "Asia/Seoul",
    "prompt": "Weekly fleet review. For each enabled agent: read its history since last week and its versions; if it changed or failed, run its eval set pinned to the current version with --notify and stop. When a run comes back, compare it against the last run of the previous version and file proposals for anything that regressed. Post what you found. Approve nothing.",
    "channel_id": "<the curator channel>",
    "suppress_if": "nothing to report",
    "enabled": true
  }'
```

Keep it bounded. Every case is a container start and at least one model call, so
review the agents that changed or failed, not all of them every week. Say in the
channel which agents you skipped and why — a review that silently covered half
the fleet reads as one that covered all of it.

## Setting up the curator agent

Same recipe as any management agent (see `roundclaw-fleet` → "Setting up a
management agent"), with two differences: grant **both** skills, and give it its
own private channel.

```json
{
  "id": "curator",
  "description": "Reviews the fleet and proposes improvements",
  "permission_mode": "acceptEdits",
  "allowed_tools": ["Read", "Grep", "Glob", "Bash"],
  "tools": ["admin-cli"],
  "skills": ["roundclaw-fleet", "roundclaw-curator"],
  "require_mention": true,
  "discord_channels": ["<a PRIVATE channel>"]
}
```

Its persona should say, in its own words: review on evidence, never approve your
own proposals, quote `eval compare` rather than re-deciding it, and report what
you skipped.

Then register the skills and the schedule:

```bash
roundclaw skill set roundclaw-fleet   --path /path/to/roundclaw/skills/roundclaw-fleet
roundclaw skill set roundclaw-curator --path /path/to/roundclaw/skills/roundclaw-curator
```

## Things that will bite you

- **An unpinned run is not a baseline.** `--version` or it is worthless in a
  week.
- **A renamed case is a lost case.** It becomes one `base_only` and one
  `new_case`, and the trend line breaks.
- **Evals cost money.** A 20-case set is 20 container starts plus 20 judge calls
  per run, and a comparison is two runs.
- **The judge is strict but not clever.** Rubrics answerable from the output work;
  rubrics needing outside knowledge produce noise.
- **A failed case is recorded as a zero, and the run continues.** Nine answers
  are still worth comparing. Check whether the zero was an error or a bad answer
  before proposing anything about it — `reason` says which.
