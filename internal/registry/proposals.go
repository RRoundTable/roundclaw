package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Proposed changes, waiting on a person.
//
// A proposal is a change written down but not made: what to change, why, and
// what evidence supports it. Something else — a person, through Discord or the
// CLI — approves it, and only then is it applied.
//
// The reason it exists is that the thing writing proposals is an agent reviewing
// the fleet on a schedule, and an agent that edits the fleet unattended is one
// prompt injection away from editing it badly. Writing the change down turns an
// unattended edit into an attended one, and gives the decision a record: who
// approved what, on what evidence, and which version it produced.
//
// Note what this is not. Nothing here *prevents* a full-scope token from calling
// PUT /v1/agents/{id}/definition directly and skipping the queue — the gate is a
// convention the curator's instructions keep, not a permission the server
// enforces. What it does guarantee is that a change made this way is recorded,
// atomic, and reversible.

const proposalSchema = `
CREATE TABLE IF NOT EXISTS proposals (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    kind        TEXT NOT NULL,
    target      TEXT NOT NULL DEFAULT '',    -- agent id, eval set id: what it acts on
    payload     TEXT NOT NULL DEFAULT '{}',  -- the change itself, shaped by kind
    rationale   TEXT NOT NULL DEFAULT '',    -- why, in the proposer's words
    evidence    TEXT NOT NULL DEFAULT '[]',  -- eval run ids and turn ids backing it
    status      TEXT NOT NULL DEFAULT 'pending',
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    decided_by  TEXT NOT NULL DEFAULT '',
    decided_at  INTEGER NOT NULL DEFAULT 0,
    decision    TEXT NOT NULL DEFAULT '',    -- the approver's note, or why it failed
    -- The agent version applying it produced, so an approval can be undone by
    -- rolling back to the version before it.
    applied_version INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_proposals_status ON proposals(status, id DESC);
CREATE INDEX IF NOT EXISTS idx_proposals_target ON proposals(target, id DESC);
`

// What a proposal asks for. Each names the shape of its payload, because the
// payload is stored as raw JSON and only makes sense against its kind.
const (
	// ProposeAgentCreate creates an agent. Payload: an Agent, optionally with a
	// "persona" field alongside it.
	ProposeAgentCreate = "agent_create"
	// ProposeAgentUpdate replaces a definition. Payload: an Agent, optionally
	// with "persona". Target: the agent id.
	ProposeAgentUpdate = "agent_update"
	// ProposePersonaUpdate rewrites only the instructions. Payload:
	// {"persona": "..."}. Target: the agent id.
	ProposePersonaUpdate = "persona_update"
	// ProposeAgentDelete removes an agent, keeping its workspace and history.
	// Payload: unused. Target: the agent id.
	ProposeAgentDelete = "agent_delete"
	// ProposeEvalCaseAdd adds cases to an eval set, creating it if it does not
	// exist. Payload: {"agent_id": "...", "cases": [...]}. Target: the set id.
	//
	// It is a proposal like any other because the cases are what "better" means.
	// An agent that could quietly rewrite its own marking scheme would be grading
	// its own homework.
	ProposeEvalCaseAdd = "eval_case_add"
)

// Proposal states.
const (
	ProposalPending  = "pending"
	ProposalApplied  = "applied"
	ProposalRejected = "rejected"
	// ProposalFailed is approved-but-broken: somebody said yes and applying it
	// did not work. Distinct from pending, so a retry is a decision rather than
	// something a scheduled sweep does on its own.
	ProposalFailed = "failed"
)

// Proposal is one change awaiting a decision.
type Proposal struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	Target    string          `json:"target,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	Rationale string          `json:"rationale,omitempty"`
	// Evidence holds free-form references — "eval run 12", "turn 481 of dev" —
	// that the proposer says support this. Strings rather than typed ids: the
	// useful evidence is not always something with a row.
	Evidence       []string  `json:"evidence,omitempty"`
	Status         string    `json:"status"`
	CreatedBy      string    `json:"created_by,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	DecidedBy      string    `json:"decided_by,omitempty"`
	DecidedAt      time.Time `json:"decided_at,omitempty"`
	Decision       string    `json:"decision,omitempty"`
	AppliedVersion int       `json:"applied_version,omitempty"`
}

