package activity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/roundtable/roundclaw/internal/store"
)

// A delegated result longer than the limit reaches the delegator as its head,
// and the whole of it is kept in the delegator's work area (adr/014).
func TestDeliverToAgentBoundsALongResult(t *testing.T) {
	a, _, pm := notifyHarness(t)
	long := strings.Repeat("가", a.cfg.Routines.ResultLimit) + "THE-TAIL-ONLY-IN-THE-FILE"
	in := agentDelivery()
	in.Result.Text = long
	require.NoError(t, runDeliver(t, a, in))

	conv := "thread-1"
	turns, err := pm.Turns(context.Background(), store.TurnFilter{Limit: 1, Conversation: &conv})
	require.NoError(t, err)
	require.Len(t, turns, 1)
	prompt := turns[0].Request
	assert.NotContains(t, prompt, "THE-TAIL-ONLY-IN-THE-FILE", "the tail must not enter the delegator's context")
	assert.Contains(t, prompt, "inbox/results/")

	pmAgent, err := a.reg.Get(context.Background(), "pm")
	require.NoError(t, err)
	dir, err := a.resolveWorkspace(context.Background(), pmAgent, conv)
	require.NoError(t, err)
	matches, err := filepath.Glob(filepath.Join(dir, "inbox", "results", "*.md"))
	require.NoError(t, err)
	require.Len(t, matches, 1)
	kept, err := os.ReadFile(matches[0])
	require.NoError(t, err)
	assert.Equal(t, long, string(kept))
}

// A short result is delivered whole, with no file.
func TestDeliverToAgentKeepsAShortResultInline(t *testing.T) {
	a, _, pm := notifyHarness(t)
	require.NoError(t, runDeliver(t, a, agentDelivery()))
	conv := "thread-1"
	turns, err := pm.Turns(context.Background(), store.TurnFilter{Limit: 1, Conversation: &conv})
	require.NoError(t, err)
	assert.NotContains(t, turns[0].Request, "inbox/results/")
}
