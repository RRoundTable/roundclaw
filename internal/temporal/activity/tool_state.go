package activity

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"

	"github.com/roundtable/roundclaw/internal/registry"
)

// Putting an agent's tools back, and telling it what it actually got.
//
// A tool is the agent's surrounding dependencies, and some of them hold state
// that does not survive an interruption. Before this, an agent discovered its
// database was gone by failing a query mid-turn, which costs the turn — the
// expensive part — to learn something that could have been checked in a second.
//
// Two rules shape everything here. Nothing declared is treated as unrestorable,
// because the dangerous default is the optimistic one. And the agent is told the
// truth in every case: usable, restored, drifted, or unavailable with a reason —
// never that a capability works when it does not.

// ToolCondition is what an agent is told about one of its tools.
type ToolCondition string

const (
	// ToolUsable — the tool answered, or declared nothing to check.
	ToolUsable ToolCondition = "usable"
	// ToolRestored — it was down, roundclaw started it, and it answered.
	ToolRestored ToolCondition = "restored"
	// ToolDrifted — it answers, but it is not what its version says it is, so
	// nothing measured against that version can be trusted to compare.
	ToolDrifted ToolCondition = "drifted"
	// ToolUnavailable — it does not answer and roundclaw could not fix it. The
	// grant still stands; the tool is down, and those are different things.
	ToolUnavailable ToolCondition = "unavailable"
)

// ToolState is one tool's condition at the start of a session.
type ToolState struct {
	ID        string
	Condition ToolCondition
	Reason    string
}

// defaultReachabilityBound is how long a check waits when a tool named no bound.
// Session start now does I/O, and an unreachable tool must not be able to delay
// work indefinitely: exceeding this reports unavailable rather than blocking.
const defaultReachabilityBound = 10 * time.Second

// resolveToolStates checks every tool an agent holds, restoring what it can, and
// returns what to tell the agent.
//
// It never fails a turn. A tool being down is something the agent works around
// with the knowledge that it is down; refusing the work instead would let one
// dead dependency stop everything, which is a larger failure than the dependency.
func (a *Activities) resolveToolStates(ctx context.Context, tools []registry.Tool) []ToolState {
	log := activity.GetLogger(ctx)
	states := make([]ToolState, 0, len(tools))

	for _, t := range tools {
		st := ToolState{ID: t.ID, Condition: ToolUsable}

		if t.Reachability.Declared() {
			bound := time.Duration(t.Reachability.WithinSeconds) * time.Second
			if bound <= 0 {
				bound = defaultReachabilityBound
			}
			if err := probe(ctx, t.Reachability, bound); err != nil {
				st = a.restore(ctx, t, bound, err)
			}
		}

		// Drift is checked even on a tool that answers: a service can be up and
		// still not be the one the version recorded. It does not override
		// unavailable, because "gone" is the more urgent thing to say.
		if st.Condition != ToolUnavailable {
			if d, err := a.reg.ToolDriftOf(ctx, t.ID); err != nil {
				log.Warn("could not check tool identity", "tool", t.ID, "error", err)
			} else if d.Declared && !d.Matches {
				st.Condition = ToolDrifted
				st.Reason = driftReason(d)
			}
		}

		if st.Condition != ToolUsable {
			log.Info("tool is not simply usable at session start",
				"tool", t.ID, "condition", string(st.Condition), "reason", st.Reason)
		}
		states = append(states, st)
	}
	return states
}

