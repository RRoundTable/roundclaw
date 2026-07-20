package adapter

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/roundtable/roundclaw/internal/claude"
)

// File input.
//
// Attachments are written into the agent's workspace and named in the prompt,
// rather than being inlined into it. The agent already has Read; handing it a
// path lets it decide how much of a file to pull in, and keeps a 20MB PDF out
// of the request text that gets stored, logged and replayed.
//
// Everything lands under inbox/ inside the agent's own work directory, so one
// agent can never read another's uploads.

const (
	// MaxAttachmentBytes caps a single upload. Discord's own limit is 25MB for
	// most accounts; matching it keeps the failure on their side, where the
	// user gets a clear message, rather than ours.
	MaxAttachmentBytes = 25 << 20
	// MaxAttachments per request. Enough for a handful of files, low enough
	// that a runaway client cannot fill the disk in one call.
	MaxAttachments = 8

	inboxDir = "inbox"
)

// Attachment is one uploaded file, still in transit.
type Attachment struct {
	Name string
	Body io.Reader
	// Size is advisory; the copy is capped regardless, because a caller can
	// lie about it or not know.
	Size int64
}

// SaveAttachments writes uploads into an agent's workspace and returns the
// paths as the container will see them.
func (d *Dispatcher) SaveAttachments(agentID string, files []Attachment) ([]string, error) {
	if len(files) == 0 {
		return nil, nil
	}
	if len(files) > MaxAttachments {
		return nil, fmt.Errorf("too many files: %d, limit is %d", len(files), MaxAttachments)
	}

	hostDir := filepath.Join(d.cfg.WorkDir(agentID), inboxDir)
	if err := os.MkdirAll(hostDir, 0o750); err != nil {
		return nil, fmt.Errorf("create inbox: %w", err)
	}

	// One prefix per request, so files that arrive together stay together and
	// two uploads of "report.pdf" cannot overwrite each other.
	batch, err := randomToken()
	if err != nil {
		return nil, err
	}

	var paths []string
	for _, f := range files {
		name := batch + "-" + sanitizeFilename(f.Name)
		hostPath := filepath.Join(hostDir, name)

		if err := writeCapped(hostPath, f.Body); err != nil {
			return nil, fmt.Errorf("save %s: %w", f.Name, err)
		}
		paths = append(paths, filepath.Join(claude.ContainerWorkspace, inboxDir, name))
	}
	return paths, nil
}

// writeCapped copies at most MaxAttachmentBytes and removes the partial file if
// the source is longer, so an oversized upload cannot land half-written and be
// read as though it were complete.
func writeCapped(path string, r io.Reader) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()

	// One byte over the limit is enough to detect the overflow.
	n, err := io.Copy(f, io.LimitReader(r, MaxAttachmentBytes+1))
	if err != nil {
		os.Remove(path)
		return err
	}
	if n > MaxAttachmentBytes {
		os.Remove(path)
		return fmt.Errorf("file is larger than %d bytes", MaxAttachmentBytes)
	}
	return nil
}

// sanitizeFilename reduces a caller-supplied name to something safe to join
// onto a path.
//
// The name comes from whoever uploaded the file, so it is treated as hostile:
// only the base name survives, separators and control characters are dropped,
// and a name that reduces to nothing gets a placeholder rather than becoming
// the directory itself.
func sanitizeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))

	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '/' || r == '\\' || r == 0:
			// dropped
		case unicode.IsControl(r):
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	clean := strings.TrimSpace(b.String())
	clean = strings.TrimLeft(clean, ".")

	if clean == "" {
		return "upload"
	}
	// Long names are legal but unhelpful, and the batch prefix needs room.
	const maxName = 120
	if len(clean) > maxName {
		ext := filepath.Ext(clean)
		if len(ext) > 16 {
			ext = ""
		}
		clean = clean[:maxName-len(ext)] + ext
	}
	return clean
}

// PromptWithAttachments prefixes a request with the paths of its uploads.
//
// The agent is told where the files are rather than being given their contents:
// it has Read, and letting it choose how much to pull in is what keeps a large
// upload from being copied into the prompt, the turn record and every log line
// that quotes it.
func PromptWithAttachments(text string, paths []string) string {
	if len(paths) == 0 {
		return text
	}
	var b strings.Builder
	b.WriteString("Files attached to this request, already saved in your workspace:\n")
	for _, p := range paths {
		fmt.Fprintf(&b, "  %s\n", p)
	}
	b.WriteString("\n")
	b.WriteString(text)
	return b.String()
}

func randomToken() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate upload id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
