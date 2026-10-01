package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/workspace"
)

// permissionMode is what every system runs under. Nobody is there to answer a
// prompt, in a container or in a benchmark.
const permissionMode = "bypassPermissions"

const (
	soloAgent   = "qb-solo"   // system C, and the agent the bench routines' steps run on
	callerAgent = "qb-caller" // system D
	apiTool     = "qb-api"
	callerSkill = "roundclaw-routines"
)

// fleet is the roundclaw API, as an operator reaches it.
type fleet struct {
	base  string
	token string
}

func (f *fleet) call(ctx context.Context, method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.base+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, firstLine(string(raw), 300))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: decode: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

type fleetTurn struct {
	TurnID  int64      `json:"turn_id"`
	Status  string     `json:"status"`
	Request string     `json:"request"`
	Result  string     `json:"result"`
	Error   string     `json:"error"`
	CostUSD float64    `json:"cost_usd"`
	Usage   core.Usage `json:"usage"`
}

func (t fleetTurn) finished() bool {
	return t.Status != string(core.TurnRunning) && t.Status != "queued"
}

type runView struct {
	Run   registry.RoutineRun `json:"run"`
	Steps []registry.StepRun  `json:"steps"`
}

func runFinished(status string) bool { return status != registry.RunRunning }

// setup registers what the checks need: the two agents, the tool that tells an
// agent where the API is, the caller skill, and the bench routines. Every write
// is a put, so a second run finds them in place.
func (b *bench) setup(ctx context.Context) error {
	toolDir := filepath.Join(b.cfg.WorkspaceRoot, "quickbench", "tool")
	if err := os.MkdirAll(toolDir, 0o750); err != nil {
		return err
	}
	if _, err := b.fleet.call(ctx, http.MethodPut, "/v1/tools/"+apiTool, map[string]any{
		"id": apiTool, "description": "where the roundclaw API is, for quickbench",
		"host_path": toolDir, "env": map[string]string{"ROUNDCLAW_URL": b.agentURL},
	}, nil); err != nil {
		return err
	}
	if _, err := b.fleet.call(ctx, http.MethodPut, "/v1/skills/"+callerSkill, map[string]any{
		"id": callerSkill, "host_path": filepath.Join(b.skills, callerSkill),
	}, nil); err != nil {
		return err
	}
	agents := []map[string]any{
		{"id": soloAgent, "description": "quickbench: an agent with no skills", "permission_mode": permissionMode},
		{"id": callerAgent, "description": "quickbench: an agent that hands known work to routines",
			"permission_mode": permissionMode, "tools": []string{apiTool}, "skills": []string{callerSkill}},
	}
	for _, def := range agents {
		id := def["id"].(string)
		status, err := b.fleet.call(ctx, http.MethodPost, "/v1/agents", def, nil)
		if status == http.StatusConflict {
			def["enabled"] = true
			_, err = b.fleet.call(ctx, http.MethodPut, "/v1/agents/"+id+"/definition", def, nil)
		}
		if err != nil {
			return err
		}
	}
	for _, src := range benchRoutines {
		if _, err := b.fleet.call(ctx, http.MethodPut, "/v1/routines/"+src["id"].(string), src, nil); err != nil {
			return err
		}
	}
	return nil
}

// benchRoutines are the routines the delegation, context and durability checks
// hand work to. Each is one step on the skill-less agent, so what a hand-off
// costs is the hand-off and not a cleverer worker.
var benchRoutines = []map[string]any{
	{
		// The procedure lives here and nowhere else: a request names the work
		// ("make the word ledger") and the routine is what knows how.
		"id": "qb-ledger",
		"contract": map[string]any{
			"intent": "make the word ledger for one word and report the ledger's checksum",
			"inputs": []string{"word"}, "done": map[string]any{"must_contain": []string{"ledger"}}, "effects": "workspace",
		},
		"steps": []map[string]any{{
			"id": "ledger", "kind": "agent", "agent": soloAgent, "effects": "workspace",
			"task": "Do exactly this with the Bash tool, in the current directory:\n" +
				"1. mkdir -p ledger/{{inputs.word}}\n" +
				"2. For each letter of '{{inputs.word}}', in order, numbered from 1: write the file ledger/{{inputs.word}}/NN.txt " +
				"(NN is the number as two digits) holding that letter repeated NN times, then a newline.\n" +
				"3. cat ledger/{{inputs.word}}/*.txt > ledger/{{inputs.word}}.all\n" +
				"4. sha256sum ledger/{{inputs.word}}.all | cut -c1-12\n" +
				"Reply with exactly one line: '{{inputs.word}} ledger <the 12 characters step 4 printed>'.",
		}},
	},
	{
		"id": "qb-long",
		"contract": map[string]any{
			"intent": "list numbered entries from 1 to n, one per line",
			"inputs": []string{"n"}, "done": map[string]any{"must_contain": []string{"entry 1"}}, "effects": "none",
		},
		"steps": []map[string]any{{
			"id": "list", "kind": "agent", "agent": soloAgent,
			"task": "Reply with the lines 'entry 1' through 'entry {{inputs.n}}', one per line, every line written out, and nothing else.",
			"done": map[string]any{"must_contain": []string{"entry {{inputs.n}}"}},
		}},
	},
	{
		"id": "qb-slow",
		"contract": map[string]any{
			"intent": "wait a number of seconds and confirm",
			"inputs": []string{"seconds"}, "done": map[string]any{"must_contain": []string{"slept ok"}}, "effects": "none",
		},
		"steps": []map[string]any{{
			"id": "sleep", "kind": "agent", "agent": soloAgent, "max_attempts": 1,
			// A loop, not `sleep N`: the CLI refuses a long sleep in the foreground.
			"task": "Run exactly this with the Bash tool, once, in the foreground: " +
				"for i in $(seq 1 {{inputs.seconds}}); do sleep 1; done; echo finished\n" +
				"When it prints finished, reply with exactly: slept ok",
			"done": map[string]any{"must_contain": []string{"slept ok"}},
		}},
	},
}

