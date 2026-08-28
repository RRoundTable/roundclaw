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
	"strconv"
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
	case "say":
		return cmdSay(rest)
	case "status":
		return cmdStatus(rest)
	case "turn":
		return cmdTurn(rest)
	case "history":
		return cmdHistory(rest)
	case "version":
		return cmdVersion(rest)
	case "eval":
		return cmdEval(rest)
	case "proposal":
		return cmdProposal(rest)
	case "schedule":
		return cmdSchedule(rest)
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
                               --notify-me: don't wait, the result comes back to you as a new turn
                               --conversation C: run in a named conversation of that agent
                               (--no-wait to just queue), --timeout, --steer, --callback, --key
  say <text>                   say something in this conversation, without a turn
                               --to <agent> to speak in the conversation that delegated to you
  status <agent>               what an agent is doing right now
  turn <agent> <turn-id>       a turn's state and result

  history <agent>              what has been asked of it, newest first
      --limit N (max 200), --since 72h (or an RFC3339 instant),
      --status running|done|stopped|error, --conversation C, --full
  version ls <agent>           its definition+persona history, newest first
  version show <agent> <n>     one version in full
  version rollback <agent> <n> apply an old version as a NEW version (--note why)

  eval set <id> --agent <a> --cases <file|->   define what "good" means
  eval run <set> [--version N]                 measure a version (--notify to be woken)
  eval compare <base-run> <candidate-run>      what regressed, what improved
      also: eval ls, eval show <set>, eval rm <set>, eval runs, eval result <run>
      run "roundclaw eval" alone for the full set of flags

  proposal new --kind K --target T --why TEXT   write a change down, do not make it
  proposal ls [--status pending]                what is waiting on a person
  proposal approve <id> | reject <id>           decide (also: /proposals in Discord)
      run "roundclaw proposal" alone for the payload shapes

  schedule ls                  my recurring work, with its next run times
  schedule show <id>
  schedule set <id> --cron "0 9 * * *" --prompt TEXT
      --tz Asia/Seoul, --desc TEXT, --channel <id> (must be one I am bound to),
      --suppress-if TEXT (skip delivery when the result contains it), --paused
      only the flags you pass change; the rest of the definition is kept
  schedule rm <id> | schedule pause <id> | schedule resume <id>
      --agent <id> on any of them to manage another agent's (needs a full token);
      inside a container it defaults to me, so my own schedules need no flag

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

// parseFlags parses args and returns the positional ones, accepting flags
// *after* positionals as well as before.
//
// Go's flag package stops at the first non-flag token, so `send dev "text"
// --notify-me` would leave --notify-me unparsed and swallow it into the text —
// silently doing the opposite of what was asked, which is precisely the failure
// this tooling exists to prevent. Callers write flags last far more often than
// first, and an agent writing a command has no way to know it mattered.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		// The first leftover is a positional; everything after it may hold more
		// flags, so hand the remainder back to Parse.
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
}

// commonFlags registers --url and --token on a flag set, defaulting to the
// environment, so every subcommand accepts them.
func commonFlags(fs *flag.FlagSet) (*string, *string) {
	base := fs.String("url", envOr("ROUNDCLAW_URL", "http://127.0.0.1:8099"), "gateway base URL")
	// ROUNDCLAW_SELF_TOKEN is what the worker puts in an agent's container: the
	// credential that says which agent this is. It is the fallback rather than
	// the default so an operator who exported a full-scope token in the same
	// shell still gets the wider one — the narrower credential should never
	// silently replace a credential somebody chose.
	token := fs.String("token", envOr("ROUNDCLAW_API_TOKEN", os.Getenv("ROUNDCLAW_SELF_TOKEN")), "bearer token")
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
	return c.doWithNote(method, path, "", body, out)
}

