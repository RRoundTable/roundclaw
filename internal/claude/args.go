package claude

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// sortedKeys returns a map's keys in a stable order, so argv built from a map
// is deterministic.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Container-side paths. roundclaw ships no code into the image, so these are
// the only two things it needs the image to agree about.
const (
	// ContainerWorkspace is the agent's working directory. `claude --resume`
	// scopes its session lookup to the current directory, so every turn for an
	// agent must run with this exact cwd or resume silently fails to find the
	// session.
	ContainerWorkspace = "/workspace"

	// ContainerClaudeHome holds session transcripts. It must be a persistent
	// host mount: the container is --rm, and if this went with it, --resume
	// would have nothing to resume.
	ContainerClaudeHome = "/home/node/.claude"
)

// MaxPromptBytes bounds a prompt passed as an argv entry. Well under any
// realistic ARG_MAX, and adapters reject longer input before it reaches here.
const MaxPromptBytes = 32 << 10

// RunSpec fully describes one containerised `claude` invocation.
type RunSpec struct {
	// Runtime is the container CLI ("docker", "podman").
	Runtime string
	Image   string
	// ContainerName must be deterministic for a given turn so that a retry can
	// force-remove a container orphaned by a crashed worker.
	ContainerName string

	// Host paths.
	WorkDir    string
	ClaudeHome string
	// AdditionalDirs are mounted read-only and exposed via --add-dir.
	AdditionalDirs []string

	// DenyPaths are shadowed with /dev/null inside the workspace, so a file
	// that has to stay on disk for other tooling is still unreadable to the
	// agent. Paths are relative to the workspace root.
	//
	// This matters most when WorkDir points at a real project: mounting a
	// repository read-write hands the agent everything in it, including the
	// .env files that repositories routinely keep out of git but not off disk.
	DenyPaths []string

	// CredentialEnv is the environment variable name to set inside the
	// container (ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN), and
	// CredentialValue is its value. Both are needed because `claude` accepts
	// either credential and picks by variable name.
	CredentialEnv   string
	CredentialValue string

	// Secrets are extra environment variables the agent's container should
	// receive — a GITHUB_TOKEN, a tool's API key. Like the credential they pass
	// by name (-e NAME): Args emits only the names, and the caller sets the
	// values on the runtime subprocess's environment, so no value reaches argv,
	// the process table, or a Temporal history event.
	Secrets map[string]string

	// SessionID is derived from the workflow ID. Resume selects which flag
	// carries it: --session-id creates a session, --resume continues one, and
	// passing --session-id for an existing session is an error.
	SessionID string
	Resume    bool

	AgentName      string
	PermissionMode string
	AllowedTools   []string

	Prompt string
}

// Validate catches a malformed spec before a container is started.
func (s RunSpec) Validate() error {
	switch {
	case s.Runtime == "":
		return fmt.Errorf("runspec: runtime is required")
	case s.Image == "":
		return fmt.Errorf("runspec: image is required")
	case s.ContainerName == "":
		return fmt.Errorf("runspec: container name is required")
	case s.SessionID == "":
		return fmt.Errorf("runspec: session id is required")
	case s.CredentialEnv == "":
		return fmt.Errorf("runspec: credential env name is required")
	case s.CredentialValue == "":
		return fmt.Errorf("runspec: credential value is required")
	case !filepath.IsAbs(s.WorkDir):
		return fmt.Errorf("runspec: work dir %q must be absolute", s.WorkDir)
	case !filepath.IsAbs(s.ClaudeHome):
		return fmt.Errorf("runspec: claude home %q must be absolute", s.ClaudeHome)
	case strings.TrimSpace(s.Prompt) == "":
		return fmt.Errorf("runspec: prompt is empty")
	case len(s.Prompt) > MaxPromptBytes:
		return fmt.Errorf("runspec: prompt is %d bytes, limit is %d", len(s.Prompt), MaxPromptBytes)
	}
	for _, d := range s.AdditionalDirs {
		if !filepath.IsAbs(d) {
			return fmt.Errorf("runspec: additional dir %q must be absolute", d)
		}
	}
	for _, d := range s.DenyPaths {
		if err := ValidateDenyPath(d); err != nil {
			return fmt.Errorf("runspec: %w", err)
		}
	}
	return nil
}

