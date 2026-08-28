package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// What a tool or skill is made of, and how that is recognised again later.
//
// A version records a definition, and a definition is a pointer: a host path, an
// env map, a note. The thing at the end of the pointer can change without the row
// moving at all, and a version that recorded only the pointer would describe
// nothing (adr/005). What closes that is a witness — something that changes when
// the tool changes.
//
// It is a witness, not a description. Nothing here has to learn that a tool is
// postgres 15; something has to learn that the tool is not what it was. A hash
// serves, and sha256 is already how this package compares things it cannot read
// back (see secrets.go).
//
// Crucially there is no default. roundclaw cannot tell from a row whether a
// mounted directory is the tool itself or a client's configuration for a server
// living elsewhere, so it does not guess: a tool declares the members it is made
// of, or it has no identity and its version says so. Guessing would produce a
// witness that moves when the config is edited and sits still when the server is
// replaced, which is worse than no witness, because it fails silently exactly
// where the stakes are highest.

// IdentityMember is one place a tool's identity is read from.
//
// Only a host path today. A member is a claim that what is there is stable: a
// source that changes on every read — a clock, a request id — would report a tool
// that has always just changed.
type IdentityMember struct {
	Path string `json:"path"`
}

// Validate checks a member before it is written.
func (m IdentityMember) Validate() error {
	if m.Path == "" || m.Path[0] != '/' {
		return fmt.Errorf("identity member: path must be absolute, got %q", m.Path)
	}
	return nil
}

// IdentitySource reduces a declared member set to a digest.
//
// It is injected for the reason PersonaSource is: the registry stores what a tool
// was, and must not learn how to walk the host to find out. Unset, every digest
// is empty — a registry with no filesystem in reach still keeps history rather
// than failing writes, which is what a test or a definition-only tool needs.
type IdentitySource func(members []IdentityMember) (string, error)

// UseIdentitySource installs the reader used by every subsequent snapshot.
func (s *Store) UseIdentitySource(fn IdentitySource) { s.identity = fn }

// digestOf reduces members to a digest, and reports why it could not when it
// could not.
//
// An unreadable member is not a failed write. A tool whose directory was deleted
// out from under it still gets a version, and that version says the identity was
// unreadable — which is the honest record, and the one that makes the deletion
// visible instead of freezing the history at the last good row.
func (s *Store) digestOf(members []IdentityMember) (string, string) {
	if s.identity == nil || len(members) == 0 {
		return "", ""
	}
	digest, err := s.identity(members)
	if err != nil {
		return "", err.Error()
	}
	return digest, ""
}

// IdentityByReading builds an IdentitySource that reads members off the host.
//
// A member naming a directory is walked; a member naming a file is read. The
// digest covers each entry's path relative to the member root, its permission
// bits and its contents, so a chmod is a change and so is a rename — both alter
// how the tool behaves while leaving byte counts alone.
//
// Members are hashed in declared order and entries within one in sorted order, so
// the digest is stable across filesystem iteration order. Symlinks are hashed as
// their target string rather than followed: a link that points somewhere else is
// a change, and following one would let a member escape the path that declared it.
func IdentityByReading() IdentitySource {
	return func(members []IdentityMember) (string, error) {
		sum := sha256.New()
		for _, m := range members {
			if err := m.Validate(); err != nil {
				return "", err
			}
			if err := hashMember(sum, m.Path); err != nil {
				return "", err
			}
		}
		return hex.EncodeToString(sum.Sum(nil)), nil
	}
}

func hashMember(sum io.Writer, root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("read identity member %s: %w", root, err)
	}
	fmt.Fprintf(sum, "member\x00%s\x00", root)
	if !info.IsDir() {
		return hashEntry(sum, root, filepath.Base(root), info)
	}

	// Collected and sorted rather than hashed as walked: WalkDir's order is
	// lexical per directory but the whole-tree order is not something to depend
	// on across platforms, and a digest that varies by traversal is not a witness.
	var entries []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entries = append(entries, rel)
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk identity member %s: %w", root, err)
	}
	sort.Strings(entries)

	for _, rel := range entries {
		full := filepath.Join(root, rel)
		info, err := os.Lstat(full)
		if err != nil {
			return fmt.Errorf("read %s: %w", full, err)
		}
		if err := hashEntry(sum, full, rel, info); err != nil {
			return err
		}
	}
	return nil
}

func hashEntry(sum io.Writer, full, name string, info os.FileInfo) error {
	fmt.Fprintf(sum, "%s\x00%o\x00", name, info.Mode().Perm())

	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(full)
		if err != nil {
			return fmt.Errorf("read link %s: %w", full, err)
		}
		fmt.Fprintf(sum, "link\x00%s\x00", target)
		return nil
	}

	f, err := os.Open(full)
	if err != nil {
		return fmt.Errorf("read %s: %w", full, err)
	}
	defer f.Close()
	if _, err := io.Copy(sum, f); err != nil {
		return fmt.Errorf("read %s: %w", full, err)
	}
	fmt.Fprint(sum, "\x00")
	return nil
}
