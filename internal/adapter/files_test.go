package adapter

import (
	"path/filepath"
	"strings"
	"testing"
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