// ValidateDenyPath rejects a shadow target that would not land inside the
// workspace. A deny path that escaped would mount /dev/null over something on
// the host — turning a protection into a way to destroy an arbitrary file.
func ValidateDenyPath(p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("deny path is empty")
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("deny path %q must be relative to the workspace", p)
	}
	clean := filepath.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("deny path %q escapes the workspace", p)
	}
	return nil
}

// Args builds the full argv for the container runtime.
//
// The credential is passed by name (-e NAME) rather than by value, so the
// secret is inherited from the runtime's environment and never appears in the
// process table or in a Temporal history event.
func (s RunSpec) Args() ([]string, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}

	args := []string{
		"run", "--rm",
		"--name", s.ContainerName,
		"--workdir", ContainerWorkspace,
		"-v", s.WorkDir + ":" + ContainerWorkspace,
		"-v", s.ClaudeHome + ":" + ContainerClaudeHome,
		"-e", s.CredentialEnv,
	}

	// Shadowing has to come after the workspace mount so it lands on top of it.
	// Docker orders nested mounts by path depth, so listing them here is
	// enough; /dev/null is read-only and reads as empty.
	for _, d := range s.DenyPaths {
		target := filepath.Join(ContainerWorkspace, filepath.Clean(d))
		args = append(args, "-v", "/dev/null:"+target+":ro")
	}

	var addDirs []string
	for _, d := range s.AdditionalDirs {
		mount := filepath.Join("/mnt", filepath.Base(d))
		args = append(args, "-v", d+":"+mount+":ro")
		addDirs = append(addDirs, mount)
	}

	// Secret env vars, by name only. Sorted so the argv is deterministic — the
	// fake-CLI test asserts argument order, and a stable order also makes a
	// container's command reproducible. Placed after the credential's -e so a
	// secret can never shadow it (the activity also filters that collision).
	for _, name := range sortedKeys(s.Secrets) {
		args = append(args, "-e", name)
	}

	// The prompt goes immediately after -p, before any flag.
	//
	// --allowedTools is variadic: it keeps consuming arguments, so a prompt
	// placed after it is swallowed and `claude` exits with "Input must be
	// provided", having never seen the request. exec.Command does not involve a
	// shell, so the prompt needs no quoting or escaping here.
	args = append(args, s.Image, "claude", "-p", s.Prompt)

	if s.Resume {
		args = append(args, "--resume", s.SessionID)
	} else {
		args = append(args, "--session-id", s.SessionID)
	}

	// --verbose is required for stream-json to emit anything beyond the final
	// result. --include-partial-messages is deliberately omitted: it emits a
	// token-level delta per event, which would turn one turn into thousands of
	// SQLite inserts. Message-level granularity is enough to show what the
	// agent is doing.
	args = append(args, "--output-format", "stream-json", "--verbose")

	if s.AgentName != "" {
		args = append(args, "--agent", s.AgentName)
	}
	if s.PermissionMode != "" {
		args = append(args, "--permission-mode", s.PermissionMode)
	}
	if len(s.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(s.AllowedTools, ","))
	}
	for _, d := range addDirs {
		args = append(args, "--add-dir", d)
	}
	return args, nil
}

// RemoveArgs builds the argv that force-removes a container left behind by a
// crashed worker. Running this before a retry is what makes the deterministic
// container name safe to reuse.
func (s RunSpec) RemoveArgs() []string {
	return []string{"rm", "-f", s.ContainerName}
}

// StopArgs builds the argv that asks the container to stop gracefully, giving
// `claude` a chance to flush its session transcript before it dies.
func (s RunSpec) StopArgs(graceSeconds int) []string {
	return []string{"stop", "--time", fmt.Sprint(graceSeconds), s.ContainerName}
}

// ContainerName builds the deterministic per-turn container name.
func ContainerName(agentID string, turnID int64) string {
	return fmt.Sprintf("roundclaw-%s-%d", agentID, turnID)
}
