package core

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The bug this pins: the splitter sliced at a byte offset. When a chunk filled
// up with no newline to break on, the cut landed inside a multi-byte character
// and both sides carried half of it. Korean is three bytes per character, so an
// agent answering in Korean — which every agent here does — produced a
// replacement character at the seam.
func TestChunkForDiscordNeverSplitsACharacter(t *testing.T) {
	// One long line with no newline anywhere: the case that forces a hard cut.
	s := strings.Repeat("한글", 3000)

	for _, chunk := range ChunkForDiscord(s, DiscordMaxMessage) {
		if !utf8.ValidString(chunk) {
			t.Fatalf("a chunk is not valid UTF-8; a character was cut in half: %q",
				chunk[max(0, len(chunk)-20):])
		}
	}
	if got := strings.Join(ChunkForDiscord(s, DiscordMaxMessage), ""); got != s {
		t.Error("re-joining the chunks does not reproduce the input")
	}
}

// Discord counts characters. Counting bytes instead was safe but wasteful: a
// Korean reply was split into three times as many messages as Discord required.
func TestChunkForDiscordCountsCharactersNotBytes(t *testing.T) {
	// 1500 characters — under the limit, but 4500 bytes, well over it.
	s := strings.Repeat("한", 1500)
	if len(s) <= DiscordMaxMessage {
		t.Fatalf("test input is only %d bytes; it must exceed the limit to be meaningful", len(s))
	}

	got := ChunkForDiscord(s, DiscordMaxMessage)
	if len(got) != 1 {
		t.Errorf("%d characters were split into %d messages; Discord would have taken one",
			utf8.RuneCountInString(s), len(got))
	}
}

// Every chunk must actually fit, or Discord rejects the message outright.
func TestChunkForDiscordRespectsTheLimit(t *testing.T) {
	inputs := map[string]string{
		"korean no breaks":  strings.Repeat("가나다라마", 900),
		"ascii no breaks":   strings.Repeat("x", 9000),
		"mixed with breaks": strings.Repeat("line 한 줄\n", 800),
		"emoji":             strings.Repeat("🙂", 3000),
	}
	for name, s := range inputs {
		for i, chunk := range ChunkForDiscord(s, DiscordMaxMessage) {
			if n := utf8.RuneCountInString(chunk); n > DiscordMaxMessage {
				t.Errorf("%s: chunk %d is %d characters, over the %d limit",
					name, i, n, DiscordMaxMessage)
			}
		}
	}
}

// Breaking on a newline is what keeps a code fence or a list from being cut
// mid-line where there was a choice.
func TestChunkForDiscordPrefersLineBreaks(t *testing.T) {
	s := strings.Repeat("a line of text\n", 500)

	// Every line is the same, so a chunk that broke mid-line shows up as a line
	// that is not that one.
	for i, chunk := range ChunkForDiscord(s, 100) {
		for _, line := range strings.Split(strings.TrimSuffix(chunk, "\n"), "\n") {
			if line != "a line of text" {
				t.Errorf("chunk %d was cut mid-line: %q", i, line)
			}
		}
	}
}

func TestChunkForDiscordShortInputIsOneMessage(t *testing.T) {
	for _, s := range []string{"", "짧다", "hello"} {
		got := ChunkForDiscord(s, DiscordMaxMessage)
		if len(got) != 1 || got[0] != s {
			t.Errorf("ChunkForDiscord(%q) = %q, want one unchanged message", s, got)
		}
	}
}

// A run of newlines at a cut is text the agent wrote on purpose. Exactly one is
// consumed — the one the cut landed on, which the message boundary now stands in
// for. Collapsing the rest would silently reformat the agent's output.
func TestChunkForDiscordConsumesOnlyTheNewlineItCutOn(t *testing.T) {
	s := strings.Repeat("x", 50) + "\n\n\n" + strings.Repeat("y", 50)

	got := ChunkForDiscord(s, 60)
	if len(got) < 2 {
		t.Fatalf("expected a split, got %d chunk(s)", len(got))
	}
	// It breaks on the last newline that fits, so the earlier two stay behind.
	if !strings.HasSuffix(got[0], "\n\n") {
		t.Errorf("the deliberate blank lines were swallowed: %q", got[0])
	}
	if strings.HasPrefix(got[1], "\n") {
		t.Errorf("the newline that was cut on was kept as well: %q", got[1])
	}
}
