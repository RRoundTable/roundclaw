package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Evals from the terminal.
//
//	roundclaw eval ls|show|set|rm       the questions
//	roundclaw eval run <set>            ask them
//	roundclaw eval runs|result          what came back
//	roundclaw eval compare <a> <b>      whether it got better
//
// compare is the one that matters. It returns arithmetic, not an opinion: which
// cases regressed, which improved, and a verdict derived from those counts.
// Anything reading this — a person or an agent — should quote it rather than
// re-deciding it from the outputs.

func cmdEval(args []string) int {
	if len(args) == 0 {
		evalUsage()
		return 2
	}
	sub, rest := args[0], args[1:]

	switch sub {
	case "ls":
		return evalList(rest)
	case "show":
		return evalShow(rest)
	case "set":
		return evalSet(rest)
	case "rm":
		return evalRemove(rest)
	case "run":
		return evalRun(rest)
	case "runs":
		return evalRuns(rest)
	case "result":
		return evalResult(rest)
	case "compare":
		return evalCompare(rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown eval subcommand %q\n\n", sub)
		evalUsage()
		return 2
	}
}

func evalUsage() {
	fmt.Fprint(os.Stderr, `usage: roundclaw eval <command>

  ls [--agent id]                    list eval sets
  show <set>                         print a set's cases
  set <id> --agent <a> --cases FILE  create or replace a set from a JSON file (- for stdin)
      --desc TEXT, --full-grants (inject the agent's secrets; off by default)
  rm <set>                           delete a set; its runs are kept

  run <set>                          start a run
      --version N   evaluate that agent version (default: whatever is live)
      --notify      wake the calling agent when it finishes, instead of waiting
      --wait        poll until it finishes (default when --notify is absent)
      --timeout D   give up polling after this long; the run continues
  runs [--agent a] [--eval set]      list runs, newest first
  result <run-id> [--full]           one run with its per-case results
  compare <base-run> <candidate-run> what changed between two runs

A case is JSON: {"name","prompt","rubric","must_contain":[],"must_not_contain":[],"weight"}
must_contain is checked exactly, in code, before any judge is asked.
`)
}

func evalList(args []string) int {
	fs := flag.NewFlagSet("eval ls", flag.ContinueOnError)
	base, token := commonFlags(fs)
	agent := fs.String("agent", "", "only sets for this agent")
	if _, err := parseFlags(fs, args); err != nil {
		return 2
	}
	path := "/v1/evals"
	if *agent != "" {
		path += "?agent=" + url.QueryEscape(*agent)
	}
	var out map[string]any
	if err := newClient(*base, *token).do(http.MethodGet, path, nil, &out); err != nil {
		return fail(err)
	}
	printJSON(out)
	return 0
}

func evalShow(args []string) int {
	fs := flag.NewFlagSet("eval show", flag.ContinueOnError)
	base, token := commonFlags(fs)
	positional, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) < 1 {
		fmt.Fprintln(os.Stderr, "usage: roundclaw eval show <set>")
		return 2
	}
	var out map[string]any
	if err := newClient(*base, *token).do(http.MethodGet, "/v1/evals/"+positional[0], nil, &out); err != nil {
		return fail(err)
	}
	printJSON(out)
	return 0
}

func evalSet(args []string) int {
	fs := flag.NewFlagSet("eval set", flag.ContinueOnError)
	base, token := commonFlags(fs)
	agent := fs.String("agent", "", "the agent this set evaluates (required)")
	casesPath := fs.String("cases", "", "JSON file holding an array of cases, or - for stdin (required)")
	desc := fs.String("desc", "", "what this set is for")
	fullGrants := fs.Bool("full-grants", false,
		"inject the agent's secrets and supplementary groups into eval containers")

	positional, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) < 1 || *agent == "" || *casesPath == "" {
		fmt.Fprintln(os.Stderr, "usage: roundclaw eval set <id> --agent <a> --cases <file|->")
		return 2
	}

	raw, err := readFileOrStdin(*casesPath)
	if err != nil {
		return fail(err)
	}
	var cases []map[string]any
	if err := json.Unmarshal(raw, &cases); err != nil {
		return fail(fmt.Errorf("cases must be a JSON array of case objects: %w", err))
	}

	body := map[string]any{
		"id": positional[0], "agent_id": *agent, "description": *desc,
		"cases": cases, "full_grants": *fullGrants,
	}
	var out map[string]any
	if err := newClient(*base, *token).do(http.MethodPut, "/v1/evals/"+positional[0], body, &out); err != nil {
		return fail(err)
	}
	printJSON(out)
	return 0
}

func evalRemove(args []string) int {
	fs := flag.NewFlagSet("eval rm", flag.ContinueOnError)
	base, token := commonFlags(fs)
	positional, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) < 1 {
		fmt.Fprintln(os.Stderr, "usage: roundclaw eval rm <set>")
		return 2
	}
	var out map[string]any
	if err := newClient(*base, *token).do(http.MethodDelete, "/v1/evals/"+positional[0], nil, &out); err != nil {
		return fail(err)
	}
	printJSON(out)
	return 0
}

