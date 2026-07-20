package claude

import (
	"strings"
	"testing"
)

var testAgents = []AgentSummary{
	{ID: "pr-reviewer", Description: "Reviews pull requests"},
	{ID: "ops-helper", Description: "Answers infra questions"},
}

// A hallucinated agent ID is the failure mode that matters: acted on blindly it
// would spin up a workflow for an agent that does not exist.
func TestValidateDecisionRejectsUnknownAgent(t *testing.T) {
	got := ValidateDecision(RouteDecision{Action: RouteDispatch, AgentID: "does-not-exist"}, testAgents)

	if got.Action != RouteClarify {
		t.Errorf("action = %q, want %q — an invented agent id was accepted", got.Action, RouteClarify)
	}
	if got.AgentID != "" {
		t.Errorf("agent_id = %q, want it cleared", got.AgentID)
	}
}

func TestValidateDecisionAcceptsKnownAgent(t *testing.T) {
	want := RouteDecision{Action: RouteDispatch, AgentID: "ops-helper", Reason: "infra question"}
	if got := ValidateDecision(want, testAgents); got != want {
		t.Errorf("decision = %+v, want %+v", got, want)
	}
}

func TestValidateDecisionRejectsUnknownAction(t *testing.T) {
	got := ValidateDecision(RouteDecision{Action: "shutdown_everything", AgentID: "ops-helper"}, testAgents)

	if got.Action != RouteClarify {
		t.Errorf("action = %q, want %q", got.Action, RouteClarify)
	}
}

// ignore and clarify carry no target, so they pass through untouched.
func TestValidateDecisionPassesThroughNonDispatch(t *testing.T) {
	for _, action := range []RouteAction{RouteIgnore, RouteClarify} {
		in := RouteDecision{Action: action, Reason: "because"}
		if got := ValidateDecision(in, testAgents); got != in {
			t.Errorf("%s: decision = %+v, want %+v", action, got, in)
		}
	}
}

// With nothing to route to, the router must not call out at all.
func TestRouteWithNoAgentsIgnoresWithoutCallingOut(t *testing.T) {
	r := Router{Runtime: "definitely-not-a-real-binary", Image: "none"}

	got, err := r.Route(t.Context(), "anything", nil)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if got.Action != RouteIgnore {
		t.Errorf("action = %q, want %q", got.Action, RouteIgnore)
	}
}

// The router prompt must name every agent, or the model cannot choose one.
func TestRouterPromptListsAgentsAndForbidsInvention(t *testing.T) {
	prompt := routerPrompt("please review my PR", testAgents)

	for _, a := range testAgents {
		if !strings.Contains(prompt, a.ID) {
			t.Errorf("prompt omits agent %q", a.ID)
		}
		if !strings.Contains(prompt, a.Description) {
			t.Errorf("prompt omits description of %q", a.ID)
		}
	}
	if !strings.Contains(prompt, "please review my PR") {
		t.Error("prompt omits the message being routed")
	}
	if !strings.Contains(prompt, "Never invent") {
		t.Error("prompt does not forbid inventing an agent id")
	}
}
