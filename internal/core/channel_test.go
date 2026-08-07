package core

import "testing"

func TestParseChannelRefDefaultsToDiscord(t *testing.T) {
	// Every channel stored before Slack existed is a bare id, and those rows are
	// not migrated. If this default ever changes, they all point at the wrong
	// chat tool at once.
	p, id, err := ParseChannelRef("123456789012345678")
	if err != nil {
		t.Fatalf("ParseChannelRef(bare) errored: %v", err)
	}
	if p != PlatformDiscord {
		t.Errorf("bare id parsed as %q, want %q", p, PlatformDiscord)
	}
	if id != "123456789012345678" {
		t.Errorf("bare id came back as %q", id)
	}
}

func TestParseChannelRefReadsSlack(t *testing.T) {
	p, id, err := ParseChannelRef("slack:C0123ABCD")
	if err != nil {
		t.Fatalf("ParseChannelRef(slack) errored: %v", err)
	}
	if p != PlatformSlack {
		t.Errorf("parsed as %q, want %q", p, PlatformSlack)
	}
	// Slack ids are uppercase and matched exactly; lowercasing the id along
	// with the prefix would make every binding miss.
	if id != "C0123ABCD" {
		t.Errorf("id came back as %q, want C0123ABCD", id)
	}
}

func TestCanonicalChannelRefCollapsesTheTwoSpellingsOfDiscord(t *testing.T) {
	// The binding table is keyed on the stored reference. If "123" and
	// "discord:123" could both be stored they would be two rows, and two agents
	// could bind one Discord channel — which the Channels spec forbids.
	bare, err := CanonicalChannelRef("123")
	if err != nil {
		t.Fatalf("CanonicalChannelRef(bare) errored: %v", err)
	}
	prefixed, err := CanonicalChannelRef("discord:123")
	if err != nil {
		t.Fatalf("CanonicalChannelRef(prefixed) errored: %v", err)
	}
	if bare != prefixed {
		t.Errorf("two spellings of one Discord channel stored as %q and %q", bare, prefixed)
	}
	if bare != "123" {
		t.Errorf("canonical Discord form is %q, want the bare id", bare)
	}
}

func TestCanonicalChannelRefIsIdempotent(t *testing.T) {
	for _, ref := range []string{"123", "slack:C0123ABCD"} {
		once, err := CanonicalChannelRef(ref)
		if err != nil {
			t.Fatalf("CanonicalChannelRef(%q) errored: %v", ref, err)
		}
		twice, err := CanonicalChannelRef(once)
		if err != nil {
			t.Fatalf("CanonicalChannelRef(%q) errored on the second pass: %v", once, err)
		}
		if once != twice {
			t.Errorf("%q normalised to %q then to %q", ref, once, twice)
		}
	}
}

func TestParseChannelRefRejectsWhatCannotBeDelivered(t *testing.T) {
	for _, ref := range []string{
		"",
		"   ",
		"slack:",
		"slack:   ",
		"teams:C1",
		":123",
	} {
		if _, _, err := ParseChannelRef(ref); err == nil {
			t.Errorf("ParseChannelRef(%q) was accepted; it names no channel this can deliver to", ref)
		}
	}
}

func TestParseChannelRefIgnoresPrefixCaseAndPadding(t *testing.T) {
	p, id, err := ParseChannelRef("  Slack : C0123ABCD  ")
	if err != nil {
		t.Fatalf("ParseChannelRef errored: %v", err)
	}
	if p != PlatformSlack || id != "C0123ABCD" {
		t.Errorf("got (%q, %q), want (slack, C0123ABCD)", p, id)
	}
}

func TestOriginForChannelPicksTheChatTool(t *testing.T) {
	// Both places that rebuild a reply address from a stored channel — a
	// schedule firing and a workflow step — go through this. Before it existed
	// they named Discord unconditionally, so a Slack schedule was delivered to
	// Discord with a Slack id.
	got, err := OriginForChannel("slack:C0123ABCD")
	if err != nil {
		t.Fatalf("OriginForChannel(slack) errored: %v", err)
	}
	if got.Type != OriginSlack || got.ChannelID != "C0123ABCD" {
		t.Errorf("got %+v, want a slack origin for C0123ABCD", got)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("built an origin delivery would refuse: %v", err)
	}

	got, err = OriginForChannel("123")
	if err != nil {
		t.Fatalf("OriginForChannel(bare) errored: %v", err)
	}
	if got.Type != OriginDiscord || got.ChannelID != "123" {
		t.Errorf("got %+v, want a discord origin for 123", got)
	}
}

func TestOriginForChannelRefusesRatherThanGuessing(t *testing.T) {
	if _, err := OriginForChannel("teams:C1"); err == nil {
		t.Error("OriginForChannel accepted an unknown chat tool; a schedule would fire into nowhere")
	}
}

func TestSlackOriginKeepsTheThread(t *testing.T) {
	// A Slack thread is a channel plus the starting message's timestamp.
	// Dropping the timestamp turns a threaded reply into one shouted at the
	// whole channel.
	o := SlackOrigin("C0123ABCD", "1712345678.000100")
	if o.MessageID != "1712345678.000100" {
		t.Errorf("thread timestamp came back as %q", o.MessageID)
	}
	if err := o.Validate(); err != nil {
		t.Errorf("Validate rejected a threaded slack origin: %v", err)
	}
}

func TestSlackOriginCanBeAnAudience(t *testing.T) {
	// An agent delegating from a Slack thread has to be able to speak back into
	// it while it is still working; an address that cannot be an audience is
	// silently dropped and the person hears nothing until the end.
	root := SlackOrigin("C0123ABCD", "1712345678.000100")
	got := AgentOrigin("pm", "thread-1").WithAudience(root)
	listening, ok := got.Listening()
	if !ok {
		t.Fatal("a delegated turn from Slack reports nobody listening")
	}
	if listening.Type != OriginSlack || listening.ChannelID != "C0123ABCD" {
		t.Errorf("audience came back as %+v", listening)
	}
}
