// Command quickbench is a small, quick comparison of roundclaw against plain
// Claude Code on a live stack.
//
// Unit and end-to-end tests cover roundclaw's plumbing with a fake CLI. What
// they cannot show is how the whole system behaves against a real model beside
// the thing it wraps: whether an agent behind roundclaw still does the work,
// whether it hands known work to a routine and holds back otherwise, what a
// hand-off puts into the delegator's context, and whether a result survives a
// killed worker. Those are judgements about live behaviour, so they live here
// rather than in `go test`, as cmd/routercheck does for the router.
//
// It compares four systems on the same model, image and CLI:
//
//	A  claude -p, sub-agents refused
//	B  claude -p, sub-agents allowed
//	C  a roundclaw agent with no skills
//	D  a roundclaw agent holding the roundclaw-routines skill
//
// and runs four checks: overall (small coding tasks with a check each),
// delegation (requests a routine covers, does not cover, and nearly covers),
// context (what a long delegated result adds to the delegator) and durability
// (a worker killed during a routine run; opt-in, since it restarts the worker).
//
// One attempt per task: it catches breakage and large gaps, and cannot rank
// the systems. It needs a running stack, the agent image and a credential:
//
//	set -a; . ./.env; set +a
//	go run ./cmd/quickbench --config roundclaw.yaml [--only overall,delegation] [--kill-worker]
//
// It registers agents, a tool, a skill and routines whose ids start with "qb-"
// and leaves them in place; every run uses fresh conversations.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/roundtable/roundclaw/internal/config"
	"github.com/roundtable/roundclaw/internal/core"
)

// trial is one measured piece of work by one system.
type trial struct {
	Check   string     `json:"check"`
	System  string     `json:"system"`
	Task    string     `json:"task"`
	Pass    bool       `json:"pass"`
	Note    string     `json:"note,omitempty"`
	CostUSD float64    `json:"cost_usd"`
	Usage   core.Usage `json:"usage"`
	WallMS  int64      `json:"wall_ms"`
	// Handoffs counts delegations: sub-agent calls for Claude Code, routine runs
	// for roundclaw.
	Handoffs int `json:"handoffs"`
	// DeliveredChars is how much a hand-off's result put into the delegator's
	// context (the context check only).
	DeliveredChars int `json:"delivered_chars,omitempty"`
	// Infra is set when the trial did not run to a result — a container that
	// would not start, an API that did not answer. Reported apart from a fail.
	Infra string `json:"infra,omitempty"`
}

type bench struct {
	cfg      *config.Config
	cred     config.Credential
	fleet    *fleet
	runID    string
	scratch  string
	agentURL string
	skills   string
	timeout  time.Duration
	killCmd  string
	startCmd string
	trials   []trial
}

func main() { os.Exit(run()) }

func run() int {
	configPath := flag.String("config", "roundclaw.yaml", "path to the roundclaw config")
	apiURL := flag.String("url", "", "roundclaw API as seen from here (default http://localhost<http.addr>)")
	agentURL := flag.String("agent-url", "", "roundclaw API as seen from an agent container (default http://roundclaw-gateway-1<http.addr>)")
	skillsDir := flag.String("skills", "skills", "directory holding the roundclaw-routines skill")
	only := flag.String("only", "overall,delegation,context", "checks to run, comma-separated: overall, delegation, context, durability")
	systems := flag.String("systems", "A,B,C,D", "systems for the overall check")
	killWorker := flag.Bool("kill-worker", false, "also run the durability check, which kills and restarts the worker")
	killCmd := flag.String("kill-cmd", "docker compose kill worker", "how the durability check kills the worker")
	startCmd := flag.String("start-cmd", "docker compose start worker", "how the durability check starts it again")
	timeout := flag.Duration("timeout", 6*time.Minute, "limit for one piece of work")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		return 2
	}
	cred, err := cfg.Container.ResolveCredential(os.LookupEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "no credential:", err)
		return 2
	}
	token := firstToken(os.Getenv(cfg.HTTP.TokensEnv))
	if token == "" {
		fmt.Fprintf(os.Stderr, "no API token: set %s\n", cfg.HTTP.TokensEnv)
		return 2
	}
	if *apiURL == "" {
		*apiURL = "http://localhost" + cfg.HTTP.Addr
	}
	if *agentURL == "" {
		*agentURL = "http://roundclaw-gateway-1" + cfg.HTTP.Addr
	}
	skills, err := filepath.Abs(*skillsDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "skills dir:", err)
		return 2
	}

	runID := time.Now().Format("0102-150405")
	b := &bench{
		cfg: cfg, cred: cred, runID: runID, agentURL: *agentURL, skills: skills,
		fleet:   &fleet{base: *apiURL, token: token},
		scratch: filepath.Join(cfg.WorkspaceRoot, "quickbench", runID),
		timeout: *timeout, killCmd: *killCmd, startCmd: *startCmd,
	}
	if err := os.MkdirAll(b.scratch, 0o750); err != nil {
		fmt.Fprintln(os.Stderr, "create scratch dir:", err)
		return 2
	}

	ctx := context.Background()
	if err := b.setup(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
		return 2
	}
	fmt.Printf("quickbench %s\n  image: %s | model: %s | credential: %s\n  records: %s\n\n",
		runID, cfg.Container.Image, orDefault(cfg.Container.Model, "CLI default"), cred.EnvName, b.scratch)

	want := map[string]bool{}
	for _, c := range strings.Split(*only, ",") {
		want[strings.TrimSpace(c)] = true
	}
	if *killWorker {
		want["durability"] = true
	}
	if want["overall"] {
		b.checkOverall(ctx, strings.Split(*systems, ","))
	}
	if want["delegation"] {
		b.checkDelegation(ctx)
	}
	if want["context"] {
		b.checkContext(ctx)
	}
	if want["durability"] {
		b.checkDurability(ctx)
	}

	if err := b.writeRecords(); err != nil {
		fmt.Fprintln(os.Stderr, "write records:", err)
		return 2
	}
	b.report()
	return 0
}

