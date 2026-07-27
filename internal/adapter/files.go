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
// Everything lands under inbox/ inside the workspace the turn runs in, and every
// workspace belongs to one agent, so one agent can never read another's uploads.
//
// Admission stages, the worker places. An upload arrives before anyone knows
// which workspace will read it — a conversation gets its own directory or git
// worktree, and only the worker running the turn resolves and creates it. So the
// gateway writes the bytes to a staging directory beside the workspaces and
// records the paths on the turn row; the worker links them into the workspace it
// actually mounts. The container path in the prompt is therefore a promise the
// worker keeps, not a guess about where the file already is.
//
// Writing straight into a conversation directory from here would be worse than
// simply wrong: resolveWorkspace treats an existing directory as one an earlier
// turn already prepared, so creating it early would silently skip the worktree
// and the CLAUDE.md seed for the first message in a thread.

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

// Staged is one request's uploads, written to disk and waiting for a turn.
type Staged struct {
	// HostPaths is where the bytes are now. Recorded on the turn row so the
	// worker can find them however many times the activity is retried.
	HostPaths []string
	// ContainerPaths is what the agent is told, and what the worker must make
	// true before the container starts.
	ContainerPaths []string
}

// StageAttachments writes uploads to the agent's staging directory and returns
// both where they are and where the container will see them.
func (d *Dispatcher) StageAttachments(agentID string, files []Attachment) (Staged, error) {
	if len(files) == 0 {
		return Staged{}, nil
	}
	if len(files) > MaxAttachments {
		return Staged{}, fmt.Errorf("too many files: %d, limit is %d", len(files), MaxAttachments)
	}

	stagingDir := d.cfg.InboxStagingDir(agentID)
	if err := os.MkdirAll(stagingDir, 0o750); err != nil {
		return Staged{}, fmt.Errorf("create inbox staging: %w", err)
	}

	// One prefix per request, so files that arrive together stay together and
	// two uploads of "report.pdf" cannot overwrite each other. It also makes the
	// staged name and the workspace name identical, so placement is a link with
	// no bookkeeping of its own.
	batch, err := randomToken()
	if err != nil {
		return Staged{}, err
	}

	var staged Staged
	for _, f := range files {
		name := batch + "-" + sanitizeFilename(f.Name)

		if err := writeCapped(filepath.Join(stagingDir, name), f.Body); err != nil {
			return Staged{}, fmt.Errorf("save %s: %w", f.Name, err)
		}
		staged.HostPaths = append(staged.HostPaths, filepath.Join(stagingDir, name))
		staged.ContainerPaths = append(staged.ContainerPaths, filepath.Join(claude.ContainerWorkspace, inboxDir, name))
	}
	return staged, nil
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
