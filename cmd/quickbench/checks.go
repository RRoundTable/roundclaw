package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/roundtable/roundclaw/internal/registry"
)

// ---- overall: small coding tasks, each with a check ------------------------

// codingTask is a piece of work whose result is files, and a command that
// decides whether they are right. The check never asks a model.
type codingTask struct {
	name   string
	prompt string
	seed   map[string]string
	check  string // run in the work directory; its output must equal want
	want   string
	frozen string // a seeded file the work must not have changed
}

var codingTasks = []codingTask{
	{
		name: "fizz",
		prompt: "Create fizz.sh in the current directory: a POSIX shell script that prints the numbers 1 to 15, one per line, " +
			"replacing multiples of 3 with Fizz, multiples of 5 with Buzz and multiples of both with FizzBuzz.",
		check: "sh fizz.sh",
		want:  "1\n2\nFizz\n4\nBuzz\nFizz\n7\n8\nFizz\nBuzz\n11\nFizz\n13\n14\nFizzBuzz",
	},
	{
		name: "wordfreq",
		prompt: "Create wordfreq.js in the current directory, for Node with no dependencies. It reads text from standard input " +
			"and prints the three most frequent words, lower-cased, one per line as '<word> <count>', most frequent first, " +
			"ties broken alphabetically.",
		check: "printf 'B a a c b A d d d d c\\n' | node wordfreq.js",
		want:  "d 4\na 3\nb 2",
	},
	{
		name: "median",
		prompt: "median.js in the current directory should print the median of the numbers given as arguments (the mean of the " +
			"two middle values when there is an even count), but it gives wrong answers. Fix it.",
		seed: map[string]string{"median.js": "const nums = process.argv.slice(2).map(Number);\nnums.sort();\n" +
			"const mid = Math.floor(nums.length / 2);\nconsole.log(nums[mid]);\n"},
		check: "node median.js 10 2 33 4; node median.js 5 100 9; node median.js 3 1",
		want:  "7\n9\n2",
	},
	{
		name: "totals",
		prompt: "orders.csv in the current directory has a header line and then customer,amount rows. Create total.sh, a POSIX " +
			"shell script that prints each customer's total as '<customer> <total>', one per line, sorted by customer name.",
		seed:  map[string]string{"orders.csv": "customer,amount\nmira,12\nabe,5\nmira,8\nzed,3\nabe,10\n"},
		check: "sh total.sh",
		want:  "abe 15\nmira 20\nzed 3",
	},
	{
		name:   "calc",
		prompt: "Make `node test.js` pass in the current directory by changing calc.js. Do not change test.js.",
		seed: map[string]string{
			"calc.js": "exports.add = (a, b) => a + b;\n",
			"test.js": "const assert = require('assert');\nconst calc = require('./calc');\n" +
				"assert.strictEqual(calc.add(2, 3), 5);\nassert.strictEqual(calc.sub(9, 4), 5);\n" +
				"assert.strictEqual(calc.mul(6, 7), 42);\nassert.throws(() => calc.div(1, 0), /division by zero/);\n" +
				"assert.strictEqual(calc.div(9, 3), 3);\nconsole.log('ok');\n",
		},
		check:  "node test.js",
		want:   "ok",
		frozen: "test.js",
	},
}

// checkOverall gives every system the same small coding tasks. The systems run
// side by side on each task, so none of them gets a quieter API than another.
func (b *bench) checkOverall(ctx context.Context, systems []string) {
	fmt.Println("overall: small coding tasks, each with a check")
	for _, task := range codingTasks {
		results := make([]trial, len(systems))
		var wg sync.WaitGroup
		for i, sys := range systems {
			wg.Add(1)
			go func(i int, sys string) {
				defer wg.Done()
				results[i] = b.runCodingTask(ctx, strings.TrimSpace(sys), task)
			}(i, sys)
		}
		wg.Wait()
		for _, t := range results {
			b.add(t)
		}
	}
}

