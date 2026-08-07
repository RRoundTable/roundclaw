package registry

import (
	"errors"
	"testing"
)

// A channel names exactly one agent, and that holds across chat tools.
func TestABoundChannelCannotBeTakenByASecondAgent(t *testing.T) {
	s := newStore(t)

	if _, err := s.Create(t.Context(), Agent{
		ID: "pm", DiscordChannels: []string{"slack:C0123ABCD"}, Enabled: true,
	}); err != nil {
		t.Fatalf("create pm: %v", err)
	}

	_, err := s.Create(t.Context(), Agent{
		ID: "dev", DiscordChannels: []string{"slack:C0123ABCD"}, Enabled: true,
	})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("second binding returned %v, want a conflict", err)
	}
}

// The spelling that would have slipped past that check.
//
// Bindings are keyed on the stored reference, so if "123" and "discord:123"
// were stored as written they would be two rows for one Discord channel — and
// two agents would answer in it, each unaware of the other.
func TestTheTwoSpellingsOfADiscordChannelAreOneBinding(t *testing.T) {
	s := newStore(t)

	if _, err := s.Create(t.Context(), Agent{
		ID: "pm", DiscordChannels: []string{"123456789"}, Enabled: true,
	}); err != nil {
		t.Fatalf("create pm: %v", err)
	}

	_, err := s.Create(t.Context(), Agent{
		ID: "dev", DiscordChannels: []string{"discord:123456789"}, Enabled: true,
	})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("binding the same channel by its other spelling returned %v, want a conflict", err)
	}
}

// A Slack channel and a Discord channel that happen to share an id are two
// different channels, and each may have its own agent.
func TestTheSameIdInTwoChatToolsIsTwoChannels(t *testing.T) {
	s := newStore(t)

	if _, err := s.Create(t.Context(), Agent{
		ID: "pm", DiscordChannels: []string{"C0123ABCD"}, Enabled: true,
	}); err != nil {
		t.Fatalf("create pm: %v", err)
	}
	if _, err := s.Create(t.Context(), Agent{
		ID: "dev", DiscordChannels: []string{"slack:C0123ABCD"}, Enabled: true,
	}); err != nil {
		t.Fatalf("binding the same id in the other chat tool was refused: %v", err)
	}

	got, err := s.ByChannel(t.Context(), "slack:C0123ABCD")
	if err != nil {
		t.Fatalf("lookup slack channel: %v", err)
	}
	if got.ID != "dev" {
		t.Errorf("slack:C0123ABCD resolved to %s, want dev", got.ID)
	}

	got, err = s.ByChannel(t.Context(), "C0123ABCD")
	if err != nil {
		t.Fatalf("lookup discord channel: %v", err)
	}
	if got.ID != "pm" {
		t.Errorf("C0123ABCD resolved to %s, want pm", got.ID)
	}
}

// A lookup normalises the same way a write does, or a channel bound as "123"
// would be invisible to a caller that spelled it "discord:123".
func TestByChannelFindsAChannelByEitherSpelling(t *testing.T) {
	s := newStore(t)

	if _, err := s.Create(t.Context(), Agent{
		ID: "pm", DiscordChannels: []string{"123456789"}, Enabled: true,
	}); err != nil {
		t.Fatalf("create pm: %v", err)
	}

	for _, ref := range []string{"123456789", "discord:123456789", " Discord : 123456789 "} {
		got, err := s.ByChannel(t.Context(), ref)
		if err != nil {
			t.Errorf("ByChannel(%q): %v", ref, err)
			continue
		}
		if got.ID != "pm" {
			t.Errorf("ByChannel(%q) = %s, want pm", ref, got.ID)
		}
	}
}

// A binding nothing could deliver to is refused when it is written, not
// discovered when a reply has already been paid for and has nowhere to go.
func TestAnUnreadableChannelIsRefusedAtWrite(t *testing.T) {
	s := newStore(t)

	_, err := s.Create(t.Context(), Agent{
		ID: "pm", DiscordChannels: []string{"teams:C1"}, Enabled: true,
	})
	if err == nil {
		t.Fatal("an agent bound to an unknown chat tool was accepted")
	}
}

// Round-tripping keeps the reference as stored, so an edit that does not touch
// the channels list cannot quietly rewrite it.
func TestChannelReferencesSurviveAnUpdate(t *testing.T) {
	s := newStore(t)

	if _, err := s.Create(t.Context(), Agent{
		ID: "pm", DiscordChannels: []string{"slack:C0123ABCD", "123456789"}, Enabled: true,
	}); err != nil {
		t.Fatalf("create pm: %v", err)
	}

	got, err := s.Get(t.Context(), "pm")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := s.Update(t.Context(), got); err != nil {
		t.Fatalf("update: %v", err)
	}

	after, err := s.Get(t.Context(), "pm")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if len(after.DiscordChannels) != 2 {
		t.Fatalf("channels after update = %v, want both kept", after.DiscordChannels)
	}
	for _, want := range []string{"slack:C0123ABCD", "123456789"} {
		found := false
		for _, ch := range after.DiscordChannels {
			if ch == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q was not kept; channels are %v", want, after.DiscordChannels)
		}
	}
}
