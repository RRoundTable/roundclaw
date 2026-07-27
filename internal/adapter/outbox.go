package adapter

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// File output.
//
// An agent writes a file into outbox/ in its workspace and names it when it
// speaks; the gateway opens it and hands the reader to Discord. Nothing is
// buffered, so a 10MB attachment never sits in memory.
//
// This is the outbound mirror of the inbox rule. A large result belongs in a
// file rather than in the message text: a 40k-token report sent as text is
// twenty Discord messages, and it stays in the session context and is re-sent on
// every later turn of that conversation. Sent as a file it costs the agent one
// path.
//
// What outbox/ is and is not:
//
// It is not a boundary against a compromised agent. An agent with Bash can run
// `cp /etc/passwd outbox/` and this will send it. What it stops is an accident —
// a workspace .env or a source file from the worktree going out because a path
// was slightly wrong — and a prompt-injected agent that reached only the message
// API naming an arbitrary path.
//
// The boundary that does matter is the channel: an agent may only be heard where
// it is already spoken to (see conversationChannel). Files make the consequence
// of getting that wrong much larger, which is why it is not relaxed here.

const outboxDir = "outbox"

const (
	// MaxOutboundBytes caps one outgoing file. Deliberately below the 25MB inbound
	// cap: that number is Discord's limit for uploads to a boosted guild, while an
	// unboosted one rejects anything over 10MB. Failing here with a clear message
	// beats a rejection from Discord that the agent cannot read.
	MaxOutboundBytes = 10 << 20
	// MaxOutboundFiles per message is Discord's own per-message maximum.
	MaxOutboundFiles = 10
)

// OutFile is one file on its way to Discord. Body must be closed by the sender.
type OutFile struct {
	Name string
	Body io.ReadCloser
	Size int64
}

// openOutbound resolves the agent's named files against its outbox and opens
// them.
//
// On any error every file already opened is closed, so a rejected request cannot
// leak descriptors. The caller closes them on the success path.
func openOutbound(workspace string, names []string) ([]OutFile, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if len(names) > MaxOutboundFiles {
		return nil, fmt.Errorf("too many files: %d, discord accepts %d per message",
			len(names), MaxOutboundFiles)
	}

	var files []OutFile
	closeAll := func() {
		for _, f := range files {
			f.Body.Close()
		}
	}

	for _, name := range names {
		path, size, err := resolveOutFile(workspace, name)
		if err != nil {
			closeAll()
			return nil, err
		}
		f, err := os.Open(path)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("open %s: %w", name, err)
		}
		files = append(files, OutFile{Name: filepath.Base(path), Body: f, Size: size})
	}
	return files, nil
}

// closeOutbound releases files that will not be sent after all. The sender owns
// them once it is called, so this is only for the paths that give up first.
func closeOutbound(files []OutFile) {
	for _, f := range files {
		f.Body.Close()
	}
}

// resolveOutFile turns an agent-supplied name into a path inside the outbox, or
// refuses.
//
// The name comes from the agent, so it is treated as hostile in the same way an
// upload's name is. Symlinks are resolved before the containment check rather
// than after: the agent has Write, so `ln -s /etc/shadow outbox/notes.txt` is a
// path that passes every purely lexical test.
func resolveOutFile(workspace, name string) (string, int64, error) {
	if strings.TrimSpace(name) == "" {
		return "", 0, fmt.Errorf("empty file name")
	}
	if filepath.IsAbs(name) {
		return "", 0, fmt.Errorf("%s: name a file inside outbox/, not an absolute path", name)
	}

	outbox := filepath.Join(workspace, outboxDir)
	// A leading separator makes Clean absorb any ".." rather than let it climb
	// out, so the join below cannot leave the outbox lexically.
	rel := strings.TrimPrefix(filepath.Clean("/"+name), "/")
	// Naming the outbox itself, or a path that reduced to nothing.
	if rel == "" || rel == "." {
		return "", 0, fmt.Errorf("%s: name a file inside outbox/", name)
	}
	// Tolerate the agent writing the prefix out, since the prompt talks about
	// /workspace/outbox and repeating it is the obvious mistake.
	rel = strings.TrimPrefix(rel, outboxDir+"/")

	realOutbox, err := filepath.EvalSymlinks(outbox)
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, fmt.Errorf("this conversation has no outbox/ yet; write the file there first")
		}
		return "", 0, fmt.Errorf("resolve outbox: %w", err)
	}

	real, err := filepath.EvalSymlinks(filepath.Join(realOutbox, rel))
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, fmt.Errorf("%s is not in outbox/", name)
		}
		return "", 0, fmt.Errorf("resolve %s: %w", name, err)
	}
	// The decisive check, and it runs on the resolved path: a symlink inside the
	// outbox pointing anywhere else is refused here rather than followed.
	if real != realOutbox && !strings.HasPrefix(real, realOutbox+string(os.PathSeparator)) {
		return "", 0, fmt.Errorf("%s resolves outside outbox/", name)
	}

	// Lstat, not Stat: the target is already resolved, and this must see what it
	// actually is rather than what a final link claims.
	fi, err := os.Lstat(real)
	if err != nil {
		return "", 0, fmt.Errorf("read %s: %w", name, err)
	}
	if fi.IsDir() {
		return "", 0, fmt.Errorf("%s is a directory", name)
	}
	// A FIFO would block the upload forever, a device file would read garbage,
	// and neither is anything an agent meant to send.
	if !fi.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s is not a regular file", name)
	}
	if fi.Size() == 0 {
		return "", 0, fmt.Errorf("%s is empty", name)
	}
	if fi.Size() > MaxOutboundBytes {
		return "", 0, fmt.Errorf("%s is %.1fMB; the limit is %dMB",
			name, float64(fi.Size())/(1<<20), MaxOutboundBytes>>20)
	}
	return real, fi.Size(), nil
}
