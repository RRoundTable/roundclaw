package registry

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// toolDir writes a directory with one file in it and returns the path, standing
// in for whatever a tool actually is on the host.
func toolDir(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write tool file: %v", err)
	}
	return dir
}

func TestPutToolRecordsVersionOne(t *testing.T) {
	s := newStore(t)
	s.UseIdentitySource(IdentityByReading())
	dir := toolDir(t, "echo one")

	if _, err := s.PutTool(t.Context(), Tool{
		ID: "deploy", Description: "ships it", HostPath: dir,
		Identity: []IdentityMember{{Path: dir}},
	}); err != nil {
		t.Fatalf("put tool: %v", err)
	}

	v, err := s.LatestToolVersion(t.Context(), "deploy")
	if err != nil {
		t.Fatalf("latest tool version: %v", err)
	}
	if v.Version != 1 {
		t.Errorf("version = %d, want 1", v.Version)
	}
	if v.Digest == "" {
		t.Error("digest is empty; a tool that declared a member should have a witness")
	}
	if v.DigestErr != "" {
		t.Errorf("digest error = %q, want none", v.DigestErr)
	}
}

// The case adr/005 exists for: the row never moves, the thing it points at does.
// A version recording only the pointer would miss this entirely.
func TestToolContentChangingMintsAVersionWithTheRowUntouched(t *testing.T) {
	s := newStore(t)
	s.UseIdentitySource(IdentityByReading())
	dir := toolDir(t, "echo one")
	tool := Tool{ID: "deploy", HostPath: dir, Identity: []IdentityMember{{Path: dir}}}

	if _, err := s.PutTool(t.Context(), tool); err != nil {
		t.Fatalf("put tool: %v", err)
	}
	first, err := s.LatestToolVersion(t.Context(), "deploy")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}

	// Nothing about the definition changes — only the file on disk.
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("echo two"), 0o600); err != nil {
		t.Fatalf("rewrite tool file: %v", err)
	}
	if _, err := s.SnapshotTool(t.Context(), "deploy", Change{Note: "content moved"}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	second, err := s.LatestToolVersion(t.Context(), "deploy")
	if err != nil {
		t.Fatalf("latest after edit: %v", err)
	}
	if second.Version != 2 {
		t.Fatalf("version = %d, want 2: a changed member is a changed tool", second.Version)
	}
	if second.Digest == first.Digest {
		t.Error("digest unchanged after the declared file was rewritten")
	}
	if second.Definition.HostPath != first.Definition.HostPath {
		t.Error("definition moved; only the content was supposed to")
	}
}

