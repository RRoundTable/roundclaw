// Command roundclaw is the terminal client for a running roundclaw gateway.
//
// It is a thin HTTP client: everything it does goes through the same REST API
// that any other caller uses, authenticated with a bearer token. It manages
// agents and secrets, sends requests, and reads status — the same surface as
// the Discord commands, without Discord.
//
// Configuration comes from the environment, overridable per invocation:
//
//	ROUNDCLAW_URL        base URL of the gateway (default http://127.0.0.1:8099)
//	ROUNDCLAW_API_TOKEN  bearer token (one of http.tokens_env)
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "agents":
		return cmdAgents(rest)
	case "agent":
		return cmdAgent(rest)
	case "send":
		return cmdSend(rest)
	case "status":
		return cmdStatus(rest)
	case "turn":
		return cmdTurn(rest)
	case "secret":
		return cmdSecret(rest)
	case "tool":
		return cmdTool(rest)
	case "skill":
		return cmdSkill(rest)
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `roundclaw — terminal client for a running gateway

Usage:
  roundclaw <command> [flags]

Commands:
  agents                       list agents
  agent show <id>              print an agent's definition
  agent rm <id>                delete an agent (workspace kept)
  send <agent> <text>          delegate a request; waits for the result by default
                               (--no-wait to just queue), --timeout, --steer, --callback, --key
  status <agent>               what an agent is doing right now
  turn <agent> <turn-id>       a turn's state and result

  secret set <name> [value]    store a secret (value from stdin if omitted)
  secret ls                    list secret names (never values)
  secret rm <name>             delete a secret
      add --agent <id> to any secret command to scope it to one agent;
      without it the secret is global (every agent sees it)

  tool set <id> --path P       register a local tool (a dir + the env it needs)
      --env NAME=VALUE (repeatable), --desc TEXT, --instructions TEXT (or - for stdin)
  tool ls                      list registered tools
  tool show <id>               print a tool's definition
  tool rm <id>                 delete a tool
      grant a tool to an agent from Discord admin: "dev에 outline 붙여줘"

  skill set <id> --path P      register a Claude Code skill (a SKILL.md directory)
  skill ls                     list registered skills
  skill show <id>              print a skill's definition
  skill rm <id>                delete a skill
      grant a skill by adding its id to an agent's "skills" list

Environment:
  ROUNDCLAW_URL        gateway base URL (default http://127.0.0.1:8099)
  ROUNDCLAW_API_TOKEN  bearer token
  --url, --token       override either per command
`)
}

// ---- HTTP client -----------------------------------------------------------

type client struct {
	base  string
	token string
	http  *http.Client
}

// commonFlags registers --url and --token on a flag set, defaulting to the
// environment, so every subcommand accepts them.
func commonFlags(fs *flag.FlagSet) (*string, *string) {
	base := fs.String("url", envOr("ROUNDCLAW_URL", "http://127.0.0.1:8099"), "gateway base URL")
	token := fs.String("token", os.Getenv("ROUNDCLAW_API_TOKEN"), "bearer token")
	return base, token
}

func newClient(base, token string) *client {
	return &client{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http:  &http.Client{Timeout: 5 * time.Minute},
	}
}