// add records a trial and prints it as it lands, so a long run shows progress.
func (b *bench) add(t trial) {
	b.trials = append(b.trials, t)
	mark := "pass"
	switch {
	case t.Infra != "":
		mark = "INFRA"
	case !t.Pass:
		mark = "FAIL"
	}
	fmt.Printf("  [%-5s] %-10s %s %-22s %6.1fs  $%.4f  %s\n",
		mark, t.Check, t.System, t.Task, float64(t.WallMS)/1000, t.CostUSD, firstLine(t.Note+t.Infra, 90))
}

func (b *bench) writeRecords() error {
	f, err := os.Create(filepath.Join(b.scratch, "trials.jsonl"))
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, t := range b.trials {
		if err := enc.Encode(t); err != nil {
			return err
		}
	}
	return nil
}

// report prints one row per check and system: what passed, what it cost and
// how much was delegated. Infrastructure failures are counted apart, so a
// container that would not start is never read as the system failing the task.
func (b *bench) report() {
	type key struct{ check, system string }
	type sum struct {
		n, pass, infra, handoffs, delivered int
		cost                                float64
		tokens, out, wall                   int64
	}
	var order []key
	sums := map[key]*sum{}
	for _, t := range b.trials {
		k := key{t.Check, t.System}
		s, ok := sums[k]
		if !ok {
			s = &sum{}
			sums[k] = s
			order = append(order, k)
		}
		s.n++
		if t.Infra != "" {
			s.infra++
		} else if t.Pass {
			s.pass++
		}
		s.handoffs += t.Handoffs
		s.delivered += t.DeliveredChars
		s.cost += t.CostUSD
		s.tokens += t.Usage.Tokens()
		for _, m := range t.Usage.Models {
			s.out += m.OutputTokens
		}
		s.wall += t.WallMS
	}

	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "CHECK\tSYSTEM\tPASSED\tINFRA\tHAND-OFFS\tDELIVERED\tCOST\tTOKENS\tOUTPUT\tTIME")
	for _, k := range order {
		s := sums[k]
		delivered := "-"
		if s.delivered > 0 {
			delivered = fmt.Sprintf("%d chars", s.delivered)
		}
		fmt.Fprintf(w, "%s\t%s\t%d/%d\t%d\t%d\t%s\t$%.4f\t%d\t%d\t%.0fs\n",
			k.check, k.system, s.pass, s.n-s.infra, s.infra, s.handoffs, delivered,
			s.cost, s.tokens, s.out, float64(s.wall)/1000)
	}
	w.Flush()
	fmt.Println("\nsystems: A claude -p without sub-agents · B claude -p with sub-agents · C roundclaw agent · D roundclaw agent with routines")
	fmt.Println("one attempt per task: this catches breakage and large gaps, and cannot rank the systems")
}

func firstToken(list string) string {
	for _, t := range strings.Split(list, ",") {
		if t = strings.TrimSpace(t); t != "" {
			return t
		}
	}
	return ""
}

func firstLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