func TestIdenticalToolWriteMintsNoVersion(t *testing.T) {
	s := newStore(t)
	s.UseIdentitySource(IdentityByReading())
	dir := toolDir(t, "echo one")
	tool := Tool{ID: "deploy", Description: "ships it", HostPath: dir,
		Identity: []IdentityMember{{Path: dir}}}

	if _, err := s.PutTool(t.Context(), tool); err != nil {
		t.Fatalf("put tool: %v", err)
	}
	// A client PUTting back what it just read must not mint a row that records
	// nothing; a history mostly made of noise is one nobody reads.
	if _, err := s.PutTool(t.Context(), tool); err != nil {
		t.Fatalf("put tool again: %v", err)
	}

	versions, err := s.ListToolVersions(t.Context(), "deploy", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(versions) != 1 {
		t.Errorf("versions = %d, want 1", len(versions))
	}
}

// A tool declaring nothing has no witness, and that is recorded rather than
// papered over with a guess at host_path.
func TestToolWithNoDeclaredIdentityHasNoDigest(t *testing.T) {
	s := newStore(t)
	s.UseIdentitySource(IdentityByReading())
	dir := toolDir(t, "echo one")

	if _, err := s.PutTool(t.Context(), Tool{ID: "deploy", HostPath: dir}); err != nil {
		t.Fatalf("put tool: %v", err)
	}
	v, err := s.LatestToolVersion(t.Context(), "deploy")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if v.Digest != "" {
		t.Errorf("digest = %q; an undeclared identity must not be inferred from host_path", v.Digest)
	}
	if v.DigestErr != "" {
		t.Errorf("digest error = %q; declaring nothing is not a failure to read", v.DigestErr)
	}
}

// An unreadable member is a version that says so, not a failed write: a tool
// whose directory was deleted still has to be recordable.
func TestUnreadableIdentityRecordsTheReasonAndStillWrites(t *testing.T) {
	s := newStore(t)
	s.UseIdentitySource(IdentityByReading())

	if _, err := s.PutTool(t.Context(), Tool{
		ID: "deploy", HostPath: "/nonexistent",
		Identity: []IdentityMember{{Path: "/nonexistent/gone"}},
	}); err != nil {
		t.Fatalf("put tool: %v", err)
	}
	v, err := s.LatestToolVersion(t.Context(), "deploy")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if v.Digest != "" {
		t.Errorf("digest = %q, want empty", v.Digest)
	}
	if v.DigestErr == "" {
		t.Error("digest error is empty; an unreadable member must say why")
	}
}

func TestRollbackReportsWhetherTheContentCameBack(t *testing.T) {
	s := newStore(t)
	s.UseIdentitySource(IdentityByReading())
	dir := toolDir(t, "echo one")

	if _, err := s.PutTool(t.Context(), Tool{ID: "deploy", Description: "first",
		HostPath: dir, Identity: []IdentityMember{{Path: dir}}}); err != nil {
		t.Fatalf("put tool: %v", err)
	}
	if _, err := s.PutTool(t.Context(), Tool{ID: "deploy", Description: "second",
		HostPath: dir, Identity: []IdentityMember{{Path: dir}}}); err != nil {
		t.Fatalf("put tool again: %v", err)
	}

	// Drift the content out from under v1 before rolling back to it.
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("echo drifted"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	res, err := s.RollbackTool(t.Context(), "deploy", 1, Change{Author: "someone"})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if res.Tool.Description != "first" {
		t.Errorf("description = %q, want the restored definition", res.Tool.Description)
	}
	if res.DigestMatches {
		t.Error("DigestMatches is true, but the declared content changed since v1")
	}
	if res.From != 1 {
		t.Errorf("From = %d, want 1", res.From)
	}
	// Applied as a new version, never as a rewind: what was undone stays visible.
	if res.Version <= 2 {
		t.Errorf("rollback minted v%d, want a version after the one it undid", res.Version)
	}
}

func TestRollbackToUnchangedContentMatches(t *testing.T) {
	s := newStore(t)
	s.UseIdentitySource(IdentityByReading())
	dir := toolDir(t, "echo one")

	if _, err := s.PutTool(t.Context(), Tool{ID: "deploy", Description: "first",
		HostPath: dir, Identity: []IdentityMember{{Path: dir}}}); err != nil {
		t.Fatalf("put tool: %v", err)
	}
	if _, err := s.PutTool(t.Context(), Tool{ID: "deploy", Description: "second",
		HostPath: dir, Identity: []IdentityMember{{Path: dir}}}); err != nil {
		t.Fatalf("put tool again: %v", err)
	}

	res, err := s.RollbackTool(t.Context(), "deploy", 1, Change{})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !res.DigestMatches {
		t.Error("DigestMatches is false, but nothing on disk moved")
	}
}

// History outlives the tool, on the same terms agent versions outlive an agent:
// deleting is exactly when somebody needs to know what it was.
func TestDeletingAToolKeepsItsVersions(t *testing.T) {
	s := newStore(t)
	s.UseIdentitySource(IdentityByReading())
	dir := toolDir(t, "echo one")

	if _, err := s.PutTool(t.Context(), Tool{ID: "deploy", HostPath: dir,
		Identity: []IdentityMember{{Path: dir}}}); err != nil {
		t.Fatalf("put tool: %v", err)
	}
	if err := s.DeleteTool(t.Context(), "deploy"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	versions, err := s.ListToolVersions(t.Context(), "deploy", 0)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(versions) != 1 {
		t.Errorf("versions = %d, want the history to survive the delete", len(versions))
	}
	if _, err := s.GetTool(t.Context(), "deploy"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get tool after delete = %v, want ErrNotFound", err)
	}
}

func TestSkillVersionsFollowTheSameShape(t *testing.T) {
	s := newStore(t)
	s.UseIdentitySource(IdentityByReading())
	dir := toolDir(t, "# SKILL.md")

	if _, err := s.PutSkill(t.Context(), Skill{ID: "review", HostPath: dir,
		Identity: []IdentityMember{{Path: dir}}}); err != nil {
		t.Fatalf("put skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("# SKILL.md, edited"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if _, err := s.SnapshotSkill(t.Context(), "review", Change{}); err != nil {
		t.Fatalf("snapshot skill: %v", err)
	}

	v, err := s.LatestSkillVersion(t.Context(), "review")
	if err != nil {
		t.Fatalf("latest skill version: %v", err)
	}
	if v.Version != 2 {
		t.Errorf("version = %d, want 2", v.Version)
	}
}

// An agent version that did not name the tool versions in play would describe a
// configuration that never existed — and would let two evaluations "of the same
// configuration" measure different things.
func TestAgentVersionRecordsTheGrantsItHeld(t *testing.T) {
	s := newStore(t)
	s.UseIdentitySource(IdentityByReading())
	dir := toolDir(t, "echo one")

	if _, err := s.PutTool(t.Context(), Tool{ID: "deploy", HostPath: dir,
		Identity: []IdentityMember{{Path: dir}}}); err != nil {
		t.Fatalf("put tool: %v", err)
	}
	if _, err := s.PutSkill(t.Context(), Skill{ID: "review", HostPath: dir}); err != nil {
		t.Fatalf("put skill: %v", err)
	}
	if _, err := s.Create(t.Context(), Agent{ID: "dev", Enabled: true,
		Tools: []string{"deploy"}, Skills: []string{"review"}}); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	v, err := s.LatestVersion(t.Context(), "dev")
	if err != nil {
		t.Fatalf("latest version: %v", err)
	}
	if got := v.Grants.Tools["deploy"]; got != 1 {
		t.Errorf("grants.tools[deploy] = %d, want 1", got)
	}
	if got := v.Grants.Skills["review"]; got != 1 {
		t.Errorf("grants.skills[review] = %d, want 1", got)
	}
}

// A grant pointing at nothing is left out rather than pinned to zero: absent
// says "there was nothing to pin", where a zero would read as a real version.
func TestGrantWithNoVersionIsLeftOutRatherThanZeroed(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create(t.Context(), Agent{ID: "dev", Enabled: true,
		Tools: []string{"never-registered"}}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	v, err := s.LatestVersion(t.Context(), "dev")
	if err != nil {
		t.Fatalf("latest version: %v", err)
	}
	if _, ok := v.Grants.Tools["never-registered"]; ok {
		t.Error("a grant with no version was recorded; it should be absent")
	}
}

// An endpoint member is how a running service says what it is: nothing on the
// host answers that, since a mount beside a service is its client's config.
func TestEndpointIdentityWitnessesAService(t *testing.T) {
	body := "postgres 15.1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, body)
	}))
	defer srv.Close()

	s := newStore(t)
	s.UseIdentitySource(IdentityByReading())
	if _, err := s.PutTool(t.Context(), Tool{ID: "db", HostPath: t.TempDir(),
		Identity: []IdentityMember{{Endpoint: srv.URL}}}); err != nil {
		t.Fatalf("put tool: %v", err)
	}
	first, err := s.LatestToolVersion(t.Context(), "db")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if first.Digest == "" {
		t.Fatal("an endpoint member produced no witness")
	}

	body = "postgres 16.0" // the service was replaced under a row that never moved
	if _, err := s.SnapshotTool(t.Context(), "db", Change{}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	second, err := s.LatestToolVersion(t.Context(), "db")
	if err != nil {
		t.Fatalf("latest after replacement: %v", err)
	}
	if second.Digest == first.Digest {
		t.Error("the witness did not move when the service did")
	}
}

func TestDriftIsReportedAgainstTheRecordedVersion(t *testing.T) {
	s := newStore(t)
	s.UseIdentitySource(IdentityByReading())
	dir := toolDir(t, "echo one")
	if _, err := s.PutTool(t.Context(), Tool{ID: "deploy", HostPath: dir,
		Identity: []IdentityMember{{Path: dir}}}); err != nil {
		t.Fatalf("put tool: %v", err)
	}

	d, err := s.ToolDriftOf(t.Context(), "deploy")
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	if !d.Declared || !d.Matches {
		t.Fatalf("a freshly written tool reports drift: %+v", d)
	}

	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("echo two"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	d, err = s.ToolDriftOf(t.Context(), "deploy")
	if err != nil {
		t.Fatalf("drift after edit: %v", err)
	}
	if d.Matches {
		t.Error("an edited tool does not report drift")
	}
}

// Nothing declared is not a drift. Silence about identity says "there is nothing
// to compare", which is different from "it changed".
func TestUndeclaredIdentityIsNotDrift(t *testing.T) {
	s := newStore(t)
	s.UseIdentitySource(IdentityByReading())
	if _, err := s.PutTool(t.Context(), Tool{ID: "deploy", HostPath: t.TempDir()}); err != nil {
		t.Fatalf("put tool: %v", err)
	}
	d, err := s.ToolDriftOf(t.Context(), "deploy")
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	if d.Declared {
		t.Error("a tool that declared nothing reported a comparable identity")
	}
}

func TestReachabilityValidationRejectsNonsense(t *testing.T) {
	for _, r := range []Reachability{
		{Endpoint: "ftp://x"},
		{File: "relative/path"},
		{Address: "no-port"},
		{WithinSeconds: -1},
	} {
		if err := (Tool{ID: "t", HostPath: "/tmp", Reachability: r}).Validate(); err == nil {
			t.Errorf("accepted an invalid reachability: %+v", r)
		}
	}
}

// A member naming both, or neither, has no defined reading.
func TestIdentityMemberNeedsExactlyOneSource(t *testing.T) {
	if (IdentityMember{}).Validate() == nil {
		t.Error("an empty member validated")
	}
	if (IdentityMember{Path: "/a", Endpoint: "http://b"}).Validate() == nil {
		t.Error("a member naming both a path and an endpoint validated")
	}
}
