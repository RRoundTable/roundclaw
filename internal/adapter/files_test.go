package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/roundtable/roundclaw/internal/claude"
	"github.com/roundtable/roundclaw/internal/config"
)

// Upload names come from whoever sent the file, so they are hostile input. A
// name that escaped the inbox would let one request write anywhere the worker
// can reach.
func TestSanitizeFilenameCannotEscape(t *testing.T) {
	hostile := []string{
		"../../etc/passwd",
		"..\\..\\windows\\system32\\config",
		"/etc/shadow",
		"....//....//etc/passwd",
		"a/b/c.txt",
		".ssh/authorized_keys",
	}
	for _, name := range hostile {
		got := sanitizeFilename(name)
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("sanitizeFilename(%q) = %q, still contains a separator", name, got)
		}
		if strings.HasPrefix(got, ".") {
			t.Errorf("sanitizeFilename(%q) = %q, still starts with a dot", name, got)
		}
		// The decisive property: joining it must stay inside the inbox.
		joined := filepath.Join("/workspace/inbox", got)
		if !strings.HasPrefix(joined, "/workspace/inbox/") {
			t.Errorf("sanitizeFilename(%q) escaped: %q", name, joined)
		}
	}
}

func TestSanitizeFilenameKeepsUsableNames(t *testing.T) {
	cases := map[string]string{
		"report.pdf":     "report.pdf",
		"my notes.txt":   "my notes.txt",
		"보고서.pdf":        "보고서.pdf",
		"data-2026.csv":  "data-2026.csv",
		"":               "upload",
		"   ":            "upload",
		"...":            "upload",
		"weird\x00name":  "weirdname",
		"tab\tseparated": "tabseparated",
	}
	for in, want := range cases {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeFilenameBoundsLength(t *testing.T) {
	got := sanitizeFilename(strings.Repeat("x", 500) + ".pdf")
	if len(got) > 120 {
		t.Errorf("name is %d characters, want at most 120", len(got))
	}
	if !strings.HasSuffix(got, ".pdf") {
		t.Errorf("extension lost when truncating: %q", got)
	}
}

func stagingDispatcher(t *testing.T) (*Dispatcher, *config.Config) {
	t.Helper()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "roundclaw.yaml")
	body := "workspace_root: ws\ncontainer:\n  image: test\nagents:\n  - id: pm\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return NewDispatcher(cfg, &fakeTemporal{}, nil, nil), cfg
}

// The bug this pins: uploads were written straight into the agent's work
// directory, which is only what /workspace resolves to for the default
// conversation. A thread mounts its own workspace, so the file was announced at
// a path that did not exist there — and the turn ran and was billed anyway.
//
// Staging cannot be inside any workspace for a second reason: resolveWorkspace
// treats an existing conversation directory as one an earlier turn prepared, so
// creating it here would skip the git worktree and the CLAUDE.md seed.
func TestStageAttachmentsKeepsUploadsOutOfTheWorkspace(t *testing.T) {
	disp, cfg := stagingDispatcher(t)

	staged, err := disp.StageAttachments("pm", []Attachment{
		{Name: "report.pdf", Body: strings.NewReader("body"), Size: 4},
	})
	if err != nil {
		t.Fatalf("StageAttachments: %v", err)
	}
	if len(staged.HostPaths) != 1 || len(staged.ContainerPaths) != 1 {
		t.Fatalf("staged %d host / %d container paths, want 1 of each",
			len(staged.HostPaths), len(staged.ContainerPaths))
	}

	work := cfg.WorkDir("pm")
	if strings.HasPrefix(staged.HostPaths[0], work+string(os.PathSeparator)) {
		t.Errorf("upload was written inside the work directory (%s); a thread's "+
			"workspace is somewhere else entirely", staged.HostPaths[0])
	}
	if _, err := os.Stat(staged.HostPaths[0]); err != nil {
		t.Errorf("staged file is not on disk: %v", err)
	}
}

// The container path is a promise the worker keeps by linking the staged file
// into the workspace under the same name. If the two names drift, placement puts
// the file somewhere the prompt never mentioned.
func TestStageAttachmentsPromisesAPlaceableContainerPath(t *testing.T) {
	disp, _ := stagingDispatcher(t)

	staged, err := disp.StageAttachments("pm", []Attachment{
		{Name: "보고서.pdf", Body: strings.NewReader("a"), Size: 1},
		{Name: "../escape.csv", Body: strings.NewReader("b"), Size: 1},
	})
	if err != nil {
		t.Fatalf("StageAttachments: %v", err)
	}

	for i, container := range staged.ContainerPaths {
		wantDir := filepath.Join(claude.ContainerWorkspace, inboxDir)
		if filepath.Dir(container) != wantDir {
			t.Errorf("container path %q is not under %s", container, wantDir)
		}
		if got, want := filepath.Base(container), filepath.Base(staged.HostPaths[i]); got != want {
			t.Errorf("staged name %q and promised name %q differ; placement would "+
				"put the file at a path the agent was never told", want, got)
		}
	}
}

// Files that arrive together share a batch prefix, so two uploads named the same
// thing cannot overwrite each other — in staging or in the workspace.
func TestStageAttachmentsKeepsSameNamedUploadsApart(t *testing.T) {
	disp, _ := stagingDispatcher(t)

	first, err := disp.StageAttachments("pm", []Attachment{
		{Name: "report.pdf", Body: strings.NewReader("one"), Size: 3},
	})
	if err != nil {
		t.Fatalf("stage first: %v", err)
	}
	second, err := disp.StageAttachments("pm", []Attachment{
		{Name: "report.pdf", Body: strings.NewReader("two"), Size: 3},
	})
	if err != nil {
		t.Fatalf("stage second: %v", err)
	}

	if first.HostPaths[0] == second.HostPaths[0] {
		t.Fatal("two uploads of the same name collided")
	}
	got, err := os.ReadFile(first.HostPaths[0])
	if err != nil {
		t.Fatalf("read first: %v", err)
	}
	if string(got) != "one" {
		t.Errorf("the first upload was overwritten: %q", got)
	}
}

// The agent is told where files are, not given their contents — a large upload
// must not end up copied into the prompt, the turn record and the logs.
func TestPromptWithAttachments(t *testing.T) {
	if got := PromptWithAttachments("hello", nil); got != "hello" {
		t.Errorf("no attachments changed the prompt: %q", got)
	}

	got := PromptWithAttachments("summarise these", []string{
		"/workspace/inbox/ab12-a.pdf",
		"/workspace/inbox/ab12-b.csv",
	})
	for _, want := range []string{"/workspace/inbox/ab12-a.pdf", "/workspace/inbox/ab12-b.csv", "summarise these"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt is missing %q:\n%s", want, got)
		}
	}
}
