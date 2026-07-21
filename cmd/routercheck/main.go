// Command routercheck verifies the router against a real model.
//
// The router's plumbing — schema, envelope decoding, hallucination guard — is
// covered by unit tests with a fake CLI. What no unit test can check is whether
// a real model actually classifies messages correctly: dispatches a clear
// request to the right agent, ignores chatter, and asks to clarify when it
// genuinely cannot tell. That is a judgement about model behaviour, so it needs
// a live model and lives here rather than in `go test`.
//
// It runs a fixed set of labelled messages through claude.Router.Route and
// prints each decision against what was expected. It exits non-zero if any case
// fails, so a human can eyeball the table or a script can gate on it.
//
// It needs a real credential — the same ANTHROPIC_API_KEY or
// CLAUDE_CODE_OAUTH_TOKEN the agents use — and the agent image. Usage:
//
//	export CLAUDE_CODE_OAUTH_TOKEN=...        # or ANTHROPIC_API_KEY
//	go run ./cmd/routercheck --config roundclaw.yaml [--model claude-haiku-4-5-20251001]
//
// The agents below are fixtures, deliberately independent of what is registered,
// so the expected routing is stable no matter the deployment.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/roundtable/roundclaw/internal/claude"
	"github.com/roundtable/roundclaw/internal/config"
)

// The routable agents the router chooses between. Descriptions are distinct on
// purpose: routing is only as good as what it is told each agent is for.
var agents = []claude.AgentSummary{
	{ID: "pr-reviewer", Description: "Reviews pull requests and gives code feedback"},
	{ID: "ops-helper", Description: "Answers questions about infrastructure, deploys and on-call"},
	{ID: "docs-writer", Description: "Writes and edits user-facing documentation"},
}

// A case passes if the decision's action is one of accept, and — when it
// dispatches — the agent matches wantAgent (empty means any real agent is fine).
type testCase struct {
	message   string
	accept    []claude.RouteAction
	wantAgent string
	note      string
}

var cases = []testCase{
	{
		message:   "can you review PR #482 before I merge it?",
		accept:    []claude.RouteAction{claude.RouteDispatch},
		wantAgent: "pr-reviewer",
		note:      "clear request for the reviewer",
	},
	{
		message:   "how do I roll back yesterday's staging deploy?",
		accept:    []claude.RouteAction{claude.RouteDispatch},
		wantAgent: "ops-helper",
		note:      "clear infra question",
	},
	{
		message:   "the getting-started page is out of date, can someone rewrite it?",
		accept:    []claude.RouteAction{claude.RouteDispatch},
		wantAgent: "docs-writer",
		note:      "clear docs request",
	},
	{
		message: "lol that standup ran way over again",
		accept:  []claude.RouteAction{claude.RouteIgnore},
		note:    "ordinary chatter — must not be dispatched",
	},
	{
		message: "anyone around to help me with a thing?",
		accept:  []claude.RouteAction{claude.RouteIgnore, claude.RouteClarify},
		note:    "a request but no clue which agent — ignore or clarify, never a guess",
	},
	{
		message: "what's the weather in Seoul today?",
		accept:  []claude.RouteAction{claude.RouteIgnore, claude.RouteClarify},
		note:    "nothing any agent handles — must not dispatch",
	},
}

func main() { os.Exit(run()) }

func run() int {
	configPath := flag.String("config", "roundclaw.yaml", "path to roundclaw.yaml")
	model := flag.String("model", "", "override router.model for this run")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		return 2
	}

	// Same credential resolution the gateway gives the router: an API key or a
	// setup-token, OAuth-first.
	cred, err := cfg.Container.ResolveCredential(os.LookupEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "no credential:", err)
		return 2
	}

	chosenModel := cfg.Router.Model
	if *model != "" {
		chosenModel = *model
	}
	timeout := cfg.Router.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	bare := cfg.Container.IsAPIKey(cred)
	router := claude.Router{
		Runtime:         cfg.Container.Runtime,
		Image:           cfg.Container.Image,
		Model:           chosenModel,
		Timeout:         timeout,
		CredentialEnv:   cred.EnvName,
		CredentialValue: cred.Value,
		Bare:            bare,
	}

	mode := "full CLI (setup-token)"
	if bare {
		mode = "--bare (API key)"
	}
	fmt.Printf("routing %d messages through %s\n  credential: %s | mode: %s | model: %s\n\n",
		len(cases), cfg.Container.Image, cred.EnvName, mode, orDefault(chosenModel, "CLI default"))

	passed := 0
	for i, tc := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), timeout+15*time.Second)
		decision, err := router.Route(ctx, tc.message, agents)
		cancel()

		ok, why := evaluate(tc, decision, err)
		mark := "PASS"
		if !ok {
			mark = "FAIL"
		} else {
			passed++
		}
		fmt.Printf("[%s] %d. %s\n", mark, i+1, tc.note)
		fmt.Printf("        message : %q\n", tc.message)
		if err != nil {
			fmt.Printf("        error   : %v\n", err)
		} else {
			fmt.Printf("        decision: %s %s\n", decision.Action, decision.AgentID)
			if decision.Reason != "" {
				fmt.Printf("        reason  : %s\n", decision.Reason)
			}
		}
		if !ok {
			fmt.Printf("        why fail: %s\n", why)
		}
		fmt.Println()
	}

	fmt.Printf("%d/%d passed\n", passed, len(cases))
	if passed != len(cases) {
		return 1
	}
	return 0
}

func evaluate(tc testCase, d claude.RouteDecision, err error) (bool, string) {
	if err != nil {
		return false, "router call failed"
	}
	if !contains(tc.accept, d.Action) {
		return false, fmt.Sprintf("action %q is not in the accepted set %v", d.Action, tc.accept)
	}
	if d.Action == claude.RouteDispatch && tc.wantAgent != "" && d.AgentID != tc.wantAgent {
		return false, fmt.Sprintf("dispatched to %q, expected %q", d.AgentID, tc.wantAgent)
	}
	return true, ""
}

func contains(actions []claude.RouteAction, want claude.RouteAction) bool {
	for _, a := range actions {
		if a == want {
			return true
		}
	}
	return false
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
