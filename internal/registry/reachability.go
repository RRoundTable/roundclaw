package registry

import (
	"context"
	"fmt"
	"strings"
)

// What "this tool works" means, said rather than done.
//
// A tool is the agent's surrounding dependencies — a package, an API server, a
// database — and some of them hold state that does not survive an interruption.
// Something has to carry how to get them back.
//
// It is a declaration, never a command. roundclaw has never executed a string out
// of the registry: every process it starts is git or the operator-configured
// container runtime, with structured arguments. A command field would be the
// first, it would be written by the agent itself now that an agent can write its
// own tools, and it would run at session start — ahead of the measurement gate
// that judges everything else an agent changes. So the tool says what must be
// true and roundclaw decides how, out of the one structured action it already
// performs (adr/004).
//
// What that cannot express is not smuggled back in. A tool needing a migration
// run or a cache warmed cannot declare itself restorable; it is reported
// unavailable, and the agent fixes it inside a turn if it wants to — later than
// session start, but gated by everything that gates a turn.

// Reachability is the condition that has to hold for a tool to be usable. Every
// field set must hold; all empty means the tool declares nothing, and a tool that
// declares nothing is treated as unrestorable — the dangerous default is the
// optimistic one.
type Reachability struct {
	// Container that must be running. This is the only thing roundclaw will act
	// on: it starts the container through the runtime it already drives, then
	// waits for the rest of the condition.
	Container string `json:"container,omitempty"`
	// Address that must accept a TCP connection, as host:port.
	Address string `json:"address,omitempty"`
	// Endpoint that must answer 2xx.
	Endpoint string `json:"endpoint,omitempty"`
	// File that must exist on the host.
	File string `json:"file,omitempty"`
	// WithinSeconds bounds the whole check. Zero takes the default. A tool that
	// is slow to come back must not be able to delay work indefinitely, so
	// exceeding this reports unavailable rather than waiting.
	WithinSeconds int `json:"within_seconds,omitempty"`
}

// Declared reports whether the tool said anything about being reachable.
func (r Reachability) Declared() bool {
	return r.Container != "" || r.Address != "" || r.Endpoint != "" || r.File != ""
}

// Restorable reports whether roundclaw can do anything about the tool being
// down. Only a container is actionable; everything else it can check but not fix.
func (r Reachability) Restorable() bool { return r.Container != "" }

// Validate checks a declaration before it is written.
func (r Reachability) Validate() error {
	if r.Endpoint != "" && !strings.HasPrefix(r.Endpoint, "http://") && !strings.HasPrefix(r.Endpoint, "https://") {
		return fmt.Errorf("reachability: endpoint must be http or https, got %q", r.Endpoint)
	}
	if r.File != "" && r.File[0] != '/' {
		return fmt.Errorf("reachability: file must be an absolute path, got %q", r.File)
	}
	if r.Address != "" && !strings.Contains(r.Address, ":") {
		return fmt.Errorf("reachability: address must be host:port, got %q", r.Address)
	}
	if r.WithinSeconds < 0 {
		return fmt.Errorf("reachability: within_seconds cannot be negative")
	}
	return nil
}

// ToolDrift says whether a tool is still what its newest version recorded.
type ToolDrift struct {
	// Declared is false when the tool named no identity. Then there is nothing
	// to compare and nothing is claimed — not a match, and not a drift.
	Declared bool
	Matches  bool
	Recorded string
	Current  string
	// Reason is why the current identity could not be read, when it could not.
	Reason string
}

// ToolDriftOf compares a tool's identity as it is now against the one its newest
// version recorded.
//
// A configuration that cannot be trusted to be what it claims cannot be measured
// against another one, which is why this is checked at session start rather than
// discovered when two evaluation runs disagree for no visible reason.
func (s *Store) ToolDriftOf(ctx context.Context, id string) (ToolDrift, error) {
	t, err := s.GetTool(ctx, id)
	if err != nil {
		return ToolDrift{}, err
	}
	if len(t.Identity) == 0 {
		return ToolDrift{}, nil
	}
	v, err := s.LatestToolVersion(ctx, id)
	if err != nil {
		return ToolDrift{}, err
	}
	current, reason := s.digestOf(t.Identity)
	return ToolDrift{
		Declared: true,
		Matches:  reason == "" && v.Digest != "" && v.Digest == current,
		Recorded: v.Digest,
		Current:  current,
		Reason:   reason,
	}, nil
}
