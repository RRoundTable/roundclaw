package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// Proposals from the terminal.
//
//	roundclaw proposal new ...        write a change down instead of making it
//	roundclaw proposal ls|show        what is waiting
//	roundclaw proposal approve|reject decide
//
// An agent reviewing the fleet files proposals; a person decides them. Approving
// applies the change through the ordinary registry calls, so it mints a version
// and can be rolled back like any other edit.

func cmdProposal(args []string) int {
	if len(args) == 0 {
		proposalUsage()
		return 2
	}
	sub, rest := args[0], args[1:]

	switch sub {
	case "ls":
		return proposalList(rest)
	case "show":
		return proposalShow(rest)
	case "new":
		return proposalNew(rest)
	case "approve":
		return proposalDecide(rest, "approve")
	case "reject":
		return proposalDecide(rest, "reject")
	default:
		fmt.Fprintf(os.Stderr, "unknown proposal subcommand %q\n\n", sub)
		proposalUsage()
		return 2
	}
}

func proposalUsage() {
	fmt.Fprint(os.Stderr, `usage: roundclaw proposal <command>

  new --kind K --target T --why TEXT [--payload FILE|-] [--evidence E]
      kinds: agent_create, agent_update, persona_update, agent_delete, eval_case_add
      --evidence is repeatable: "eval run 12", "turn 481 of dev"
  ls [--status pending] [--target dev]   what has been proposed
  show <id>                              one proposal in full
  approve <id> [--note TEXT]             apply it, and record who said so
  reject <id> [--note TEXT]              close it without applying

Payload by kind:
  agent_create / agent_update   an agent definition, optionally with "persona"
  persona_update                {"persona": "..."}
  agent_delete                  none
  eval_case_add                 {"agent_id": "...", "cases": [...]}

Approving is a person's job. An agent that files a proposal and approves it
itself has only added a step to changing the fleet unattended.
`)
}

func proposalList(args []string) int {
	fs := flag.NewFlagSet("proposal ls", flag.ContinueOnError)
	base, token := commonFlags(fs)
	status := fs.String("status", "", "only this state: pending, applied, rejected, failed")
	target := fs.String("target", "", "only proposals about this agent or eval set")
	if _, err := parseFlags(fs, args); err != nil {
		return 2
	}

	q := url.Values{}
	if *status != "" {
		q.Set("status", *status)
	}
	if *target != "" {
		q.Set("target", *target)
	}
	var out map[string]any
	if err := newClient(*base, *token).do(http.MethodGet, "/v1/proposals?"+q.Encode(), nil, &out); err != nil {
		return fail(err)
	}
	printJSON(out)
	return 0
}

func proposalShow(args []string) int {
	fs := flag.NewFlagSet("proposal show", flag.ContinueOnError)
	base, token := commonFlags(fs)
	positional, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) < 1 {
		fmt.Fprintln(os.Stderr, "usage: roundclaw proposal show <id>")
		return 2
	}
	var out map[string]any
	if err := newClient(*base, *token).do(http.MethodGet, "/v1/proposals/"+positional[0], nil, &out); err != nil {
		return fail(err)
	}
	printJSON(out)
	return 0
}

// evidenceList collects a repeatable --evidence flag.
type evidenceList []string

func (e *evidenceList) String() string { return strings.Join(*e, ", ") }
func (e *evidenceList) Set(v string) error {
	*e = append(*e, v)
	return nil
}

func proposalNew(args []string) int {
	fs := flag.NewFlagSet("proposal new", flag.ContinueOnError)
	base, token := commonFlags(fs)
	kind := fs.String("kind", "", "what to propose (required)")
	target := fs.String("target", "", "the agent or eval set it acts on (required)")
	why := fs.String("why", "", "why this change; a proposal without a reason cannot be reviewed (required)")
	payloadPath := fs.String("payload", "", "JSON file holding the change, or - for stdin")
	var evidence evidenceList
	fs.Var(&evidence, "evidence", "what backs this up (repeatable)")

	if _, err := parseFlags(fs, args); err != nil {
		return 2
	}
	if *kind == "" || *target == "" || *why == "" {
		fmt.Fprintln(os.Stderr, "usage: roundclaw proposal new --kind K --target T --why TEXT [--payload FILE|-]")
		return 2
	}

	body := map[string]any{"kind": *kind, "target": *target, "rationale": *why}
	if len(evidence) > 0 {
		body["evidence"] = []string(evidence)
	}
	if *payloadPath != "" {
		raw, err := readFileOrStdin(*payloadPath)
		if err != nil {
			return fail(err)
		}
		if !json.Valid(raw) {
			return fail(fmt.Errorf("payload is not valid JSON"))
		}
		body["payload"] = json.RawMessage(raw)
	}

	var out map[string]any
	if err := newClient(*base, *token).do(http.MethodPost, "/v1/proposals", body, &out); err != nil {
		return fail(err)
	}
	printJSON(out)
	return 0
}

func proposalDecide(args []string, action string) int {
	fs := flag.NewFlagSet("proposal "+action, flag.ContinueOnError)
	base, token := commonFlags(fs)
	note := fs.String("note", "", "what you want recorded alongside the decision")
	by := fs.String("by", "", "who is deciding; defaults to whatever the caller is")

	positional, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) < 1 {
		fmt.Fprintf(os.Stderr, "usage: roundclaw proposal %s <id> [--note TEXT]\n", action)
		return 2
	}

	body := map[string]string{}
	if *note != "" {
		body["note"] = *note
	}
	if *by != "" {
		body["by"] = *by
	}

	var out map[string]any
	path := "/v1/proposals/" + positional[0] + "/" + action
	if err := newClient(*base, *token).do(http.MethodPost, path, body, &out); err != nil {
		return fail(err)
	}
	printJSON(out)
	return 0
}