// restore does the one structured thing roundclaw is willing to do about a tool
// being down: start the container the tool named, then wait for the condition it
// declared. Anything else is unrestorable by declaration and reported as such.
func (a *Activities) restore(ctx context.Context, t registry.Tool, bound time.Duration, probeErr error) ToolState {
	if !t.Reachability.Restorable() {
		return ToolState{ID: t.ID, Condition: ToolUnavailable, Reason: probeErr.Error()}
	}

	runtime := a.cfg.Container.Runtime
	startCtx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	if out, err := exec.CommandContext(startCtx, runtime, "start", t.Reachability.Container).CombinedOutput(); err != nil {
		return ToolState{ID: t.ID, Condition: ToolUnavailable,
			Reason: fmt.Sprintf("could not start %s: %v: %s", t.Reachability.Container, err, strings.TrimSpace(string(out)))}
	}

	// Started is not the same as ready. The declared condition is what "ready"
	// means, and waiting for it is the difference between handing the agent a
	// working database and handing it one that is still opening its files.
	if err := waitFor(ctx, t.Reachability, bound); err != nil {
		return ToolState{ID: t.ID, Condition: ToolUnavailable,
			Reason: fmt.Sprintf("%s started but did not become reachable: %v", t.Reachability.Container, err)}
	}
	return ToolState{ID: t.ID, Condition: ToolRestored}
}

// waitFor retries the probe until the bound expires, so a container that takes a
// moment to accept connections is restored rather than reported dead.
func waitFor(ctx context.Context, r registry.Reachability, bound time.Duration) error {
	deadline := time.Now().Add(bound)
	var last error
	for {
		if last = probe(ctx, r, time.Until(deadline)); last == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return last
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// probe checks every condition the tool declared. All of them must hold: a tool
// that named a container and an address is saying both, and answering half of it
// is not being reachable.
func probe(ctx context.Context, r registry.Reachability, bound time.Duration) error {
	if bound <= 0 {
		return fmt.Errorf("no time left to check")
	}
	if r.File != "" {
		if err := statFile(r.File); err != nil {
			return err
		}
	}
	if r.Address != "" {
		d := net.Dialer{Timeout: bound}
		conn, err := d.DialContext(ctx, "tcp", r.Address)
		if err != nil {
			return fmt.Errorf("%s did not accept a connection: %w", r.Address, err)
		}
		conn.Close()
	}
	if r.Endpoint != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.Endpoint, nil)
		if err != nil {
			return fmt.Errorf("%s is not a usable endpoint: %w", r.Endpoint, err)
		}
		resp, err := (&http.Client{Timeout: bound}).Do(req)
		if err != nil {
			return fmt.Errorf("%s did not answer: %w", r.Endpoint, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("%s answered %d", r.Endpoint, resp.StatusCode)
		}
	}
	return nil
}

func driftReason(d registry.ToolDrift) string {
	if d.Reason != "" {
		return "its identity could not be read: " + d.Reason
	}
	if d.Recorded == "" {
		return "its newest version recorded no identity, so there is nothing to compare against"
	}
	return "it does not match the identity its newest version recorded"
}

// toolStateNote is what the agent reads before it does anything.
//
// Ordered and stable so the same fleet state reads the same way twice, and led by
// the tools that are not simply fine — the point is that the agent knows about
// the dead database before it writes a query, not that it reads a status table.
func toolStateNote(states []ToolState) string {
	var bad []ToolState
	for _, s := range states {
		if s.Condition != ToolUsable {
			bad = append(bad, s)
		}
	}
	if len(bad) == 0 {
		return ""
	}
	sort.Slice(bad, func(i, j int) bool { return bad[i].ID < bad[j].ID })

	var b strings.Builder
	b.WriteString("Before you start, the state of your tools:\n\n")
	for _, s := range bad {
		switch s.Condition {
		case ToolRestored:
			fmt.Fprintf(&b, "- **%s** was down and has been restarted. Its state is whatever actually came back, which may be less than you left.\n", s.ID)
		case ToolDrifted:
			fmt.Fprintf(&b, "- **%s** works, but %s. Treat results from it as not comparable with earlier ones.\n", s.ID, s.Reason)
		case ToolUnavailable:
			fmt.Fprintf(&b, "- **%s** is UNAVAILABLE: %s. You still hold it — it is down, not withdrawn. Do the work you can without it and say what you could not do.\n", s.ID, s.Reason)
		}
	}
	b.WriteString("\n---\n\n")
	return b.String()
}

// statFile is split out so the probe reads as one list of conditions.
func statFile(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s is not there: %w", path, err)
	}
	return nil
}