// conversationDir is where a roundclaw agent works for one conversation.
func (b *bench) conversationDir(agent, conversation string) string {
	return workspace.ConversationDir(b.cfg, agent, conversation)
}

// submit queues a request for an agent in a conversation.
func (b *bench) submit(ctx context.Context, agent, conversation, text string) (int64, error) {
	var out struct {
		TurnID int64 `json:"turn_id"`
	}
	_, err := b.fleet.call(ctx, http.MethodPost, "/v1/agents/"+agent+"/requests",
		map[string]any{"conversation_id": conversation, "text": text}, &out)
	return out.TurnID, err
}

// turns returns a conversation's turns, oldest first, in full.
func (b *bench) turns(ctx context.Context, agent, conversation string) ([]fleetTurn, error) {
	var out struct {
		Turns []fleetTurn `json:"turns"`
	}
	q := url.Values{"conversation": {conversation}, "full": {"true"}, "limit": {"50"}}
	if _, err := b.fleet.call(ctx, http.MethodGet, "/v1/agents/"+agent+"/turns?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	for i, j := 0, len(out.Turns)-1; i < j; i, j = i+1, j-1 {
		out.Turns[i], out.Turns[j] = out.Turns[j], out.Turns[i]
	}
	return out.Turns, nil
}

// runsFrom returns the routine runs an agent started from a conversation: its
// hand-offs. roundclaw records the return address on the run, so this reads
// what happened and does not rely on what the agent says it did.
func (b *bench) runsFrom(ctx context.Context, agent, conversation string) ([]registry.RoutineRun, error) {
	var list struct {
		Routines []struct {
			ID string `json:"id"`
		} `json:"routines"`
	}
	if _, err := b.fleet.call(ctx, http.MethodGet, "/v1/routines", nil, &list); err != nil {
		return nil, err
	}
	var found []registry.RoutineRun
	for _, rt := range list.Routines {
		var out struct {
			Runs []registry.RoutineRun `json:"runs"`
		}
		q := url.Values{"requested_by": {agent}, "limit": {"50"}}
		if _, err := b.fleet.call(ctx, http.MethodGet, "/v1/routines/"+rt.ID+"/runs?"+q.Encode(), nil, &out); err != nil {
			return nil, err
		}
		for _, r := range out.Runs {
			if r.Origin.Agent == agent && r.Origin.Conversation == conversation {
				found = append(found, r)
			}
		}
	}
	return found, nil
}

func (b *bench) getRun(ctx context.Context, id int64) (runView, error) {
	var v runView
	_, err := b.fleet.call(ctx, http.MethodGet, fmt.Sprintf("/v1/routine-runs/%d", id), nil, &v)
	return v, err
}

// fleetWork is everything one request to a roundclaw agent led to: its own
// turns, the routine runs it started, and the turns those runs woke it for.
type fleetWork struct {
	Turns   []fleetTurn
	Runs    []registry.RoutineRun
	Final   string
	CostUSD float64
	Usage   core.Usage
	WallMS  int64
}

// ask sends a request and follows it to the end: until every turn of the
// conversation has finished, every routine run it started has finished, and
// each of those runs has woken the agent. A hand-off is asynchronous — the
// first turn ends when the work is handed over — so the answer to the request
// is the last turn's, not the first's.
func (b *bench) ask(ctx context.Context, agent, conversation, text string) (fleetWork, error) {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	started := time.Now()
	if _, err := b.submit(ctx, agent, conversation, text); err != nil {
		return fleetWork{}, err
	}
	var w fleetWork
	for {
		select {
		case <-ctx.Done():
			return w, fmt.Errorf("timed out after %s", b.timeout)
		case <-time.After(2 * time.Second):
		}
		turns, err := b.turns(ctx, agent, conversation)
		if err != nil {
			return w, err
		}
		runs, err := b.runsFrom(ctx, agent, conversation)
		if err != nil {
			return w, err
		}
		w.Turns, w.Runs = turns, runs
		settled := len(turns) >= 1+len(runs)
		for _, t := range turns {
			settled = settled && t.finished()
		}
		for _, r := range runs {
			settled = settled && runFinished(r.Status)
		}
		if settled {
			break
		}
	}
	w.WallMS = time.Since(started).Milliseconds()
	for _, t := range w.Turns {
		w.CostUSD += t.CostUSD
		w.Usage.Add(t.Usage)
	}
	for _, r := range w.Runs {
		w.CostUSD += r.CostUSD
		w.Usage.Add(r.Usage)
	}
	last := w.Turns[len(w.Turns)-1]
	w.Final = last.Result
	if last.Status != string(core.TurnDone) {
		return w, fmt.Errorf("turn %d ended %s: %s", last.TurnID, last.Status, firstLine(last.Error, 160))
	}
	return w, nil
}
