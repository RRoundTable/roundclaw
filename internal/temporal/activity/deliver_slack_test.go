package activity

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/store"
)

// recordingSlack stands in for the Slack Web API. SlackSender is declared in
// roundclaw's own terms precisely so this needs no library.
type recordingSlack struct {
	mu   sync.Mutex
	sent []string // "channel/thread: text"
}

func (r *recordingSlack) PostMessage(_ context.Context, channelID, threadTS, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, channelID+"/"+threadTS+": "+text)
	return nil
}

func (r *recordingSlack) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sent...)
}

func TestDeliverSlackAnswersInTheThreadItWasAskedIn(t *testing.T) {
	a, _, _ := notifyHarness(t)
	sink := &recordingSlack{}
	a.slack = sink

	err := runDeliver(t, a, DeliverInput{
		AgentID: "dev",
		Origin:  core.SlackOrigin("C0123ABCD", "1712345678.000100"),
		Result:  core.TurnResult{TurnID: 1, Status: core.TurnDone, Text: "다 됐습니다"},
	})
	if err != nil {
		t.Fatalf("DeliverResponse: %v", err)
	}

	got := sink.messages()
	if len(got) != 1 || got[0] != "C0123ABCD/1712345678.000100: 다 됐습니다" {
		t.Errorf("sent %v, want the answer in the thread that asked", got)
	}
}

// Every chunk carries the thread, not just the first. Posting the rest bare
// would drop the tail of a long answer into the channel, out of the
// conversation it belongs to.
func TestDeliverSlackKeepsEveryChunkInTheThread(t *testing.T) {
	a, _, _ := notifyHarness(t)
	sink := &recordingSlack{}
	a.slack = sink

	long := strings.Repeat("한 줄입니다.\n", 900)
	err := runDeliver(t, a, DeliverInput{
		AgentID: "dev",
		Origin:  core.SlackOrigin("C0123ABCD", "1712345678.000100"),
		Result:  core.TurnResult{TurnID: 1, Status: core.TurnDone, Text: long},
	})
	if err != nil {
		t.Fatalf("DeliverResponse: %v", err)
	}

	got := sink.messages()
	if len(got) < 2 {
		t.Fatalf("got %d message(s); this text should have been split", len(got))
	}
	for i, m := range got {
		if !strings.HasPrefix(m, "C0123ABCD/1712345678.000100: ") {
			t.Errorf("chunk %d left the thread: %.40s…", i, m)
		}
	}
}

// Splitting to Slack's limit rather than Discord's. A reply that fits in one
// Slack message must not arrive as two because Discord would have needed two.
func TestDeliverSlackSplitsAtSlacksLimit(t *testing.T) {
	a, _, _ := notifyHarness(t)
	sink := &recordingSlack{}
	a.slack = sink

	// Comfortably over Discord's 2000 and under Slack's 4000.
	text := strings.Repeat("가", 3000)
	err := runDeliver(t, a, DeliverInput{
		AgentID: "dev",
		Origin:  core.SlackOrigin("C0123ABCD", ""),
		Result:  core.TurnResult{TurnID: 1, Status: core.TurnDone, Text: text},
	})
	if err != nil {
		t.Fatalf("DeliverResponse: %v", err)
	}
	if got := sink.messages(); len(got) != 1 {
		t.Errorf("got %d messages, want 1: this fits in a single Slack message", len(got))
	}
}

// A chat tool that is not configured must fail without retrying: no number of
// attempts teaches this binary a credential it was not given, and the result is
// safe in SQLite either way.
func TestDeliverSlackWithNoClientIsNotRetried(t *testing.T) {
	a, _, _ := notifyHarness(t)

	err := runDeliver(t, a, DeliverInput{
		AgentID: "dev",
		Origin:  core.SlackOrigin("C0123ABCD", ""),
		Result:  core.TurnResult{TurnID: 1, Status: core.TurnDone, Text: "hi"},
	})
	if err == nil {
		t.Fatal("delivery to an unconfigured slack succeeded")
	}
	if !strings.Contains(err.Error(), "no slack client") {
		t.Errorf("error = %v, want it to name the missing client", err)
	}
}

// A failed turn says why in the channel rather than going quiet. Shared with
// the Discord path so the two cannot describe the same failure differently.
func TestDeliverSlackReportsAFailedTurn(t *testing.T) {
	a, _, _ := notifyHarness(t)
	sink := &recordingSlack{}
	a.slack = sink

	err := runDeliver(t, a, DeliverInput{
		AgentID: "dev",
		Origin:  core.SlackOrigin("C0123ABCD", ""),
		Result: core.TurnResult{
			TurnID: 1, Status: core.TurnError, ErrorMessage: "컨테이너가 죽었습니다",
		},
	})
	if err != nil {
		t.Fatalf("DeliverResponse: %v", err)
	}
	got := sink.messages()
	if len(got) != 1 || !strings.Contains(got[0], "컨테이너가 죽었습니다") {
		t.Errorf("sent %v, want the failure reported", got)
	}
}

// The reason replyOriginFor has to know about Slack.
//
// A delegated turn's result comes back as a new turn for the delegator, and the
// address that turn answers to is read out of the conversation's own history.
// Before Slack was listed there, a conversation living in a Slack thread looked
// like one with no audience, and the delegator's report was recorded and never
// seen.
func TestANotificationAnswersIntoASlackConversation(t *testing.T) {
	a, sig, pm := notifyHarness(t)
	sink := &recordingSlack{}
	a.slack = sink

	// A second conversation of pm, this one living in Slack.
	if _, _, err := pm.CreateTurn(context.Background(), store.NewTurn{
		Request:      "dev한테 시켜줘",
		Origin:       core.SlackOrigin("C0123ABCD", "1712345678.000100"),
		Conversation: "1712345678-000100",
	}); err != nil {
		t.Fatalf("seed pm slack turn: %v", err)
	}

	err := runDeliver(t, a, DeliverInput{
		AgentID: "dev",
		Origin:  core.AgentOrigin("pm", "1712345678-000100"),
		Result:  core.TurnResult{TurnID: 7, Status: core.TurnDone, Text: "끝냈습니다"},
	})
	if err != nil {
		t.Fatalf("DeliverResponse: %v", err)
	}

	if len(sig.sent()) != 1 {
		t.Fatalf("woke %d workflows, want 1", len(sig.sent()))
	}

	turns, err := pm.RecentTurnsIn(context.Background(), "1712345678-000100", 1)
	if err != nil {
		t.Fatalf("read pm conversation: %v", err)
	}
	if len(turns) == 0 {
		t.Fatal("no turn was queued for pm")
	}
	if got := turns[0].Origin; got.Type != core.OriginSlack || got.ChannelID != "C0123ABCD" {
		t.Errorf("the notification answers to %s, want pm's slack thread", got)
	}
}
