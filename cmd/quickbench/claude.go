package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/roundtable/roundclaw/internal/claude"
	"github.com/roundtable/roundclaw/internal/core"
)

// subAgentTools are the names the CLI has given its sub-agent tool. Refusing
// both is what "sub-agents refused" means, whichever the pinned CLI uses.
var subAgentTools = []string{"Task", "Agent"}

// claudeRun is what one plain `claude -p` produced.
type claudeRun struct {
	Text     string
	IsError  bool
	CostUSD  float64
	Usage    core.Usage
	WallMS   int64
	Handoffs int
	// ReturnedChars is the size of what sub-agents handed back to the parent:
	// the text of each sub-agent tool result, which stays in its context.
	ReturnedChars int
}

// runClaude runs Claude Code directly in the agent image, with the same argv
// roundclaw builds for a turn, so the only difference from a roundclaw agent is
// roundclaw itself.
func (b *bench) runClaude(ctx context.Context, name, dir, prompt string, subAgents bool) (claudeRun, error) {
	home := dir + "-home"
	for _, d := range []string{dir, home} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return claudeRun{}, fmt.Errorf("create %s: %w", d, err)
		}
	}
	spec := claude.RunSpec{
		Runtime: b.cfg.Container.Runtime, Image: b.cfg.Container.Image,
		ContainerName: "quickbench-" + b.runID + "-" + name,
		WorkDir:       dir, ClaudeHome: home,
		CredentialEnv: b.cred.EnvName, CredentialValue: b.cred.Value,
		SessionID:      claude.SessionID("quickbench-" + b.runID + "-" + name),
		PermissionMode: permissionMode,
		Model:          b.cfg.Container.Model,
		Network:        b.cfg.Container.Network,
		Prompt:         prompt,
	}
	if !subAgents {
		spec.DisallowedTools = subAgentTools
	}
	args, err := spec.Args()
	if err != nil {
		return claudeRun{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, spec.Runtime, args...)
	cmd.Env = append(os.Environ(), spec.CredentialEnv+"="+spec.CredentialValue)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return claudeRun{}, err
	}
	started := time.Now()
	if err := cmd.Start(); err != nil {
		return claudeRun{}, fmt.Errorf("start container: %w", err)
	}

	var out claudeRun
	sawResult := false
	subCalls := map[string]bool{}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		out.ReturnedChars += subAgentReturn(line, subCalls)
		events, err := claude.Decode(line)
		if err != nil {
			continue // a line the decoder does not know is not a failed run
		}
		for _, ev := range events {
			if ev.Kind == claude.KindResult {
				sawResult = true
				out.Text, out.IsError, out.CostUSD, out.Usage = ev.Text, ev.IsError, ev.CostUSD, ev.Usage
			}
		}
	}
	waitErr := cmd.Wait()
	out.WallMS = time.Since(started).Milliseconds()
	out.Handoffs = len(subCalls)
	if ctx.Err() != nil {
		// The container outlives a killed client; remove it so the name is free.
		_ = exec.Command(spec.Runtime, spec.RemoveArgs()...).Run()
		return out, fmt.Errorf("timed out after %s", b.timeout)
	}
	if !sawResult {
		return out, fmt.Errorf("claude produced no result (%v): %s", waitErr, firstLine(stderr.String(), 200))
	}
	return out, nil
}

// subAgentReturn reads one stream-json line for sub-agent traffic: it notes the
// id of every sub-agent call, and returns the size of any result one of those
// calls handed back. The stream decoder keeps only a truncated preview of a
// tool result, which is why the size is read from the line itself.
func subAgentReturn(line []byte, calls map[string]bool) int {
	var w struct {
		Message struct {
			Content []struct {
				Type      string          `json:"type"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &w) != nil {
		return 0
	}
	n := 0
	for _, c := range w.Message.Content {
		switch c.Type {
		case "tool_use":
			for _, name := range subAgentTools {
				if c.Name == name {
					calls[c.ID] = true
				}
			}
		case "tool_result":
			if calls[c.ToolUseID] {
				n += len([]rune(resultText(c.Content)))
			}
		}
	}
	return n
}

// resultText flattens a tool result, which is either a string or a list of
// text blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, bl := range blocks {
		b.WriteString(bl.Text)
	}
	return b.String()
}

// runCheck runs a task's check in the agent image, with no network, against
// the directory the work was done in, and returns what it printed.
func (b *bench) runCheck(ctx context.Context, dir, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, b.cfg.Container.Runtime, "run", "--rm", "--network", "none",
		"-v", abs+":"+claude.ContainerWorkspace, "--workdir", claude.ContainerWorkspace,
		"--entrypoint", "sh", b.cfg.Container.Image, "-c", command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%v: %s", err, firstLine(stderr.String(), 160))
	}
	return stdout.String(), nil
}