func (b *bench) runCodingTask(ctx context.Context, sys string, task codingTask) trial {
	t := trial{Check: "overall", System: sys, Task: task.name}
	var dir string
	switch sys {
	case "A", "B":
		dir = filepath.Join(b.scratch, "overall", task.name+"-"+sys)
	case "C", "D":
		dir = b.conversationDir(agentFor(sys), b.conversation(task.name+"-"+sys))
	default:
		t.Infra = "unknown system " + sys
		return t
	}
	if err := seed(dir, task.seed); err != nil {
		t.Infra = err.Error()
		return t
	}

	switch sys {
	case "A", "B":
		res, err := b.runClaude(ctx, "overall-"+task.name+"-"+strings.ToLower(sys), dir, task.prompt, sys == "B")
		t.CostUSD, t.Usage, t.WallMS, t.Handoffs = res.CostUSD, res.Usage, res.WallMS, res.Handoffs
		if err != nil {
			t.Infra = err.Error()
			return t
		}
	default:
		w, err := b.ask(ctx, agentFor(sys), b.conversation(task.name+"-"+sys), task.prompt)
		t.CostUSD, t.Usage, t.WallMS, t.Handoffs = w.CostUSD, w.Usage, w.WallMS, len(w.Runs)
		if err != nil {
			t.Infra = err.Error()
			return t
		}
	}

	out, err := b.runCheck(ctx, dir, task.check)
	got := strings.TrimSpace(out)
	switch {
	case err != nil:
		t.Note = "check failed: " + err.Error()
	case got != task.want:
		t.Note = fmt.Sprintf("check printed %q", firstLine(got, 60))
	case task.frozen != "" && !unchanged(dir, task.frozen, task.seed[task.frozen]):
		t.Note = task.frozen + " was changed"
	default:
		t.Pass = true
	}
	return t
}

func agentFor(sys string) string {
	if sys == "D" {
		return callerAgent
	}
	return soloAgent
}

// conversation names a conversation for this run, so no run meets another's
// files or session.
func (b *bench) conversation(name string) string {
	return "qb-" + b.runID + "-" + strings.ToLower(name)
}

func seed(dir string, files map[string]string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o640); err != nil {
			return fmt.Errorf("seed %s: %w", name, err)
		}
	}
	return nil
}

func unchanged(dir, name, want string) bool {
	got, err := os.ReadFile(filepath.Join(dir, name))
	return err == nil && string(got) == want
}

// ---- delegation: hand off what a routine covers, and only that -------------

type delegationCase struct {
	name    string
	request string
	answer  string // text the final answer must contain
	handOff bool   // whether a routine covers the request
}

// The covered work is a named procedure only the routine knows, the shape
// recurring work takes: the request says what, the routine holds how. The
// skill-less agent is given the same requests and has no way to know the
// procedure, so its failures there measure the knowledge a routine carries,
// not the agent. (Earlier versions used work an agent can do in one command;
// no agent handed that off, and rightly — doing it was cheaper than asking.)
var delegationCases = []delegationCase{
	{"covered-1", ledgerRequest("lantern"), ledgerChecksum("lantern"), true},
	{"covered-2", ledgerRequest("harbor"), ledgerChecksum("harbor"), true},
	{"covered-3", ledgerRequest("window"), ledgerChecksum("window"), true},
	{"uncovered-1", "What is 17 times 23? Answer with the number.", "391", false},
	{"uncovered-2", "Write the word 'river' in upper case.", "RIVER", false},
	// A near miss: it reads like the routine's work, and is not. The routine
	// makes a ledger for a word; this asks for a plain checksum of one.
	{"near-miss", "Tell me the short SHA-256 checksum of the word 'ledger' itself: the first 12 hex characters, no trailing newline.",
		shortSHA256([]byte("ledger")), false},
}

func ledgerRequest(word string) string {
	return "Make the word ledger for '" + word + "' and tell me the ledger's checksum."
}

// ledgerChecksum is what the qb-ledger routine's procedure yields for a word:
// one line per letter, the letter repeated by its position, hashed.
func ledgerChecksum(word string) string {
	var b strings.Builder
	for i, letter := range word {
		b.WriteString(strings.Repeat(string(letter), i+1) + "\n")
	}
	return shortSHA256([]byte(b.String()))
}

func shortSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12]
}

// checkDelegation asks the routine-holding agent for work a routine covers,
// work none covers, and a near miss. It passes a case when the hand-off
// decision was the right one and the answer was right. The skill-less agent
// does the same requests itself, as the cost and accuracy to compare with.
func (b *bench) checkDelegation(ctx context.Context) {
	fmt.Println("delegation: requests a routine covers, does not cover, and nearly covers")
	for _, c := range delegationCases {
		var solo, caller trial
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			solo = b.runDelegationCase(ctx, "C", c)
		}()
		go func() {
			defer wg.Done()
			caller = b.runDelegationCase(ctx, "D", c)
		}()
		wg.Wait()
		b.add(solo)
		b.add(caller)
	}
}

