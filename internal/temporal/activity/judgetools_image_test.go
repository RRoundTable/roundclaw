package activity

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"
)

// Does the deny list actually leave the judge with nothing?
//
// judgeDeniedTools is a set of names, not a policy, so it is only as complete as
// the CLI version it was written against. Nothing in the build fails when the
// image gains a tool the list does not mention — the judge simply gets it. This
// asks the image itself what it offers and fails if anything survives.
//
// It runs a container and spends a model call, so it is opt-in rather than part
// of the suite. Run it after changing the image or the CLI version:
//
//	ROUNDCLAW_IMAGE_TEST=roundclaw/claude:latest \
//	CLAUDE_CODE_OAUTH_TOKEN=$(docker exec roundclaw-worker-1 printenv CLAUDE_CODE_OAUTH_TOKEN) \
//	go test ./internal/temporal/activity -run TestJudgeDeniesEveryToolTheImageOffers -v
//
// The credential passes by name, as it does in production: Args puts it in the
// container's environment with -e and never in argv.
func TestJudgeDeniesEveryToolTheImageOffers(t *testing.T) {
	image := os.Getenv("ROUNDCLAW_IMAGE_TEST")
	if image == "" {
		t.Skip("set ROUNDCLAW_IMAGE_TEST to the agent image to run this against a real container")
	}
	credEnv := ""
	for _, name := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		if os.Getenv(name) != "" {
			credEnv = name
			break
		}
	}
	if credEnv == "" {
		t.Skip("no CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY in the environment")
	}

	// The control run says what the image offers when nothing is denied. Asking
	// the image beats hard-coding a list here, which would rot the same way
	// judgeDeniedTools can.
	offered := toolsOffered(t, image, credEnv, nil)
	if len(offered) == 0 {
		t.Fatal("the image offered no tools even with nothing denied; the probe is broken, not the deny list")
	}
	t.Logf("image offers %d tools: %v", len(offered), offered)

	left := toolsOffered(t, image, credEnv, judgeDeniedTools)
	if len(left) > 0 {
		t.Errorf("the judge is still offered %d tool(s): %v\nadd them to judgeDeniedTools", len(left), left)
	}
}

// toolsOffered reads the tool list out of the CLI's init event, which names
// every tool the model is shown before it does anything.
func toolsOffered(t *testing.T, image, credEnv string, deny []string) []string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	args := []string{
		"run", "--rm", "-e", credEnv, image,
		// The prompt goes immediately after -p: --disallowedTools is variadic and
		// would otherwise swallow it, exactly as Args documents for --allowedTools.
		"claude", "-p", "Reply with the single word: ok",
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json", "--verbose",
	}
	if len(deny) > 0 {
		args = append(args, "--disallowedTools", strings.Join(deny, ","))
	}

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), credEnv+"="+os.Getenv(credEnv))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run image: %v", err)
	}

	for _, line := range strings.Split(string(out), "\n") {
		var ev struct {
			Type    string   `json:"type"`
			Subtype string   `json:"subtype"`
			Tools   []string `json:"tools"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev.Type == "system" && ev.Subtype == "init" {
			sort.Strings(ev.Tools)
			return ev.Tools
		}
	}
	t.Fatal("no init event in the CLI output; cannot tell what tools were offered")
	return nil
}
