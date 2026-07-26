package main

import (
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
)

// Reading what happened and what changed.
//
//	roundclaw history <agent>            what has been asked of it, and how it went
//	roundclaw version ls|show|rollback   what its definition and persona used to be
//
// The two answer the halves of "why is this agent behaving like this": the turns
// show the symptom, the versions show what was changed and when. Both are reads
// against SQLite — no model call, and no dependency on Temporal being healthy —
// except rollback, which writes.

func cmdHistory(args []string) int {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	base, token := commonFlags(fs)
	limit := fs.Int("limit", 20, "how many turns to return (max 200)")
	since := fs.String("since", "", "only turns since then: a duration back from now (72h) or an RFC3339 instant")
	status := fs.String("status", "", "only turns in this state: running, done, stopped, error")
	conversation := fs.String("conversation", "", "only this conversation; `default` is the agent's own")
	full := fs.Bool("full", false, "do not truncate requests and results")

	positional, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) < 1 {
		fmt.Fprintln(os.Stderr, "usage: roundclaw history <agent> [--limit N] [--since 72h] [--status error] [--conversation C] [--full]")
		return 2
	}

	q := url.Values{}
	q.Set("limit", strconv.Itoa(*limit))
	if *since != "" {
		q.Set("since", *since)
	}
	if *status != "" {
		q.Set("status", *status)
	}
	// Set only when given: the endpoint distinguishes "any conversation" from
	// "the default one", and an always-present empty value would collapse them.
	if isFlagSet(fs, "conversation") {
		q.Set("conversation", *conversation)
	}
	if *full {
		q.Set("full", "true")
	}

	var out map[string]any
	c := newClient(*base, *token)
	if err := c.do(http.MethodGet, "/v1/agents/"+positional[0]+"/turns?"+q.Encode(), nil, &out); err != nil {
		return fail(err)
	}
	printJSON(out)
	return 0
}

func cmdVersion(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: roundclaw version <ls|show|rollback> <agent> [n]")
		return 2
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	base, token := commonFlags(fs)
	limit := fs.Int("limit", 0, "how many versions to list; 0 lists all")
	note := fs.String("note", "", "why, recorded on the version a rollback creates")

	positional, err := parseFlags(fs, rest)
	if err != nil {
		return 2
	}
	if len(positional) < 1 {
		fmt.Fprintln(os.Stderr, "usage: roundclaw version <ls|show|rollback> <agent> [n]")
		return 2
	}
	agentID := positional[0]
	c := newClient(*base, *token)

	switch sub {
	case "ls":
		path := "/v1/agents/" + agentID + "/versions"
		if *limit > 0 {
			path += "?limit=" + strconv.Itoa(*limit)
		}
		var out map[string]any
		if err := c.do(http.MethodGet, path, nil, &out); err != nil {
			return fail(err)
		}
		printJSON(out)
		return 0

	case "show", "rollback":
		if len(positional) < 2 {
			fmt.Fprintf(os.Stderr, "usage: roundclaw version %s <agent> <n>\n", sub)
			return 2
		}
		n, err := strconv.Atoi(positional[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "version must be an integer")
			return 2
		}
		path := "/v1/agents/" + agentID + "/versions/" + strconv.Itoa(n)
		method := http.MethodGet
		if sub == "rollback" {
			path += "/rollback"
			method = http.MethodPost
		}
		var out map[string]any
		if err := c.doWithNote(method, path, *note, nil, &out); err != nil {
			return fail(err)
		}
		printJSON(out)
		return 0

	default:
		fmt.Fprintf(os.Stderr, "unknown version subcommand %q\n", sub)
		return 2
	}
}

// isFlagSet reports whether a flag was given on the command line, as opposed to
// holding its default. It is how an empty string stays distinguishable from an
// absent one.
func isFlagSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}
