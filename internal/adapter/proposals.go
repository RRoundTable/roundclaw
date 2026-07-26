package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/roundtable/roundclaw/internal/registry"
)

// Applying a proposal.
//
// This lives on the dispatcher rather than in an HTTP handler because both edges
// approve: a person clicks a button in Discord, or a person runs the CLI against
// the API. Two implementations of "what approving means" would drift, and the
// one that drifted would be the one nobody tested.

// PersonaPath is where an agent's instructions live: CLAUDE.md at the root of
// its workspace, which is what the CLI loads from the mount's cwd.
func (d *Dispatcher) PersonaPath(agentID string) string {
	return filepath.Join(d.cfg.WorkDir(agentID), "CLAUDE.md")
}

// ReadPersona returns an agent's instructions, or "" if it has none.
func (d *Dispatcher) ReadPersona(agentID string) (string, error) {
	data, err := os.ReadFile(d.PersonaPath(agentID))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// WritePersona replaces an agent's instructions. It does not snapshot a version;
// the caller does, so that a definition change and a persona change made
// together produce one version rather than two.
func (d *Dispatcher) WritePersona(agentID, content string) error {
	dir := d.cfg.WorkDir(agentID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(content), 0o640)
}

// ApproveProposal applies a pending proposal and records who approved it.
//
// The order — apply, then record — is deliberate. If applying fails the
// proposal is marked failed with the reason rather than left pending, because a
// person already said yes and a pending row would invite a second person to say
// yes to the same broken change.
func (d *Dispatcher) ApproveProposal(ctx context.Context, id int64, by, note string) (registry.Proposal, int, error) {
	p, err := d.reg.GetProposal(ctx, id)
	if err != nil {
		return registry.Proposal{}, 0, err
	}
	if p.Status != registry.ProposalPending {
		return p, 0, fmt.Errorf("%w: proposal %d was already %s", registry.ErrConflict, id, p.Status)
	}

	version, applyErr := d.applyProposal(ctx, p, by)
	if applyErr != nil {
		if _, err := d.reg.DecideProposal(ctx, id, registry.ProposalFailed, by, applyErr.Error(), 0); err != nil {
			slog.Error("could not record a failed approval", "proposal", id, "error", err)
		}
		return p, 0, applyErr
	}

	decided, err := d.reg.DecideProposal(ctx, id, registry.ProposalApplied, by, note, version)
	if err != nil {
		// The change is real even though the bookkeeping failed, and reporting a
		// clean error would leave somebody believing nothing happened.
		return decided, version, fmt.Errorf("the change was applied, but recording it failed: %w", err)
	}
	slog.Info("proposal applied", "proposal", id, "kind", p.Kind,
		"target", p.Target, "by", by, "version", version)
	return decided, version, nil
}

// RejectProposal closes a proposal without applying it.
func (d *Dispatcher) RejectProposal(ctx context.Context, id int64, by, note string) (registry.Proposal, error) {
	return d.reg.DecideProposal(ctx, id, registry.ProposalRejected, by, note, 0)
}

// applyProposal makes the change and returns the agent version it produced, if
// any.
//
// Every branch goes through the ordinary registry and persona calls rather than
// writing rows itself. That is what makes an approved change indistinguishable
// from a hand edit afterwards: same validation, same version snapshot, same
// rollback.
func (d *Dispatcher) applyProposal(ctx context.Context, p registry.Proposal, by string) (int, error) {
	change := registry.Change{
		Note:   fmt.Sprintf("proposal %d: %s", p.ID, p.Rationale),
		Author: by,
	}

	switch p.Kind {
	case registry.ProposeAgentCreate, registry.ProposeAgentUpdate:
		var body struct {
			registry.Agent
			// A pointer so that an absent field leaves the persona alone, while
			// an explicit "" clears it. A definition change that silently wiped
			// the instructions would be a disaster wearing a small diff.
			Persona *string `json:"persona"`
		}
		if err := json.Unmarshal(p.Payload, &body); err != nil {
			return 0, fmt.Errorf("payload is not an agent definition: %w", err)
		}
		body.Agent.ID = p.Target

		// The persona goes first, for the reason a rollback does it first: the
		// registry snapshots whatever the persona source reads at write time, so
		// the other order would mint a version pairing the new definition with
		// the old instructions — a combination that never existed.
		if body.Persona != nil {
			if err := d.WritePersona(p.Target, *body.Persona); err != nil {
				return 0, fmt.Errorf("write persona: %w", err)
			}
		}
		var err error
		if p.Kind == registry.ProposeAgentCreate {
			body.Agent.Enabled = true
			_, err = d.reg.Create(ctx, body.Agent, change)
		} else {
			_, err = d.reg.Update(ctx, body.Agent, change)
		}
		if err != nil {
			return 0, err
		}
		return d.latestVersion(ctx, p.Target), nil

	case registry.ProposePersonaUpdate:
		var body struct {
			Persona string `json:"persona"`
		}
		if err := json.Unmarshal(p.Payload, &body); err != nil {
			return 0, fmt.Errorf(`payload must be {"persona": "..."}: %w`, err)
		}
		if err := d.WritePersona(p.Target, body.Persona); err != nil {
			return 0, fmt.Errorf("write persona: %w", err)
		}
		v, err := d.reg.Snapshot(ctx, p.Target, change)
		if err != nil {
			return 0, err
		}
		return v.Version, nil

	case registry.ProposeAgentDelete:
		// Stopped before deleted, for the same reason the delete endpoint does
		// it: the workflows outlive the definition, and queued work would
		// otherwise run on and fail one turn at a time with "unknown agent".
		if err := d.StopAll(ctx, p.Target, "agent deleted by proposal"); err != nil {
			slog.Info("could not stop an agent before deleting it", "agent", p.Target, "error", err)
		}
		if err := d.reg.Delete(ctx, p.Target); err != nil {
			return 0, err
		}
		// Version history outlives the definition on purpose, so the version
		// before the delete is still there to recreate from.
		return d.latestVersion(ctx, p.Target), nil

	case registry.ProposeEvalCaseAdd:
		var body struct {
			AgentID string              `json:"agent_id"`
			Cases   []registry.EvalCase `json:"cases"`
		}
		if err := json.Unmarshal(p.Payload, &body); err != nil {
			return 0, fmt.Errorf(`payload must be {"agent_id": "...", "cases": [...]}: %w`, err)
		}
		if len(body.Cases) == 0 {
			return 0, errors.New("the proposal adds no cases")
		}

		set, err := d.reg.GetEvalSet(ctx, p.Target)
		switch {
		case errors.Is(err, registry.ErrNotFound):
			set = registry.EvalSet{ID: p.Target, AgentID: body.AgentID, Enabled: true}
		case err != nil:
			return 0, err
		}
		// Appended, never replacing. A proposal that could drop existing cases
		// would let the thing being measured quietly narrow its own exam.
		existing := make(map[string]bool, len(set.Cases))
		for _, c := range set.Cases {
			existing[c.Name] = true
		}
		for _, c := range body.Cases {
			if existing[c.Name] {
				return 0, fmt.Errorf("case %q is already in %s; propose a rewrite explicitly instead",
					c.Name, p.Target)
			}
			set.Cases = append(set.Cases, c)
		}
		if _, err := d.reg.PutEvalSet(ctx, set); err != nil {
			return 0, err
		}
		return 0, nil

	default:
		return 0, fmt.Errorf("unknown proposal kind %q", p.Kind)
	}
}

// latestVersion reports the version a change produced, or 0 if it cannot be
// read. Used only for the undo hint, so a failure here must not fail an
// application that already succeeded.
func (d *Dispatcher) latestVersion(ctx context.Context, agentID string) int {
	v, err := d.reg.LatestVersion(ctx, agentID)
	if err != nil {
		return 0
	}
	return v.Version
}

// UndoHint is the command that reverses an applied proposal, or "" when there is
// nothing to roll back to.
func UndoHint(p registry.Proposal, version int) string {
	if version > 1 {
		return fmt.Sprintf("roundclaw version rollback %s %d", p.Target, version-1)
	}
	return ""
}

// ProposalSummary renders a proposal for a human deciding on it: what it does,
// why, and what backs it up.
func ProposalSummary(p registry.Proposal) string {
	s := fmt.Sprintf("**#%d — %s `%s`**\n%s", p.ID, p.Kind, p.Target, p.Rationale)
	if len(p.Evidence) > 0 {
		s += "\n_evidence:_ "
		for i, e := range p.Evidence {
			if i > 0 {
				s += ", "
			}
			s += e
		}
	}
	if p.CreatedBy != "" {
		s += "\n_proposed by " + p.CreatedBy + "_"
	}
	return s
}