// do sends a request and decodes the JSON response into out (may be nil). A
// non-2xx status becomes an error carrying the server's message.
func (c *client) do(method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", resp.Status, serverMessage(raw))
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func serverMessage(raw []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error != "" {
		return e.Error
	}
	return strings.TrimSpace(string(raw))
}

// fail prints an error and returns the process exit code.
func fail(err error) int {
	fmt.Fprintln(os.Stderr, "error:", err)
	return 1
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// ---- commands --------------------------------------------------------------

func cmdAgents(args []string) int {
	fs := flag.NewFlagSet("agents", flag.ContinueOnError)
	base, token := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var out struct {
		Agents []struct {
			ID          string `json:"id"`
			Description string `json:"description"`
			Enabled     bool   `json:"enabled"`
		} `json:"agents"`
	}
	if err := newClient(*base, *token).do(http.MethodGet, "/v1/agents", nil, &out); err != nil {
		return fail(err)
	}
	if len(out.Agents) == 0 {
		fmt.Println("no agents")
		return 0
	}
	for _, a := range out.Agents {
		state := "enabled"
		if !a.Enabled {
			state = "disabled"
		}
		fmt.Printf("%-24s %-9s %s\n", a.ID, state, a.Description)
	}
	return 0
}

func cmdAgent(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: roundclaw agent <show|rm> <id>")
		return 2
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	base, token := commonFlags(fs)
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: roundclaw agent <show|rm> <id>")
		return 2
	}
	id := fs.Arg(0)
	c := newClient(*base, *token)
	switch sub {
	case "show":
		var out map[string]any
		if err := c.do(http.MethodGet, "/v1/agents/"+id+"/definition", nil, &out); err != nil {
			return fail(err)
		}
		printJSON(out)
		return 0
	case "rm":
		if err := c.do(http.MethodDelete, "/v1/agents/"+id, nil, nil); err != nil {
			return fail(err)
		}
		fmt.Printf("deleted %s (workspace and session kept)\n", id)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown agent subcommand %q\n", sub)
		return 2
	}
}

func cmdSend(args []string) int {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	base, token := commonFlags(fs)
	// Waiting is the default. Delegation is the main use of send, and a
	// delegating agent almost always needs the result to continue. When wait was
	// opt-in, an agent that forgot the flag fired and forgot: the delegated turn
	// ran to completion but its result was recorded and never returned, so the
	// caller was left promising a follow-up it had no way to deliver.
	wait := fs.Bool("wait", true, "hold until the turn finishes, polling past the server's wait budget (default)")
	noWait := fs.Bool("no-wait", false, "queue the turn and return its id immediately, without waiting")
	timeout := fs.Duration("timeout", 30*time.Minute, "give up waiting after this long; the turn keeps running and can be read with `turn`")
	steer := fs.Bool("steer", false, "interrupt the running turn instead of queueing")
	callback := fs.String("callback", "", "URL to POST the result to when it finishes")
	key := fs.String("key", "", "idempotency key (a retry with the same key is one turn)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "usage: roundclaw send <agent> <text> [--wait|--no-wait] [--timeout D] [--steer] [--callback URL] [--key K]")
		return 2
	}
	waiting := *wait && !*noWait
	agent := fs.Arg(0)
	text := strings.Join(fs.Args()[1:], " ")

	body := map[string]any{"text": text}
	if *callback != "" {
		body["callback_url"] = *callback
	}
	if *steer {
		body["steer"] = true
	}

	path := "/v1/agents/" + agent + "/requests"
	if waiting {
		path += "?wait=true"
	}
	c := newClient(*base, *token)

	req, err := http.NewRequest(http.MethodPost, c.base+path, jsonReader(body))
	if err != nil {
		return fail(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if *key != "" {
		req.Header.Set("Idempotency-Key", *key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fail(fmt.Errorf("%s: %s", resp.Status, serverMessage(raw)))
	}

	var out struct {
		TurnID        int64   `json:"turn_id"`
		Status        string  `json:"status"`
		QueuePosition int     `json:"queue_position"`
		Duplicate     bool    `json:"duplicate"`
		Result        string  `json:"result"`
		CostUSD       float64 `json:"cost_usd"`
		Error         string  `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fail(err)
	}

	if !waiting {
		dup := ""
		if out.Duplicate {
			dup = " (duplicate — same key already ran)"
		}
		fmt.Printf("turn %d, %s, queue position %d%s\n", out.TurnID, out.Status, out.QueuePosition, dup)
		return 0
	}

	// Waiting. The server returns the finished turn when it completes inside its
	// wait budget (HTTP 200); otherwise it demotes to 202 with the turn still
	// running, and finishing the wait is ours. A busy agent shares its queue with
	// Discord, so a delegated turn can take many minutes — longer than any single
	// held connection should live, which is why the rest is done by polling.
	status, result, errMsg, cost := out.Status, out.Result, out.Error, out.CostUSD
	if !turnFinished(status) {
		t, ok := pollTurn(c, agent, out.TurnID, *timeout)
		if !ok {
			fmt.Printf("turn %d still running after %s — read it later with: roundclaw turn %s %d\n",
				out.TurnID, *timeout, agent, out.TurnID)
			return 1
		}
		status, result, errMsg, cost = t.Status, t.Result, t.Error, t.CostUSD
	}

	fmt.Printf("turn %d, %s (cost $%.4f)\n", out.TurnID, status, cost)
	if result != "" {
		fmt.Printf("\n%s\n", result)
	}
	if errMsg != "" {
		fmt.Fprintf(os.Stderr, "\nturn error: %s\n", errMsg)
		return 1
	}
	return 0
}

// turnState is the slice of GET /turns/{id} that a waiting caller needs.
type turnState struct {
	Status  string  `json:"status"`
	Result  string  `json:"result"`
	Error   string  `json:"error"`
	CostUSD float64 `json:"cost_usd"`
}

// turnFinished reports whether a turn has left the running state. A turn is
// created running and moves to done, stopped, or error; the 202 wait-demotion
// response reports "queued", which is likewise not yet finished.
func turnFinished(status string) bool {
	switch status {
	case "", "running", "queued":
		return false
	default:
		return true
	}
}

// pollTurn waits for a turn to finish, returning its final record. It polls the
// turn by id rather than holding one long connection: a turn can run for many
// minutes, and no single request should be expected to stay open that long. A
// transient read error is retried rather than abandoning a turn already running.
func pollTurn(c *client, agent string, turnID int64, timeout time.Duration) (turnState, bool) {
	deadline := time.Now().Add(timeout)
	path := fmt.Sprintf("/v1/agents/%s/turns/%d", agent, turnID)
	for {
		var t turnState
		if err := c.do(http.MethodGet, path, nil, &t); err == nil && turnFinished(t.Status) {
			return t, true
		}
		if time.Now().After(deadline) {
			return turnState{}, false
		}
		time.Sleep(pollInterval)
	}
}

// pollInterval is how long pollTurn waits between reads. It is a variable so
// tests can shrink it; in production the turn takes far longer than this.
var pollInterval = 2 * time.Second

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	base, token := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: roundclaw status <agent>")
		return 2
	}
	var out map[string]any
	if err := newClient(*base, *token).do(http.MethodGet, "/v1/agents/"+fs.Arg(0), nil, &out); err != nil {
		return fail(err)
	}
	printJSON(out)
	return 0
}

func cmdTurn(args []string) int {
	fs := flag.NewFlagSet("turn", flag.ContinueOnError)
	base, token := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "usage: roundclaw turn <agent> <turn-id>")
		return 2
	}
	var out map[string]any
	path := "/v1/agents/" + fs.Arg(0) + "/turns/" + fs.Arg(1)
	if err := newClient(*base, *token).do(http.MethodGet, path, nil, &out); err != nil {
		return fail(err)
	}
	printJSON(out)
	return 0
}

func cmdSecret(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: roundclaw secret <set|ls|rm> [--agent id] ...")
		return 2
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("secret", flag.ContinueOnError)
	base, token := commonFlags(fs)
	agent := fs.String("agent", "", "scope the secret to one agent (default: global)")
	value := fs.String("value", "", "secret value (set only; omit to read from stdin)")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	c := newClient(*base, *token)
	prefix := "/v1/secrets"
	if *agent != "" {
		prefix = "/v1/agents/" + *agent + "/secrets"
	}

	switch sub {
	case "ls":
		var out struct {
			Secrets []struct {
				Name      string `json:"name"`
				UpdatedAt string `json:"updated_at"`
			} `json:"secrets"`
		}
		if err := c.do(http.MethodGet, prefix, nil, &out); err != nil {
			return fail(err)
		}
		if len(out.Secrets) == 0 {
			fmt.Println("no secrets")
			return 0
		}
		for _, s := range out.Secrets {
			fmt.Printf("%-32s updated %s\n", s.Name, s.UpdatedAt)
		}
		return 0
	case "set":
		if fs.NArg() < 1 {
			fmt.Fprintln(os.Stderr, "usage: roundclaw secret set <name> [value] [--agent id]")
			return 2
		}
		name := fs.Arg(0)
		val := *value
		if val == "" && fs.NArg() >= 2 {
			val = fs.Arg(1)
		}
		if val == "" {
			// Read from stdin, so the value never lands in shell history or the
			// process table.
			raw, err := io.ReadAll(os.Stdin)
			if err != nil {
				return fail(err)
			}
			val = strings.TrimRight(string(raw), "\r\n")
		}
		if val == "" {
			return fail(errors.New("empty secret value"))
		}
		if err := c.do(http.MethodPut, prefix+"/"+name, map[string]string{"value": val}, nil); err != nil {
			return fail(err)
		}
		fmt.Printf("set %s%s\n", name, scopeSuffix(*agent))
		return 0
	case "rm":
		if fs.NArg() < 1 {
			fmt.Fprintln(os.Stderr, "usage: roundclaw secret rm <name> [--agent id]")
			return 2
		}
		name := fs.Arg(0)
		if err := c.do(http.MethodDelete, prefix+"/"+name, nil, nil); err != nil {
			return fail(err)
		}
		fmt.Printf("deleted %s%s\n", name, scopeSuffix(*agent))
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown secret subcommand %q\n", sub)
		return 2
	}
}

// envFlags collects repeated --env NAME=VALUE flags into a map.
type envFlags map[string]string

func (e envFlags) String() string { return fmt.Sprintf("%v", map[string]string(e)) }

func (e envFlags) Set(v string) error {
	name, value, ok := strings.Cut(v, "=")
	if !ok || name == "" {
		return fmt.Errorf("expected NAME=VALUE, got %q", v)
	}
	e[name] = value
	return nil
}

func cmdTool(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: roundclaw tool <set|ls|show|rm> ...")
		return 2
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("tool", flag.ContinueOnError)
	base, token := commonFlags(fs)
	path := fs.String("path", "", "host directory to mount read-only at /mnt/<base> (set only)")
	desc := fs.String("desc", "", "one-line description (set only)")
	instr := fs.String("instructions", "", "how the agent should use the tool; - reads stdin (set only)")
	env := envFlags{}
	fs.Var(env, "env", "NAME=VALUE env var the tool needs; repeatable (set only)")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	c := newClient(*base, *token)

	switch sub {
	case "ls":
		var out struct {
			Tools []struct {
				ID          string `json:"id"`
				Description string `json:"description"`
				HostPath    string `json:"host_path"`
			} `json:"tools"`
		}
		if err := c.do(http.MethodGet, "/v1/tools", nil, &out); err != nil {
			return fail(err)
		}
		if len(out.Tools) == 0 {
			fmt.Println("no tools")
			return 0
		}
		for _, t := range out.Tools {
			fmt.Printf("%-20s %-40s %s\n", t.ID, t.HostPath, t.Description)
		}
		return 0
	case "show":
		if fs.NArg() < 1 {
			fmt.Fprintln(os.Stderr, "usage: roundclaw tool show <id>")
			return 2
		}
		var out map[string]any
		if err := c.do(http.MethodGet, "/v1/tools/"+fs.Arg(0), nil, &out); err != nil {
			return fail(err)
		}
		printJSON(out)
		return 0
	case "set":
		if fs.NArg() < 1 {
			fmt.Fprintln(os.Stderr, "usage: roundclaw tool set <id> --path P [--env NAME=VALUE] [--desc T] [--instructions T]")
			return 2
		}
		if *path == "" {
			return fail(errors.New("--path is required"))
		}
		instructions := *instr
		if instructions == "-" {
			raw, err := io.ReadAll(os.Stdin)
			if err != nil {
				return fail(err)
			}
			instructions = strings.TrimRight(string(raw), "\r\n")
		}
		body := map[string]any{
			"host_path":    *path,
			"description":  *desc,
			"env":          map[string]string(env),
			"instructions": instructions,
		}
		if err := c.do(http.MethodPut, "/v1/tools/"+fs.Arg(0), body, nil); err != nil {
			return fail(err)
		}
		fmt.Printf("registered tool %s -> %s\n", fs.Arg(0), *path)
		return 0
	case "rm":
		if fs.NArg() < 1 {
			fmt.Fprintln(os.Stderr, "usage: roundclaw tool rm <id>")
			return 2
		}
		if err := c.do(http.MethodDelete, "/v1/tools/"+fs.Arg(0), nil, nil); err != nil {
			return fail(err)
		}
		fmt.Printf("deleted tool %s\n", fs.Arg(0))
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown tool subcommand %q\n", sub)
		return 2
	}
}

func cmdSkill(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: roundclaw skill <set|ls|show|rm> ...")
		return 2
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("skill", flag.ContinueOnError)
	base, token := commonFlags(fs)
	path := fs.String("path", "", "host directory containing SKILL.md (set only)")
	desc := fs.String("desc", "", "one-line description (set only)")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	c := newClient(*base, *token)

	switch sub {
	case "ls":
		var out struct {
			Skills []struct {
				ID          string `json:"id"`
				Description string `json:"description"`
				HostPath    string `json:"host_path"`
			} `json:"skills"`
		}
		if err := c.do(http.MethodGet, "/v1/skills", nil, &out); err != nil {
			return fail(err)
		}
		if len(out.Skills) == 0 {
			fmt.Println("no skills")
			return 0
		}
		for _, s := range out.Skills {
			fmt.Printf("%-20s %-40s %s\n", s.ID, s.HostPath, s.Description)
		}
		return 0
	case "show":
		if fs.NArg() < 1 {
			fmt.Fprintln(os.Stderr, "usage: roundclaw skill show <id>")
			return 2
		}
		var out map[string]any
		if err := c.do(http.MethodGet, "/v1/skills/"+fs.Arg(0), nil, &out); err != nil {
			return fail(err)
		}
		printJSON(out)
		return 0
	case "set":
		if fs.NArg() < 1 {
			fmt.Fprintln(os.Stderr, "usage: roundclaw skill set <id> --path P [--desc T]")
			return 2
		}
		if *path == "" {
			return fail(errors.New("--path is required"))
		}
		body := map[string]any{"host_path": *path, "description": *desc}
		if err := c.do(http.MethodPut, "/v1/skills/"+fs.Arg(0), body, nil); err != nil {
			return fail(err)
		}
		fmt.Printf("registered skill %s -> %s\n", fs.Arg(0), *path)
		return 0
	case "rm":
		if fs.NArg() < 1 {
			fmt.Fprintln(os.Stderr, "usage: roundclaw skill rm <id>")
			return 2
		}
		if err := c.do(http.MethodDelete, "/v1/skills/"+fs.Arg(0), nil, nil); err != nil {
			return fail(err)
		}
		fmt.Printf("deleted skill %s\n", fs.Arg(0))
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown skill subcommand %q\n", sub)
		return 2
	}
}

func scopeSuffix(agent string) string {
	if agent == "" {
		return " (global)"
	}
	return " for " + agent
}

func jsonReader(v any) io.Reader {
	raw, _ := json.Marshal(v)
	return bytes.NewReader(raw)
}

func printJSON(v any) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Println(v)
		return
	}
	fmt.Println(string(raw))
}
