package claude

import (
	"path/filepath"
	"strings"
	"testing"
)

// A deny path that escaped would mount /dev/null over a host file — turning a
// protection into a way to destroy something outside the workspace.
func TestValidateDenyPathRejectsEscapes(t *testing.T) {
	for _, bad := range []string{
		"", "   ",
		"/etc/passwd",
		"..",
		"../secrets",
		"../../etc/shadow",
		"a/../../../etc/passwd",
	} {
		if err := ValidateDenyPath(bad); err == nil {
			t.Errorf("deny path %q was accepted", bad)
		}
	}
}

func TestValidateDenyPathAcceptsWorkspaceRelative(t *testing.T) {
	for _, good := range []string{"env", ".env", "data/kis.json", "config/secrets.yaml"} {
		if err := ValidateDenyPath(good); err != nil {
			t.Errorf("deny path %q was rejected: %v", good, err)
		}
	}
}

func TestArgsShadowsDenyPaths(t *testing.T) {
	s := baseSpec()
	s.DenyPaths = []string{"env", ".env", "data/kis.json"}
	args, err := s.Args()
	if err != nil {
		t.Fatalf("args: %v", err)
	}
	for _, want := range []string{
		"/dev/null:/workspace/env:ro",
		"/dev/null:/workspace/.env:ro",
		"/dev/null:/workspace/data/kis.json:ro",
	} {
		if !hasPair(args, "-v", want) {
			t.Errorf("missing shadow mount %q in %v", want, args)
		}
	}

	// The shadow must land after the workspace mount or it would be covered by
	// it rather than the other way round.
	workspaceAt, shadowAt := -1, -1
	for i, a := range args {
		if a == "/srv/agent/work:/workspace" {
			workspaceAt = i
		}
		if strings.HasPrefix(a, "/dev/null:/workspace/env") {
			shadowAt = i
		}
	}
	if workspaceAt < 0 || shadowAt < 0 || shadowAt < workspaceAt {
		t.Errorf("shadow mount at %d must come after the workspace mount at %d", shadowAt, workspaceAt)
	}
}

// An agent pointed at a real project must work in that directory, not the
// managed one, or its edits would go somewhere nobody looks.
func TestArgsUsesTheGivenWorkDir(t *testing.T) {
	s := baseSpec()
	s.WorkDir = "/home/user/my-project"
	args, err := s.Args()
	if err != nil {
		t.Fatalf("args: %v", err)
	}
	if !hasPair(args, "-v", "/home/user/my-project:"+ContainerWorkspace) {
		t.Errorf("project directory not mounted at the workspace: %v", args)
	}
	// Read-write: no :ro suffix, unlike additional_dirs.
	for _, a := range args {
		if strings.HasPrefix(a, "/home/user/my-project:") && strings.HasSuffix(a, ":ro") {
			t.Errorf("work dir was mounted read-only: %q", a)
		}
	}
	if filepath.Clean(ContainerWorkspace) != "/workspace" {
		t.Fatal("container workspace path changed; --resume scoping depends on it")
	}
}

func TestArgsRejectsEscapingDenyPath(t *testing.T) {
	s := baseSpec()
	s.DenyPaths = []string{"../../etc/passwd"}
	if _, err := s.Args(); err == nil {
		t.Fatal("an escaping deny path reached the container runtime")
	}
}