// doWithNote is do with a changelog line attached, recorded on any agent version
// the request mints. Empty note records nothing.
func (c *client) doWithNote(method, path, note string, body, out any) error {
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
	// Inside an agent container ROUNDCLAW_AGENT_ID is already set, so an agent
	// that changes another agent signs the version it creates without being asked
	// to. It is a changelog rather than an audit trail — the header is unverified
	// — but "who edited dev last week" is otherwise unanswerable.
	if id := os.Getenv("ROUNDCLAW_AGENT_ID"); id != "" {
		req.Header.Set("X-Roundclaw-Author", "agent:"+id)
	}
	if note != "" {
		req.Header.Set("X-Roundclaw-Note", note)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpError{code: resp.StatusCode, status: resp.Status, msg: serverMessage(raw)}
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// httpError carries the status code so a caller can tell "not there yet" from
// "the gateway is unhappy". `schedule set` needs the difference: it merges onto
// the definition it reads back, and taking a failed read for a missing schedule
// would quietly drop every field the command did not name.
type httpError struct {
	code   int
	status string
	msg    string
}

func (e *httpError) Error() string { return e.status + ": " + e.msg }

// notFound reports whether err is a 404 from the gateway.
func notFound(err error) bool {
	var he *httpError
	return errors.As(err, &he) && he.code == http.StatusNotFound
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
	// --notify-me is the durable alternative to waiting. Waiting ties the result
	// to this process: a bash timeout or a finished turn kills the reader and the
	// result is then recorded with nobody to read it. With a return address on the
	// turn row instead, the delegated result comes back as a new turn in the
	// calling conversation however long it takes.
	notifyMe := fs.Bool("notify-me", false,
		"return the result to me as a new turn when it finishes, instead of waiting (implies --no-wait)")
	conversation := fs.String("conversation", "",
		"run in this named conversation of the target agent (own session, queue and workspace); "+
			"with --notify-me it defaults to the conversation I am running in")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) < 2 {
		fmt.Fprintln(os.Stderr, "usage: roundclaw send <agent> <text> [--notify-me] [--conversation C] "+
			"[--wait|--no-wait] [--timeout D] [--steer] [--callback URL] [--key K]")
		return 2
	}
	waiting := *wait && !*noWait
	agent := pos[0]
	text := strings.Join(pos[1:], " ")

	body := map[string]any{"text": text}
	if *callback != "" {
		body["callback_url"] = *callback
	}
	if *steer {
		body["steer"] = true
	}
	if *conversation != "" {
		body["conversation_id"] = *conversation
	}
	if *notifyMe {
		// Who to wake comes from the environment the worker injects, not from a
		// flag: the caller should not have to know its own agent ID, and a value
		// it typed could name someone else.
		me := os.Getenv("ROUNDCLAW_AGENT_ID")
		if me == "" {
			return fail(fmt.Errorf("--notify-me needs ROUNDCLAW_AGENT_ID, which is set inside an agent container; " +
				"from a terminal use --wait or --callback instead"))
		}
		mine := os.Getenv("ROUNDCLAW_CONVERSATION_ID")
		body["notify"] = map[string]any{
			"agent":        me,
			"conversation": mine,
		}
		// Delegated work runs in the conversation it belongs to, not the
		// delegate's default one. Left unnamed it lands in the conversation
		// /ask, schedules and webhooks share, so every thread handing work to
		// the same agent piles into one Claude session and reads the others'
		// history — and that conversation ends up carrying an audience per
		// thread, which is the one-conversation-one-audience invariant a result
		// relies on to know where it goes.
		//
		// The delegator's own ID is passed through unchanged rather than derived
		// from. A thread ID is a Discord snowflake, unique across the fleet, so
		// it cannot collide with a conversation the delegate already holds; it
		// does not grow as a chain delegates onward; and `/stop` typed in that
		// thread resolves to this same ID, which is what keeps a wedged
		// delegated turn stoppable from where the person is watching it.
		if *conversation == "" && mine != "" {
			body["conversation_id"] = mine
		}
		// Waiting as well would defeat the point and hold this process open for
		// a result that is already guaranteed to come back.
		waiting = false
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
		Conversation  string  `json:"conversation"`
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
		if out.Conversation != "" {
			fmt.Printf("conversation %s — continue there with: roundclaw send %s --conversation %s \"...\"\n",
				out.Conversation, agent, out.Conversation)
		}
		if *notifyMe {
			fmt.Printf("the result will come back to you as a new turn; do not promise a follow-up yourself\n")
		}
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

// fileList collects a repeatable --file flag.
type fileList []string

func (f *fileList) String() string { return strings.Join(*f, ", ") }
func (f *fileList) Set(v string) error {
	*f = append(*f, v)
	return nil
}

// cmdSay speaks in a conversation without running a turn.
//
// It exists for the middle of a long job. A turn's result is the agent's only
// scheduled chance to say anything, which is no use to whoever is watching an
// empty channel for twenty minutes. This costs nothing — no session, no
// container, no model call — and correspondingly guarantees nothing: it is not
// retried and not recorded as work. Final results belong in --notify-me.
func cmdSay(args []string) int {
	fs := flag.NewFlagSet("say", flag.ContinueOnError)
	base, token := commonFlags(fs)
	to := fs.String("to", "", "agent whose conversation to speak in (default: my own)")
	conversation := fs.String("conversation", "", "conversation to speak in (default: inferred from where I am)")
	var files fileList
	fs.Var(&files, "file", "a file in outbox/ to attach (repeatable)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	// A file says enough on its own; only a message carrying neither is empty.
	if len(pos) < 1 && len(files) == 0 {
		fmt.Fprintln(os.Stderr,
			"usage: roundclaw say <text> [--file outbox/report.pdf] [--to <agent>] [--conversation C]")
		return 2
	}
	text := strings.Join(pos, " ")

	// Defaults come from the environment the worker injects, so the common cases
	// need no flags: speaking here, or reporting to whoever delegated to me.
	agent, conv := *to, *conversation
	switch {
	case agent == "" && conv == "":
		agent, conv = os.Getenv("ROUNDCLAW_AGENT_ID"), os.Getenv("ROUNDCLAW_CONVERSATION_ID")
	case agent != "" && conv == "" && agent == os.Getenv("ROUNDCLAW_REPLY_TO"):
		// --to the delegator: its conversation is the one that is waiting.
		conv = os.Getenv("ROUNDCLAW_REPLY_TO_CONVERSATION")
	case agent == "":
		agent = os.Getenv("ROUNDCLAW_AGENT_ID")
	}
	if agent == "" {
		return fail(fmt.Errorf("nothing to speak as: set --to, or run inside an agent container where " +
			"ROUNDCLAW_AGENT_ID is set"))
	}

	body := map[string]any{"text": text}
	if conv != "" {
		body["conversation"] = conv
	}
	if len(files) > 0 {
		body["files"] = []string(files)
	}
	// Speaking where I am: name the turn I am running, so the server reads my
	// audience off that row rather than inferring one from the conversation's
	// recent history. A delegated turn has no history of its own to infer from —
	// the address it needs was written on the row when the turn was admitted.
	// Speaking into someone else's conversation deliberately does not send it:
	// their audience is theirs to resolve, not mine to assert.
	if agent == os.Getenv("ROUNDCLAW_AGENT_ID") && conv == os.Getenv("ROUNDCLAW_CONVERSATION_ID") {
		if id, err := strconv.ParseInt(os.Getenv("ROUNDCLAW_TURN_ID"), 10, 64); err == nil && id > 0 {
			body["turn_id"] = id
		}
	}

	c := newClient(*base, *token)
	var out struct {
		Delivered bool   `json:"delivered"`
		Target    string `json:"target"`
		Files     int    `json:"files"`
	}
	if err := c.do(http.MethodPost, "/v1/agents/"+agent+"/messages", body, &out); err != nil {
		return fail(err)
	}
	if !out.Delivered {
		return fail(fmt.Errorf("not delivered"))
	}
	attached := ""
	if out.Files > 0 {
		attached = fmt.Sprintf(", %d file(s) attached", out.Files)
	}
	fmt.Printf("said it in %s (channel %s%s)\n", agent, out.Target, attached)
	return 0
}

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	base, token := commonFlags(fs)
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) < 1 {
		fmt.Fprintln(os.Stderr, "usage: roundclaw status <agent>")
		return 2
	}
	var out map[string]any
	if err := newClient(*base, *token).do(http.MethodGet, "/v1/agents/"+pos[0], nil, &out); err != nil {
		return fail(err)
	}
	printJSON(out)
	return 0
}

func cmdTurn(args []string) int {
	fs := flag.NewFlagSet("turn", flag.ContinueOnError)
	base, token := commonFlags(fs)
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) < 2 {
		fmt.Fprintln(os.Stderr, "usage: roundclaw turn <agent> <turn-id>")
		return 2
	}
	var out map[string]any
	path := "/v1/agents/" + pos[0] + "/turns/" + pos[1]
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
	// parseFlags, not fs.Parse: `secret set NAME --agent gameart` is how anyone
	// writes it, and a plain Parse stops at NAME and never sees --agent — storing
	// a credential globally, readable by every agent, while reporting success.
	pos, err := parseFlags(fs, rest)
	if err != nil {
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
		if len(pos) < 1 {
			fmt.Fprintln(os.Stderr, "usage: roundclaw secret set <name> [value] [--agent id]")
			return 2
		}
		name := pos[0]
		val := *value
		if val == "" && len(pos) >= 2 {
			val = pos[1]
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
		if len(pos) < 1 {
			fmt.Fprintln(os.Stderr, "usage: roundclaw secret rm <name> [--agent id]")
			return 2
		}
		name := pos[0]
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

// scheduleView is a schedule definition joined with its live trigger state.
type scheduleView struct {
	ID          string `json:"id"`
	AgentID     string `json:"agent_id"`
	Description string `json:"description"`
	Cron        string `json:"cron"`
	Timezone    string `json:"timezone"`
	Prompt      string `json:"prompt"`
	ChannelID   string `json:"channel_id"`
	SuppressIf  string `json:"suppress_if"`
	Enabled     bool   `json:"enabled"`
	Paused      bool   `json:"paused"`
	NextRun     string `json:"next_run"`
	Unavailable string `json:"unavailable"`
}

// schedulePrefix is the route an agent's schedules live under. With no agent it
// falls back to the fleet-wide routes, which a restricted token cannot reach —
// that is deliberate: the agent-scoped path is what carries the caller's
// identity, and without one there is nothing to scope to.
func schedulePrefix(agent string) string {
	if agent == "" {
		return "/v1/schedules"
	}
	return "/v1/agents/" + agent + "/schedules"
}

// cmdSchedule manages recurring work. Inside an agent container --agent defaults
// to the agent itself, so managing one's own schedules takes no flag and cannot
// accidentally name somebody else.
func cmdSchedule(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: roundclaw schedule <ls|show|set|rm|pause|resume> [id] [flags]")
		return 2
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("schedule", flag.ContinueOnError)
	base, token := commonFlags(fs)
	agent := fs.String("agent", os.Getenv("ROUNDCLAW_AGENT_ID"), "agent that owns the schedule (default: me)")
	cron := fs.String("cron", "", "cron expression, e.g. \"0 9 * * *\" (set only)")
	prompt := fs.String("prompt", "", "what to ask the agent when it fires; - reads stdin (set only)")
	tz := fs.String("tz", "", "IANA timezone, e.g. Asia/Seoul (default UTC)")
	desc := fs.String("desc", "", "one-line description")
	channel := fs.String("channel", "", "channel id to report into; must be one the agent is bound to")
	suppressIf := fs.String("suppress-if", "", "do not deliver a result containing this text")
	paused := fs.Bool("paused", false, "save it paused (set only)")
	pos, err := parseFlags(fs, rest)
	if err != nil {
		return 2
	}
	// Which flags were actually typed, so `set` can change the time without
	// wiping the prompt.
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	c := newClient(*base, *token)
	prefix := schedulePrefix(*agent)

	needID := func() (string, bool) {
		if len(pos) < 1 {
			fmt.Fprintf(os.Stderr, "usage: roundclaw schedule %s <id> [--agent id]\n", sub)
			return "", false
		}
		return pos[0], true
	}

	switch sub {
	case "ls":
		var out struct {
			Schedules []scheduleView `json:"schedules"`
		}
		if err := c.do(http.MethodGet, prefix, nil, &out); err != nil {
			return fail(err)
		}
		if len(out.Schedules) == 0 {
			fmt.Println("no schedules")
			return 0
		}
		for _, s := range out.Schedules {
			state := "running"
			switch {
			case s.Unavailable != "":
				state = "unknown"
			case s.Paused || !s.Enabled:
				state = "paused"
			}
			next := s.NextRun
			if next == "" {
				next = "-"
			}
			fmt.Printf("%-20s %-16s %-14s %-14s %-8s next %s  %s\n",
				s.ID, s.AgentID, s.Cron, s.Timezone, state, next, s.Description)
		}
		return 0

	case "show":
		id, ok := needID()
		if !ok {
			return 2
		}
		var out map[string]any
		if err := c.do(http.MethodGet, prefix+"/"+id, nil, &out); err != nil {
			return fail(err)
		}
		printJSON(out)
		return 0

	case "set":
		id, ok := needID()
		if !ok {
			return 2
		}
		// Read the current definition first and change only what was named. A PUT
		// replaces the whole row, so `set daily --cron "0 8 * * *"` would otherwise
		// drop the prompt — and a schedule is refused without one, which at least
		// fails loudly, but an omitted channel would silently go quiet instead.
		var cur scheduleView
		readErr := c.do(http.MethodGet, prefix+"/"+id, nil, &cur)
		if readErr != nil && !notFound(readErr) {
			// Anything other than "there is no such schedule yet" leaves us unable
			// to say what we would be overwriting.
			return fail(fmt.Errorf("read the current definition: %w", readErr))
		}
		exists := readErr == nil
		body := map[string]any{}
		if exists {
			body["description"] = cur.Description
			body["cron"] = cur.Cron
			body["timezone"] = cur.Timezone
			body["prompt"] = cur.Prompt
			body["channel_id"] = cur.ChannelID
			body["suppress_if"] = cur.SuppressIf
		}
		if given["cron"] {
			body["cron"] = *cron
		}
		if given["tz"] {
			body["timezone"] = *tz
		}
		if given["desc"] {
			body["description"] = *desc
		}
		if given["channel"] {
			body["channel_id"] = *channel
		}
		if given["suppress-if"] {
			body["suppress_if"] = *suppressIf
		}
		if given["prompt"] {
			text := *prompt
			if text == "-" {
				raw, err := io.ReadAll(os.Stdin)
				if err != nil {
					return fail(err)
				}
				text = strings.TrimRight(string(raw), "\r\n")
			}
			body["prompt"] = text
		}
		// Left unsaid, the server keeps whatever state the schedule is already in;
		// a new one starts running.
		if given["paused"] {
			body["enabled"] = !*paused
		}
		if *agent == "" {
			// The fleet-wide route takes the owner from the body, and a new
			// schedule has none to inherit.
			if !exists {
				return fail(errors.New("--agent is required to create a schedule " +
					"(inside a container it is already set for you)"))
			}
			body["agent_id"] = cur.AgentID
		}

		var out scheduleView
		if err := c.do(http.MethodPut, prefix+"/"+id, body, &out); err != nil {
			return fail(err)
		}
		state := "running"
		if out.Paused || !out.Enabled {
			state = "paused"
		}
		fmt.Printf("saved %s for %s: %s %s, %s\n", out.ID, out.AgentID, out.Cron, out.Timezone, state)
		if out.NextRun != "" {
			fmt.Printf("next run %s\n", out.NextRun)
		}
		if out.ChannelID == "" {
			fmt.Println("no channel: the result is recorded, not announced anywhere")
		}
		return 0

	case "rm":
		id, ok := needID()
		if !ok {
			return 2
		}
		if err := c.do(http.MethodDelete, prefix+"/"+id, nil, nil); err != nil {
			return fail(err)
		}
		fmt.Printf("deleted schedule %s\n", id)
		return 0

	case "pause", "resume":
		id, ok := needID()
		if !ok {
			return 2
		}
		var out scheduleView
		if err := c.do(http.MethodPost, prefix+"/"+id+"/"+sub, nil, &out); err != nil {
			return fail(err)
		}
		if sub == "pause" {
			fmt.Printf("paused %s; it keeps its definition and fires again when resumed\n", id)
			return 0
		}
		fmt.Printf("resumed %s", id)
		if out.NextRun != "" {
			fmt.Printf(", next run %s", out.NextRun)
		}
		fmt.Println()
		return 0

	default:
		fmt.Fprintf(os.Stderr, "unknown schedule subcommand %q\n", sub)
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