func evalRun(args []string) int {
	fs := flag.NewFlagSet("eval run", flag.ContinueOnError)
	base, token := commonFlags(fs)
	version := fs.Int("version", 0, "the agent version to evaluate; 0 uses whatever is live")
	notify := fs.Bool("notify", false,
		"do not wait: the calling agent is woken with the result when the run finishes")
	wait := fs.Bool("wait", true, "poll until the run finishes (default, unless --notify)")
	timeout := fs.Duration("timeout", time.Hour, "give up polling after this long; the run continues")

	positional, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) < 1 {
		fmt.Fprintln(os.Stderr, "usage: roundclaw eval run <set> [--version N] [--notify]")
		return 2
	}

	body := map[string]any{"version": *version}
	if *notify {
		// The container already knows who it is, so an agent asking to be told
		// does not have to name itself — and cannot name somebody else by mistake.
		self := os.Getenv("ROUNDCLAW_AGENT_ID")
		if self == "" {
			return fail(fmt.Errorf("--notify only works inside an agent container, where ROUNDCLAW_AGENT_ID is set"))
		}
		body["notify"] = map[string]string{
			"agent": self, "conversation": os.Getenv("ROUNDCLAW_CONVERSATION_ID"),
		}
	}

	c := newClient(*base, *token)
	var started struct {
		RunID int64  `json:"run_id"`
		Note  string `json:"note"`
	}
	if err := c.do(http.MethodPost, "/v1/evals/"+positional[0]+"/run", body, &started); err != nil {
		return fail(err)
	}

	if *notify || !*wait {
		printJSON(map[string]any{"run_id": started.RunID, "status": "running", "note": started.Note})
		return 0
	}
	return pollEvalRun(c, started.RunID, *timeout)
}

// pollEvalRun waits for a run to reach a terminal state.
//
// Ten seconds between polls, not one: a run is minutes of container work, and a
// tighter loop would only add requests. A caller that cannot afford to wait at
// all should use --notify, which costs nothing while it waits.
func pollEvalRun(c *client, runID int64, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	path := "/v1/evals/runs/" + strconv.FormatInt(runID, 10)
	for {
		var out struct {
			Run struct {
				Status string `json:"status"`
			} `json:"run"`
		}
		if err := c.do(http.MethodGet, path, nil, &out); err != nil {
			return fail(err)
		}
		if out.Run.Status != "running" {
			var full map[string]any
			if err := c.do(http.MethodGet, path, nil, &full); err != nil {
				return fail(err)
			}
			printJSON(full)
			return 0
		}
		if time.Now().After(deadline) {
			printJSON(map[string]any{
				"run_id": runID, "status": "running",
				"note": "still running; it was not stopped. Read it later with `roundclaw eval result " +
					strconv.FormatInt(runID, 10) + "`",
			})
			return 0
		}
		time.Sleep(10 * time.Second)
	}
}

func evalRuns(args []string) int {
	fs := flag.NewFlagSet("eval runs", flag.ContinueOnError)
	base, token := commonFlags(fs)
	agent := fs.String("agent", "", "only runs of this agent")
	evalSetID := fs.String("eval", "", "only runs of this eval set")
	limit := fs.Int("limit", 20, "how many runs to list")
	if _, err := parseFlags(fs, args); err != nil {
		return 2
	}

	q := url.Values{}
	if *agent != "" {
		q.Set("agent", *agent)
	}
	if *evalSetID != "" {
		q.Set("eval", *evalSetID)
	}
	q.Set("limit", strconv.Itoa(*limit))

	var out map[string]any
	if err := newClient(*base, *token).do(http.MethodGet, "/v1/evals/runs?"+q.Encode(), nil, &out); err != nil {
		return fail(err)
	}
	printJSON(out)
	return 0
}

func evalResult(args []string) int {
	fs := flag.NewFlagSet("eval result", flag.ContinueOnError)
	base, token := commonFlags(fs)
	full := fs.Bool("full", false, "do not truncate the answers")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) < 1 {
		fmt.Fprintln(os.Stderr, "usage: roundclaw eval result <run-id> [--full]")
		return 2
	}
	path := "/v1/evals/runs/" + positional[0]
	if *full {
		path += "?full=true"
	}
	var out map[string]any
	if err := newClient(*base, *token).do(http.MethodGet, path, nil, &out); err != nil {
		return fail(err)
	}
	printJSON(out)
	return 0
}

func evalCompare(args []string) int {
	fs := flag.NewFlagSet("eval compare", flag.ContinueOnError)
	base, token := commonFlags(fs)
	positional, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) < 2 {
		fmt.Fprintln(os.Stderr, "usage: roundclaw eval compare <base-run> <candidate-run>")
		return 2
	}
	q := url.Values{"base": {positional[0]}, "candidate": {positional[1]}}
	var out map[string]any
	if err := newClient(*base, *token).do(http.MethodGet, "/v1/evals/compare?"+q.Encode(), nil, &out); err != nil {
		return fail(err)
	}
	printJSON(out)
	return 0
}

// readFileOrStdin reads a path, or stdin when the path is "-". Cases are often
// generated by an agent and piped straight in, so "-" is the common case rather
// than the exotic one.
func readFileOrStdin(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}