func (b *bench) runDelegationCase(ctx context.Context, sys string, c delegationCase) trial {
	t := trial{Check: "delegation", System: sys, Task: c.name}
	w, err := b.ask(ctx, agentFor(sys), b.conversation("d-"+c.name+"-"+sys), c.request)
	t.CostUSD, t.Usage, t.WallMS, t.Handoffs = w.CostUSD, w.Usage, w.WallMS, len(w.Runs)
	if err != nil {
		t.Infra = err.Error()
		return t
	}
	right := strings.Contains(strings.ToLower(w.Final), strings.ToLower(c.answer))
	if sys == "C" {
		t.Pass = right
		if !right {
			t.Note = "wrong answer: " + firstLine(w.Final, 60)
		}
		return t
	}

	handed := len(w.Runs) > 0
	var notes []string
	switch {
	case handed && !c.handOff:
		notes = append(notes, "handed off work no routine covers ("+w.Runs[0].RoutineID+")")
	case !handed && c.handOff:
		notes = append(notes, "did the work itself though a routine covers it")
	case handed:
		r := w.Runs[0]
		stated := !r.Expect.Empty()
		notes = append(notes, fmt.Sprintf("handed to %s, run %s, own check stated: %v", r.RoutineID, r.Status, stated))
	default:
		notes = append(notes, "held back")
	}
	if !right {
		notes = append(notes, "wrong answer: "+firstLine(w.Final, 60))
	}
	t.Pass = handed == c.handOff && right
	t.Note = strings.Join(notes, "; ")
	return t
}

// ---- context: what a long delegated result adds to the delegator -----------

const longEntries = 300

// checkContext hands off work whose result is long, and measures how much of
// it lands in the delegator's context. For Claude Code that is the text its
// sub-agent returned; for roundclaw it is the message the agent is woken with,
// which should be a bounded head and the path of a file holding the rest.
func (b *bench) checkContext(ctx context.Context) {
	fmt.Println("context: what a long delegated result adds to the delegator")
	last := fmt.Sprintf("entry %d", longEntries)
	var claudeTrial, fleetTrial trial
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		t := trial{Check: "context", System: "B", Task: "long-result"}
		prompt := fmt.Sprintf("Use a sub-agent (the Agent tool) to produce the lines 'entry 1' through 'entry %d', one per line, "+
			"every line written out, and to return all of them to you. Then tell me exactly what the last line says.", longEntries)
		res, err := b.runClaude(ctx, "context-b", filepath.Join(b.scratch, "context", "b"), prompt, true)
		t.CostUSD, t.Usage, t.WallMS, t.Handoffs, t.DeliveredChars = res.CostUSD, res.Usage, res.WallMS, res.Handoffs, res.ReturnedChars
		switch {
		case err != nil:
			t.Infra = err.Error()
		case res.Handoffs == 0:
			t.Note = "did not use a sub-agent"
		case !strings.Contains(res.Text, last):
			t.Note = "wrong answer: " + firstLine(res.Text, 60)
		default:
			t.Pass = true
		}
		claudeTrial = t
	}()

	go func() {
		defer wg.Done()
		t := trial{Check: "context", System: "D", Task: "long-result"}
		conv := b.conversation("context-d")
		request := fmt.Sprintf("Hand this to the qb-long routine with n set to %d. When its result arrives, tell me exactly "+
			"what its last line says.", longEntries)
		w, err := b.ask(ctx, callerAgent, conv, request)
		t.CostUSD, t.Usage, t.WallMS, t.Handoffs = w.CostUSD, w.Usage, w.WallMS, len(w.Runs)
		// Every turn after the first is one the agent was woken for: its request
		// is what the hand-off delivered.
		for _, turn := range w.Turns[min(1, len(w.Turns)):] {
			t.DeliveredChars += len([]rune(turn.Request))
		}
		kept, _ := filepath.Glob(filepath.Join(b.conversationDir(callerAgent, conv), "inbox", "results", "*"))
		switch {
		case err != nil:
			t.Infra = err.Error()
		case len(w.Runs) == 0:
			t.Note = "did not hand off"
		case len(kept) == 0:
			t.Note = "the full result was not kept as a file"
		case !strings.Contains(w.Final, last):
			t.Note = "wrong answer: " + firstLine(w.Final, 60)
		default:
			t.Pass = true
			t.Note = "full result kept at inbox/results/" + filepath.Base(kept[0])
		}
		fleetTrial = t
	}()

	wg.Wait()
	b.add(claudeTrial)
	b.add(fleetTrial)
}

