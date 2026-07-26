package adapter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/roundtable/roundclaw/internal/registry"
)

func proposalPath(id int64) string { return "/v1/proposals/" + strconv.FormatInt(id, 10) }
func approvePath(id int64) string  { return proposalPath(id) + "/approve" }
func rejectPath(id int64) string   { return proposalPath(id) + "/reject" }

// propose files a proposal and returns its id.
func propose(t *testing.T, srv *httptest.Server, kind, target, why string, payload any) int64 {
	t.Helper()
	body := map[string]any{"kind": kind, "target": target, "rationale": why}
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		body["payload"] = json.RawMessage(raw)
	}
	resp := send(t, srv, http.MethodPost, "/v1/proposals", body, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("filing a proposal returned %d", resp.StatusCode)
	}
	return decode[registry.Proposal](t, resp).ID
}

// A proposal changes nothing until somebody approves it. This is the whole
// reason the queue exists, so it is asserted directly rather than implied.
func TestAFiledProposalChangesNothing(t *testing.T) {
	srv, _, _ := newHarness(t)

	id := propose(t, srv, registry.ProposePersonaUpdate, "pr-reviewer",
		"the agent keeps answering in English", map[string]string{"persona": "Answer in Korean."})
	if id == 0 {
		t.Fatal("no proposal id")
	}

	persona := decode[struct {
		Persona string `json:"persona"`
	}](t, send(t, srv, http.MethodGet, "/v1/agents/pr-reviewer/persona", nil, nil))
	if persona.Persona != "" {
		t.Errorf("persona changed before approval: %q", persona.Persona)
	}
}

// Approving applies the change through the ordinary calls, so it mints a version
// and is rollback-able like any hand edit.
func TestApprovingAPersonaProposalAppliesItAndMintsAVersion(t *testing.T) {
	srv, _, _ := newHarness(t)

	id := propose(t, srv, registry.ProposePersonaUpdate, "pr-reviewer",
		"the agent keeps answering in English", map[string]string{"persona": "Answer in Korean."})

	resp := send(t, srv, http.MethodPost, approvePath(id), map[string]string{"by": "discord:ryu"}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve returned %d", resp.StatusCode)
	}
	out := decode[struct {
		Proposal registry.Proposal `json:"proposal"`
		Version  int               `json:"version"`
		Undo     string            `json:"undo"`
	}](t, resp)

	if out.Proposal.Status != registry.ProposalApplied {
		t.Errorf("status = %q, want applied", out.Proposal.Status)
	}
	if out.Proposal.DecidedBy != "discord:ryu" {
		t.Errorf("decided_by = %q; the record must name who said yes", out.Proposal.DecidedBy)
	}
	if out.Version != 2 {
		t.Errorf("version = %d, want 2", out.Version)
	}
	if out.Undo == "" {
		t.Error("no undo was offered for a change that has a previous version")
	}

	persona := decode[struct {
		Persona string `json:"persona"`
	}](t, send(t, srv, http.MethodGet, "/v1/agents/pr-reviewer/persona", nil, nil))
	if persona.Persona != "Answer in Korean." {
		t.Errorf("persona after approval = %q", persona.Persona)
	}
}

// Two people clicking Approve at the same moment must not apply the change
// twice — the second is told the decision was already made.
func TestAProposalCannotBeDecidedTwice(t *testing.T) {
	srv, _, _ := newHarness(t)

	id := propose(t, srv, registry.ProposePersonaUpdate, "pr-reviewer", "because",
		map[string]string{"persona": "first"})

	if resp := send(t, srv, http.MethodPost, approvePath(id), nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("first approve returned %d", resp.StatusCode)
	}
	if resp := send(t, srv, http.MethodPost, approvePath(id), nil, nil); resp.StatusCode != http.StatusConflict {
		t.Errorf("second approve returned %d, want 409", resp.StatusCode)
	}
	// And rejecting a decided proposal is refused for the same reason.
	if resp := send(t, srv, http.MethodPost, rejectPath(id), nil, nil); resp.StatusCode != http.StatusConflict {
		t.Errorf("rejecting an applied proposal returned %d, want 409", resp.StatusCode)
	}
}

