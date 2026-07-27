package activity

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Placing a turn's uploads.
//
// The gateway accepted the bytes before anything knew which workspace would read
// them: a conversation runs in its own directory or git worktree, and resolving —
// and creating — that is the worker's job. So admission stages the files beside
// the workspaces and records the paths on the turn row, and this puts them where
// the prompt already told the agent to look.
//
// The link is what makes it cheap. Staging and the workspace both live under the
// agent's directory, so a hard link costs a directory entry and no bytes, and a
// 25MB upload is never copied.

// placeAttachments links a turn's staged uploads into inbox/ under workspace.
//
// The staged entries are deliberately left behind. A hard link shares the inode,
// so keeping them costs nothing, and it is what makes a retried activity safe:
// the second attempt finds the files still there and the links already made.
func placeAttachments(workspace string, staged []string) error {
	if len(staged) == 0 {
		return nil
	}

	inbox := filepath.Join(workspace, "inbox")
	if err := os.MkdirAll(inbox, 0o750); err != nil {
		return fmt.Errorf("create inbox in %s: %w", workspace, err)
	}

	for _, src := range staged {
		name := filepath.Base(src)
		// The staged name is the name the prompt promised, so the link needs no
		// mapping — see StageAttachments.
		dst := filepath.Join(inbox, name)

		err := os.Link(src, dst)
		switch {
		case err == nil:
		case errors.Is(err, os.ErrExist):
			// An earlier attempt of this activity already placed it.
		case errors.Is(err, os.ErrNotExist):
			// No retry will bring the upload back, and answering as though a file
			// had been delivered is the failure this whole path exists to avoid.
			return newNonRetryable(fmt.Errorf(
				"upload %s is no longer staged, so it cannot be given to the agent", name))
		default:
			// Cross-device is the case Link cannot serve. Both paths are under the
			// agent's directory so it should not arise, but a copy is correct
			// wherever it does.
			if err := copyFile(src, dst); err != nil {
				return fmt.Errorf("place upload %s: %w", name, err)
			}
		}
	}
	return nil
}

// copyFile is the fallback for a staging directory on another filesystem. It
// writes to a temporary name and renames, so a failure part-way cannot leave a
// truncated file that reads as a complete upload.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".partial-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed

	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