// Validate checks a proposal before it is written.
func (p Proposal) Validate() error {
	switch p.Kind {
	case ProposeAgentCreate, ProposeAgentUpdate, ProposePersonaUpdate,
		ProposeAgentDelete, ProposeEvalCaseAdd:
	default:
		return fmt.Errorf("unknown proposal kind %q", p.Kind)
	}
	if strings.TrimSpace(p.Target) == "" {
		return fmt.Errorf("a proposal must name what it acts on")
	}
	// A change with no stated reason cannot be judged, only rubber-stamped, and
	// the whole point of the queue is that somebody judges it.
	if strings.TrimSpace(p.Rationale) == "" {
		return fmt.Errorf("a proposal must say why; an unexplained change cannot be reviewed")
	}
	if len(p.Payload) > 0 && !json.Valid(p.Payload) {
		return fmt.Errorf("payload is not valid JSON")
	}
	return nil
}

// CreateProposal files a proposal in the pending state.
func (s *Store) CreateProposal(ctx context.Context, p Proposal) (Proposal, error) {
	if err := p.Validate(); err != nil {
		return Proposal{}, err
	}
	payload := "{}"
	if len(p.Payload) > 0 {
		payload = string(p.Payload)
	}
	evidence, err := json.Marshal(nonNil(p.Evidence))
	if err != nil {
		return Proposal{}, fmt.Errorf("encode evidence: %w", err)
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO proposals (kind, target, payload, rationale, evidence, status, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Kind, p.Target, payload, p.Rationale, string(evidence),
		ProposalPending, p.CreatedBy, time.Now().UnixMilli())
	if err != nil {
		return Proposal{}, fmt.Errorf("file proposal: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Proposal{}, fmt.Errorf("proposal id: %w", err)
	}
	return s.GetProposal(ctx, id)
}

// GetProposal returns one proposal.
func (s *Store) GetProposal(ctx context.Context, id int64) (Proposal, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, kind, target, payload, rationale, evidence, status,
		       created_by, created_at, decided_by, decided_at, decision, applied_version
		FROM proposals WHERE id = ?`, id)
	p, err := scanProposal(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Proposal{}, fmt.Errorf("%w: proposal %d", ErrNotFound, id)
	}
	return p, err
}

// ListProposals returns proposals newest first, optionally filtered by status
// and target. An empty status lists every state, which is what a reviewer
// looking for "what did we decide about dev" wants.
func (s *Store) ListProposals(ctx context.Context, status, target string, limit int) ([]Proposal, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT id, kind, target, payload, rationale, evidence, status,
	             created_by, created_at, decided_by, decided_at, decision, applied_version
	      FROM proposals`
	var (
		where []string
		args  []any
	)
	if status != "" {
		where = append(where, "status = ?")
		args = append(args, status)
	}
	if target != "" {
		where = append(where, "target = ?")
		args = append(args, target)
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list proposals: %w", err)
	}
	defer rows.Close()

	var out []Proposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DecideProposal records a decision, refusing to decide one twice.
//
// The compare-and-set on status is what makes two people clicking Approve at the
// same moment safe: the second one is told the decision was already made rather
// than applying the change again.
func (s *Store) DecideProposal(ctx context.Context, id int64, status, by, note string, appliedVersion int) (Proposal, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE proposals
		SET status = ?, decided_by = ?, decided_at = ?, decision = ?, applied_version = ?
		WHERE id = ? AND status = ?`,
		status, by, time.Now().UnixMilli(), note, appliedVersion, id, ProposalPending)
	if err != nil {
		return Proposal{}, fmt.Errorf("decide proposal %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Either it does not exist or it was already decided; saying which is
		// more useful than a bare failure.
		existing, getErr := s.GetProposal(ctx, id)
		if getErr != nil {
			return Proposal{}, getErr
		}
		return existing, fmt.Errorf("%w: proposal %d was already %s by %s",
			ErrConflict, id, existing.Status, decidedByOrSomeone(existing))
	}
	return s.GetProposal(ctx, id)
}

func decidedByOrSomeone(p Proposal) string {
	if p.DecidedBy == "" {
		return "someone"
	}
	return p.DecidedBy
}

func scanProposal(row scanner) (Proposal, error) {
	var (
		p                    Proposal
		payload, evidence    string
		createdAt, decidedAt int64
	)
	if err := row.Scan(&p.ID, &p.Kind, &p.Target, &payload, &p.Rationale, &evidence,
		&p.Status, &p.CreatedBy, &createdAt, &p.DecidedBy, &decidedAt,
		&p.Decision, &p.AppliedVersion); err != nil {
		return Proposal{}, err
	}
	p.Payload = json.RawMessage(payload)
	if err := json.Unmarshal([]byte(evidence), &p.Evidence); err != nil {
		p.Evidence = nil
	}
	p.CreatedAt = time.UnixMilli(createdAt)
	if decidedAt > 0 {
		p.DecidedAt = time.UnixMilli(decidedAt)
	}
	return p, nil
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
