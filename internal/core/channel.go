package core

import (
	"fmt"
	"strings"
)

// A stored channel is a reference, not a bare vendor id.
//
// Three things store a channel and later turn it back into a reply address: an
// agent's bindings, a schedule, and a workflow. A bare id was enough while one
// chat tool existed; with two it no longer says where a reply goes. So the
// stored value names the tool as well — "slack:C0123ABCD" — and a value with no
// prefix means Discord, which is what every row written before this said.
//
// The alternative was a platform column on all three tables and a matching
// field on three API shapes: the same fact six times. This also leaves version
// snapshots byte-identical, and those are a record of what an agent was — a
// migration that rewrote them would make rollback restore a shape that never
// existed. See adr/002-channel-refs.

// Platform is a chat tool a channel can live in.
type Platform string

const (
	PlatformDiscord Platform = "discord"
	PlatformSlack   Platform = "slack"
)

// channelRefSep separates the tool from the id. Neither vendor's ids contain a
// colon, which is what makes the format unambiguous rather than a convention.
const channelRefSep = ":"

// ParseChannelRef splits a stored channel reference into the tool it names and
// the id within it.
//
// An unprefixed value is Discord. That default is permanent: it is what every
// row written before Slack existed means, and rewriting those rows is exactly
// what this format avoids.
func ParseChannelRef(ref string) (Platform, string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", fmt.Errorf("channel reference is empty")
	}

	tool, id, found := strings.Cut(ref, channelRefSep)
	if !found {
		return PlatformDiscord, ref, nil
	}

	switch p := Platform(strings.ToLower(strings.TrimSpace(tool))); p {
	case PlatformDiscord, PlatformSlack:
		// The id keeps its case: Slack's are uppercase and matching is exact.
		if id = strings.TrimSpace(id); id == "" {
			return "", "", fmt.Errorf("channel reference %q names %s but no channel", ref, p)
		}
		return p, id, nil
	default:
		return "", "", fmt.Errorf("channel reference %q names an unknown chat tool %q", ref, tool)
	}
}

// FormatChannelRef renders the canonical stored form for a tool and id.
//
// Discord renders bare rather than as "discord:…". One tool has to be the
// unprefixed default because the rows that predate this format have no prefix,
// and having two spellings of the same channel is what CanonicalChannelRef
// exists to prevent.
func FormatChannelRef(p Platform, id string) string {
	if p == PlatformDiscord {
		return id
	}
	return string(p) + channelRefSep + id
}

// CanonicalChannelRef normalises a reference to the single spelling that gets
// stored, rejecting one that cannot be acted on.
//
// This is load-bearing rather than tidiness. A channel binding is keyed on the
// reference, so if "123" and "discord:123" could both be stored they would be
// two rows and two agents could bind one Discord channel — which the Channels
// spec says cannot happen. Every write normalises first; no caller may skip it
// on the grounds that its input looks already clean.
func CanonicalChannelRef(ref string) (string, error) {
	p, id, err := ParseChannelRef(ref)
	if err != nil {
		return "", err
	}
	return FormatChannelRef(p, id), nil
}

// OriginForChannel is the reply address a stored channel reference names.
//
// Both places that rebuild an address from a stored channel — a schedule firing
// and a workflow step — go through this, so neither can assume a tool the way
// they did when there was only one.
func OriginForChannel(ref string) (Origin, error) {
	p, id, err := ParseChannelRef(ref)
	if err != nil {
		return Origin{}, err
	}
	switch p {
	case PlatformSlack:
		// No thread: a schedule and a workflow post at the top of the channel,
		// and the reply is the start of whatever thread follows.
		return SlackOrigin(id, ""), nil
	default:
		return DiscordOrigin(id, ""), nil
	}
}

// A "which platform is this?" accessor and a "what is its message limit?"
// lookup both belong here and are deliberately absent: every caller that has a
// reference wants an address, which is OriginForChannel, and every caller that
// splits a message already knows which tool it is talking to. Adding them back
// needs a caller, not a hunch.
