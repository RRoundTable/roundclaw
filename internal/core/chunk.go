package core

import "strings"

// Splitting a reply into chat messages.
//
// This lives in core because both processes send them: the gateway replies
// inline, and the worker delivers a finished turn. Two copies of a subtle
// rune-boundary rule is how the two drift.

// Per-message limits, in characters. Each is the tool's own; a reply is split
// to the limit of the tool it is being delivered to, so one that fits in a
// single Slack message is not cut into three because Discord would have needed
// three. The caller passes the one it is delivering to.
//
// Characters, not bytes — the distinction is the whole reason this is careful.
// Korean is three bytes per character in UTF-8, so a byte-counting splitter
// treats a 2000-character Korean message as three messages' worth and cuts it
// into thirds for no reason.
const (
	DiscordMaxMessage = 2000
	// SlackMaxMessage is the text limit of chat.postMessage. Slack truncates
	// past it rather than rejecting, which is worse than being told, so the
	// split has to happen here.
	SlackMaxMessage = 4000
)

// ChunkMessage splits s into messages the destination will accept, preferring
// to break on a newline so a code block or list is not cut mid-line where
// avoidable. max is in characters; zero or less means DiscordMaxMessage, the
// smaller of the two and so the safe default.
//
// Cuts land on rune boundaries. Slicing at a byte offset would split a
// multi-byte character in half whenever a chunk filled up with no newline in it
// — the reply then carries a replacement character at every seam, which for
// Korean or emoji is most seams.
func ChunkMessage(s string, max int) []string {
	if max <= 0 {
		max = DiscordMaxMessage
	}
	if countRunes(s) <= max {
		return []string{s}
	}

	var out []string
	for countRunes(s) > max {
		// A rune boundary by construction, so every cut below is safe.
		limit := runeOffset(s, max)

		cut := strings.LastIndexByte(s[:limit], '\n')
		if cut <= 0 {
			// One long line, or a leading newline that would emit nothing. Take
			// the full allowance rather than looping forever on a zero-width cut.
			cut = limit
		}
		out = append(out, s[:cut])

		// Exactly one newline — the one that was cut on. Any others were in the
		// text on purpose and belong to the next message.
		s = strings.TrimPrefix(s[cut:], "\n")
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

// runeOffset returns the byte offset of the n'th rune of s, or len(s) if s is
// shorter. Ranging a string yields the byte offset of each rune's first byte,
// which is exactly the boundary a cut needs.
func runeOffset(s string, n int) int {
	for i := range s {
		if n == 0 {
			return i
		}
		n--
	}
	return len(s)
}

func countRunes(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