func TestRejectingLeavesEverythingAlone(t *testing.T) {
	srv, _, _ := newHarness(t)

	id := propose(t, srv, registry.ProposeAgentDelete, "pr-reviewer", "it looks unused", nil)
	resp := send(t, srv, http.MethodPost, rejectPath(id), map[string]string{"note": "still in use"}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reject returned %d", resp.StatusCode)
	}
	if p := decode[registry.Proposal](t, resp); p.Status != registry.ProposalRejected || p.Decision != "still in use" {
		t.Errorf("proposal = %+v; want rejected with the note kept", p)
	}
	if resp := send(t, srv, http.MethodGet, "/v1/agents/pr-reviewer/definition", nil, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("the agent is gone after a rejection: %d", resp.StatusCode)
	}
}

// An unexplained change cannot be reviewed, only rubber-stamped — and the point
// of the queue is that somebody reviews it.
func TestAProposalNeedsAReason(t *testing.T) {
	srv, _, _ := newHarness(t)
	resp := send(t, srv, http.MethodPost, "/v1/proposals", map[string]any{
		"kind": registry.ProposePersonaUpdate, "target": "pr-reviewer",
		"payload": map[string]string{"persona": "x"},
	}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a proposal with no rationale returned %d, want 400", resp.StatusCode)
	}
}

// An eval set is what "better" means. Adding cases is fine; silently replacing
// them would let the thing being measured narrow its own exam.
func TestEvalCaseProposalAppendsAndRefusesToOverwrite(t *testing.T) {
	srv, _, _ := newHarness(t)

	first := propose(t, srv, registry.ProposeEvalCaseAdd, "pr-basic",
		"three failures last month had no case covering them",
		map[string]any{
			"agent_id": "pr-reviewer",
			"cases": []map[string]any{
				{"name": "cites-the-file", "prompt": "review this diff", "rubric": "does it name the file"},
			},
		})
	if resp := send(t, srv, http.MethodPost, approvePath(first), nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("approving the first case set returned %d", resp.StatusCode)
	}

	// A second, distinct case is appended to the set the first one created.
	second := propose(t, srv, registry.ProposeEvalCaseAdd, "pr-basic", "another gap",
		map[string]any{
			"agent_id": "pr-reviewer",
			"cases":    []map[string]any{{"name": "flags-secrets", "prompt": "review this", "rubric": "does it spot the key"}},
		})
	if resp := send(t, srv, http.MethodPost, approvePath(second), nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("appending a case returned %d", resp.StatusCode)
	}

	set := decode[registry.EvalSet](t, send(t, srv, http.MethodGet, "/v1/evals/pr-basic", nil, nil))
	if len(set.Cases) != 2 {
		t.Fatalf("set has %d cases, want both", len(set.Cases))
	}

	// Reusing a name would overwrite an existing question, so it is refused.
	third := propose(t, srv, registry.ProposeEvalCaseAdd, "pr-basic", "rewrite it",
		map[string]any{
			"agent_id": "pr-reviewer",
			"cases":    []map[string]any{{"name": "cites-the-file", "prompt": "something easier", "rubric": "anything"}},
		})
	if resp := send(t, srv, http.MethodPost, approvePath(third), nil, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("overwriting an existing case returned %d, want 400", resp.StatusCode)
	}
	// And the failed attempt is recorded as failed, not left pending for
	// somebody to approve again.
	if p := decode[registry.Proposal](t, send(t, srv, http.MethodGet, proposalPath(third), nil, nil)); p.Status != registry.ProposalFailed {
		t.Errorf("failed proposal status = %q, want failed", p.Status)
	}
}
