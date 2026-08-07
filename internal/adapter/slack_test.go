package adapter

import (
	"strings"
	"testing"
)

// A conversation ID ends up in a Temporal workflow ID and in a directory name
// under the agent's workspace, so a Slack timestamp is checked before it is
// used as one rather than trusted for being well-formed so far.
func TestSlackConversationRefusesAnythingButATimestamp(t *testing.T) {
	for _, ts := range []string{
		"../../etc/passwd",
		"1712345678.000100/../..",
		"abc",
		"1712345678 000100",
	} {
		if _, err := slackConversation(ts); err == nil {
			t.Errorf("slackConversation(%q) was accepted; it becomes a path segment", ts)
		}
	}
}

func TestSlackConversationIsStableAndPathSafe(t *testing.T) {
	got, err := slackConversation("1712345678.000100")
	if err != nil {
		t.Fatalf("slackConversation: %v", err)
	}
	if got != "1712345678-000100" {
		t.Errorf("got %q, want the dot replaced", got)
	}
	// The same thread must resolve to the same conversation every time, or a
	// second message in it would start a fresh session and lose the context.
	again, _ := slackConversation("1712345678.000100")
	if again != got {
		t.Errorf("the same thread produced %q then %q", got, again)
	}
	if strings.ContainsAny(got, "./\\") {
		t.Errorf("%q still carries a path separator", got)
	}
}

// No thread means the channel itself, which is the agent's default
// conversation — the one /ask, schedules and webhooks share.
func TestSlackConversationEmptyIsTheDefaultConversation(t *testing.T) {
	got, err := slackConversation("")
	if err != nil {
		t.Fatalf("slackConversation(\"\"): %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want the default conversation", got)
	}
}

// Slack gives a slash command one free-text field, so the subcommand and its
// argument are cut out of it.
func TestCutWord(t *testing.T) {
	for _, tc := range []struct{ in, head, rest string }{
		{"create", "create", ""},
		{"edit pm", "edit", "pm"},
		{"  edit   pm  ", "edit", "pm"},
		{"pm 리포트 좀 정리해줘", "pm", "리포트 좀 정리해줘"},
		{"pm\n여러 줄\n프롬프트", "pm", "여러 줄\n프롬프트"},
		{"", "", ""},
	} {
		head, rest := cutWord(tc.in)
		if head != tc.head || rest != tc.rest {
			t.Errorf("cutWord(%q) = (%q, %q), want (%q, %q)", tc.in, head, rest, tc.head, tc.rest)
		}
	}
}

// The shared formatters emit Discord's bold. Slack renders ** literally, so a
// status report would arrive full of asterisks without this.
func TestSlackMarkupConvertsBold(t *testing.T) {
	got := slackMarkup("**pm** — running (turn #3)")
	if got != "*pm* — running (turn #3)" {
		t.Errorf("got %q", got)
	}
	if strings.Contains(got, "**") {
		t.Errorf("%q still carries Discord's bold", got)
	}
}

func TestSlackMarkupLeavesEverythingElseAlone(t *testing.T) {
	in := "`code` _italic_ and a lone * asterisk\n```\nblock\n```"
	if got := slackMarkup(in); got != in {
		t.Errorf("got %q, want it unchanged", got)
	}
}

// A stored channel reference is rendered as a link with the tool prefix
// removed: both tools spell a channel link <#id>, and "<#slack:C1>" links to
// nothing.
func TestChannelLabelDropsTheToolPrefix(t *testing.T) {
	if got := channelLabel("slack:C0123ABCD"); got != "<#C0123ABCD>" {
		t.Errorf("got %q", got)
	}
	if got := channelLabel("123456789"); got != "<#123456789>" {
		t.Errorf("got %q", got)
	}
}

// Shown rather than hidden: somebody has to be able to see what is wrong with
// a binding in order to fix it.
func TestChannelLabelShowsAnUnreadableReference(t *testing.T) {
	got := channelLabel("teams:C1")
	if !strings.Contains(got, "teams:C1") || !strings.Contains(got, "unreadable") {
		t.Errorf("got %q, want the bad reference shown and named", got)
	}
}

// The view carries what the interaction that opened it knew, because a
// submission is a separate interaction that remembers none of it.
func TestSplitMetadata(t *testing.T) {
	channel, target := splitMetadata("C0123ABCD\npm")
	if channel != "C0123ABCD" || target != "pm" {
		t.Errorf("got (%q, %q)", channel, target)
	}

	// The steer form stashes three values; the second split reads the rest.
	channel, rest := splitMetadata("C0123ABCD\npm\n1712345678.000100")
	agent, thread := splitMetadata(rest)
	if channel != "C0123ABCD" || agent != "pm" || thread != "1712345678.000100" {
		t.Errorf("got (%q, %q, %q)", channel, agent, thread)
	}

	// A create form has no target, and must not read the channel as one.
	channel, target = splitMetadata("C0123ABCD\n")
	if channel != "C0123ABCD" || target != "" {
		t.Errorf("got (%q, %q)", channel, target)
	}
}

func TestTruncateRunesCountsCharacters(t *testing.T) {
	// Slack's limits count characters. Cutting "회의록 정리" at a byte offset
	// would send a broken character.
	if got := truncateRunes("회의록 정리", 3); got != "회의록" {
		t.Errorf("got %q, want 회의록", got)
	}
	if got := truncateRunes("short", 99); got != "short" {
		t.Errorf("got %q", got)
	}
}