// ---- durability: a worker killed during a routine run ----------------------

// checkDurability starts a slow routine run that will wake an agent, kills the
// worker while the step is running, starts it again, and passes when the run
// still finishes and the agent is woken for it exactly once.
func (b *bench) checkDurability(ctx context.Context) {
	fmt.Println("durability: a worker killed during a routine run")
	t := trial{Check: "durability", System: "D", Task: "kill-worker"}
	defer func() { b.add(t) }()
	started := time.Now()
	conv := b.conversation("durability")

	var startedRun struct {
		RunID int64 `json:"run_id"`
	}
	if _, err := b.fleet.call(ctx, http.MethodPost, "/v1/routines/qb-slow/runs", map[string]any{
		"inputs": map[string]string{"seconds": "25"},
		"notify": map[string]string{"agent": callerAgent, "conversation": conv},
	}, &startedRun); err != nil {
		t.Infra = err.Error()
		return
	}
	t.Handoffs = 1

	// Kill only once the step is running, so the kill lands on work in progress.
	if err := b.until(ctx, 90*time.Second, func() (bool, error) {
		v, err := b.getRun(ctx, startedRun.RunID)
		return err == nil && len(v.Steps) > 0, err
	}); err != nil {
		t.Infra = "the step never started: " + err.Error()
		return
	}
	time.Sleep(5 * time.Second)
	if err := shell(b.killCmd); err != nil {
		t.Infra = "kill worker: " + err.Error()
		return
	}
	time.Sleep(3 * time.Second)
	if err := shell(b.startCmd); err != nil {
		t.Infra = "start worker: " + err.Error()
		return
	}

	var final runView
	if err := b.until(ctx, b.timeout, func() (bool, error) {
		v, err := b.getRun(ctx, startedRun.RunID)
		final = v
		return err == nil && runFinished(v.Run.Status), err
	}); err != nil {
		t.Infra = "the run never finished after the worker came back: " + err.Error()
		return
	}
	t.CostUSD, t.Usage = final.Run.CostUSD, final.Run.Usage

	// The wake-up names the run. Wait for it, then wait longer: a second
	// delivery would arrive late, and "exactly once" is the claim.
	marker := fmt.Sprintf("run %d ", startedRun.RunID)
	wakes := func() (int, error) {
		turns, err := b.turns(ctx, callerAgent, conv)
		n := 0
		for _, turn := range turns {
			if strings.Contains(turn.Request, marker) {
				n++
			}
		}
		return n, err
	}
	if err := b.until(ctx, 2*time.Minute, func() (bool, error) {
		n, err := wakes()
		return n > 0, err
	}); err != nil {
		t.Note = "the run finished but the agent was never woken"
		t.WallMS = time.Since(started).Milliseconds()
		return
	}
	time.Sleep(20 * time.Second)
	n, err := wakes()
	t.WallMS = time.Since(started).Milliseconds()
	switch {
	case err != nil:
		t.Infra = err.Error()
	case final.Run.Status != registry.RunPassed:
		t.Note = fmt.Sprintf("the run ended %s: %s", final.Run.Status, final.Run.Reason)
	case n != 1:
		t.Note = fmt.Sprintf("the agent was woken %d times", n)
	default:
		t.Pass = true
		t.Note = fmt.Sprintf("run %d passed after the kill; woken once", startedRun.RunID)
	}
}

// until polls a condition every two seconds up to a limit. An error from the
// condition is transient — the API is briefly unreachable while a service
// restarts — so it is kept and polling goes on.
func (b *bench) until(ctx context.Context, limit time.Duration, cond func() (bool, error)) error {
	deadline := time.Now().Add(limit)
	var lastErr error
	for time.Now().Before(deadline) {
		ok, err := cond()
		if ok {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if lastErr != nil {
		return fmt.Errorf("gave up after %s: %w", limit, lastErr)
	}
	return fmt.Errorf("gave up after %s", limit)
}

func shell(command string) error {
	out, err := exec.Command("sh", "-c", command).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, firstLine(string(out), 160))
	}
	return nil
}
