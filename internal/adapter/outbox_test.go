package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// outboxWith builds a workspace containing an outbox with the given files, and
// returns the workspace root.
func outboxWith(t *testing.T, files map[string]string) string {
	t.Helper()
	ws := t.TempDir()
	outbox := filepath.Join(ws, outboxDir)
	if err := os.MkdirAll(outbox, 0o750); err != nil {
		t.Fatalf("create outbox: %v", err)
	}
	for name, body := range files {
		path := filepath.Join(outbox, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("create dir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return ws
}

// The name comes from the agent, so it is hostile input. A path that escaped the
// outbox would put anything the gateway can read into a Discord channel.
func TestResolveOutFileCannotEscapeTheOutbox(t *testing.T) {
	ws := outboxWith(t, map[string]string{"ok.txt": "fine"})

	// A file the agent must not be able to reach by any spelling.
	secret := filepath.Join(ws, ".env")
	if err := os.WriteFile(secret, []byte("SECRET=1"), 0o640); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	hostile := []string{
		"../.env",
		"../../etc/passwd",
		"/etc/shadow",
		"..",
		".",
		"",
		"   ",
		"subdir/../../.env",
		"./../.env",
	}
	for _, name := range hostile {
		got, _, err := resolveOutFile(ws, name)
		if err == nil {
			t.Errorf("resolveOutFile(%q) was accepted and resolved to %q", name, got)
		}
	}
}

// The check that a lexical one cannot make. The agent has Write, so it can
// create a link inside its own outbox that points anywhere the gateway can read.
func TestResolveOutFileRefusesASymlinkOutOfTheOutbox(t *testing.T) {
	ws := outboxWith(t, nil)

	secret := filepath.Join(ws, ".env")
	if err := os.WriteFile(secret, []byte("SECRET=1"), 0o640); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	link := filepath.Join(ws, outboxDir, "notes.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if got, _, err := resolveOutFile(ws, "notes.txt"); err == nil {
		t.Errorf("a symlink out of the outbox was followed to %q — the workspace's "+
			".env would have been posted to Discord", got)
	}
}

// A link that stays inside the outbox is not an escape and has no reason to be
// refused.
func TestResolveOutFileAllowsASymlinkInsideTheOutbox(t *testing.T) {
	ws := outboxWith(t, map[string]string{"real.pdf": "body"})

	link := filepath.Join(ws, outboxDir, "alias.pdf")
	if err := os.Symlink(filepath.Join(ws, outboxDir, "real.pdf"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, _, err := resolveOutFile(ws, "alias.pdf"); err != nil {
		t.Errorf("a link within the outbox was refused: %v", err)
	}
}

func TestResolveOutFileAcceptsOrdinaryNames(t *testing.T) {
	ws := outboxWith(t, map[string]string{
		"report.pdf":       "body",
		"보고서.csv":          "body",
		"nested/chart.png": "body",
	})

	for _, name := range []string{
		"report.pdf",
		"보고서.csv",
		"nested/chart.png",
		// The prompt talks about /workspace/outbox, so writing the prefix out is
		// the obvious mistake and is tolerated rather than refused.
		"outbox/report.pdf",
	} {
		path, size, err := resolveOutFile(ws, name)
		if err != nil {
			t.Errorf("resolveOutFile(%q): %v", name, err)
			continue
		}
		if !strings.HasPrefix(path, filepath.Join(ws, outboxDir)+string(os.PathSeparator)) {
			t.Errorf("resolveOutFile(%q) = %q, which is outside the outbox", name, path)
		}
		if size != 4 {
			t.Errorf("resolveOutFile(%q) size = %d, want 4", name, size)
		}
	}
}

// A FIFO would hold the upload open forever and a directory is not a file. An
// agent never means either one.
func TestResolveOutFileRefusesWhatIsNotARegularFile(t *testing.T) {
	ws := outboxWith(t, map[string]string{"sub/inner.txt": "x"})

	if _, _, err := resolveOutFile(ws, "sub"); err == nil {
		t.Error("a directory was accepted as a file to send")
	}
}

func TestResolveOutFileRefusesEmptyAndOversizeFiles(t *testing.T) {
	ws := outboxWith(t, map[string]string{"empty.txt": ""})

	if _, _, err := resolveOutFile(ws, "empty.txt"); err == nil {
		t.Error("an empty file was accepted; Discord rejects it and the agent gets no reason")
	}

	big := filepath.Join(ws, outboxDir, "big.bin")
	if err := os.WriteFile(big, make([]byte, MaxOutboundBytes+1), 0o640); err != nil {
		t.Fatalf("write oversize: %v", err)
	}
	_, _, err := resolveOutFile(ws, "big.bin")
	if err == nil {
		t.Fatal("an oversize file was accepted; discord would reject it")
	}
	if !strings.Contains(err.Error(), "MB") {
		t.Errorf("the size error does not say how big it is: %v", err)
	}
}

func TestResolveOutFileReportsAMissingOutbox(t *testing.T) {
	ws := t.TempDir() // no outbox at all

	_, _, err := resolveOutFile(ws, "report.pdf")
	if err == nil {
		t.Fatal("a file was resolved in a workspace with no outbox")
	}
	if !strings.Contains(err.Error(), "outbox") {
		t.Errorf("the error does not tell the agent where to put the file: %v", err)
	}
}

// A rejected request must not leave the files it already opened behind.
func TestOpenOutboundClosesWhatItOpenedOnFailure(t *testing.T) {
	ws := outboxWith(t, map[string]string{"good.txt": "body"})

	files, err := openOutbound(ws, []string{"good.txt", "missing.txt"})
	if err == nil {
		t.Fatal("a batch with a missing file was accepted")
	}
	if files != nil {
		t.Errorf("failed openOutbound still returned %d file(s)", len(files))
	}
}

func TestOpenOutboundRefusesMoreThanDiscordAccepts(t *testing.T) {
	ws := outboxWith(t, nil)

	names := make([]string, MaxOutboundFiles+1)
	for i := range names {
		names[i] = "f.txt"
	}
	if _, err := openOutbound(ws, names); err == nil {
		t.Error("more files than Discord accepts per message were allowed through")
	}
}

func TestOpenOutboundWithNoNames(t *testing.T) {
	files, err := openOutbound(t.TempDir(), nil)
	if err != nil || files != nil {
		t.Errorf("openOutbound(nil) = %v, %v; want no files and no error", files, err)
	}
}
