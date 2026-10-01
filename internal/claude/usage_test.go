package claude

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The field names below are the ones the Claude Code SDK documents for the
// result and init events (modelUsage, usage, num_turns, duration_api_ms, model).
func TestDecodeResultCarriesUsagePerModel(t *testing.T) {
	line := `{"type":"result","subtype":"success","result":"ok","total_cost_usd":0.42,` +
		`"is_error":false,"num_turns":7,"duration_api_ms":12345,` +
		`"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40},` +
		`"modelUsage":{"claude-opus-5-5":{"inputTokens":9,"outputTokens":18,"cacheReadInputTokens":27,"cacheCreationInputTokens":36,"costUSD":0.40,"costBasis":"list"},` +
		`"claude-haiku-4-5":{"inputTokens":1,"outputTokens":2,"cacheReadInputTokens":3,"cacheCreationInputTokens":4,"costUSD":0.02}}}`

	evs, err := Decode([]byte(line))
	require.NoError(t, err)
	require.Len(t, evs, 1)
	u := evs[0].Usage
	assert.Equal(t, 7, u.Turns)
	assert.Equal(t, int64(12345), u.APIMS)
	require.Len(t, u.Models, 2)
	opus := u.Models["claude-opus-5-5"]
	assert.Equal(t, int64(9), opus.InputTokens)
	assert.Equal(t, int64(18), opus.OutputTokens)
	assert.Equal(t, int64(27), opus.CacheReadTokens)
	assert.Equal(t, int64(36), opus.CacheWriteTokens)
	assert.InDelta(t, 0.40, opus.CostUSD, 1e-9)
	assert.Equal(t, int64(100), u.Tokens())
}

// An older CLI reports aggregate usage only; the tokens are kept rather than
// lost, under a key the caller relabels with the init event's model.
func TestDecodeResultWithoutModelUsageFallsBack(t *testing.T) {
	line := `{"type":"result","result":"ok","total_cost_usd":0.1,` +
		`"usage":{"input_tokens":5,"output_tokens":6,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`
	evs, err := Decode([]byte(line))
	require.NoError(t, err)
	m, ok := evs[0].Usage.Models[UnattributedModel]
	require.True(t, ok)
	assert.Equal(t, int64(5), m.InputTokens)
	assert.InDelta(t, 0.1, m.CostUSD, 1e-9)
}

func TestDecodeInitCarriesModel(t *testing.T) {
	evs, err := Decode([]byte(`{"type":"system","subtype":"init","session_id":"s","model":"claude-opus-5-5"}`))
	require.NoError(t, err)
	assert.Equal(t, KindInit, evs[0].Kind)
	assert.Equal(t, "claude-opus-5-5", evs[0].Model)
}

// The fake CLI and CLIs that predate usage reporting emit neither field.
func TestDecodeResultWithoutAnyUsageIsEmpty(t *testing.T) {
	evs, err := Decode([]byte(`{"type":"result","result":"ok","total_cost_usd":0.01}`))
	require.NoError(t, err)
	assert.Empty(t, evs[0].Usage.Models)
}
