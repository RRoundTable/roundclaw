package core

// ModelUsage is what one model consumed during a piece of work, as the CLI
// reported it. Tokens are the fact; CostUSD is the CLI's own figure, recorded
// as given. roundclaw keeps no price table: the CLI knows the price of the model
// it ran, and a table of ours would go stale (adr/006).
type ModelUsage struct {
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// Usage breaks a piece of work's consumption down by model. A single turn can
// touch more than one model — a smaller one for background work — which is why
// this is a map and not one set of counters.
type Usage struct {
	Models map[string]ModelUsage `json:"models,omitempty"`
	// Turns is the number of model round-trips the CLI made.
	Turns int `json:"turns,omitempty"`
	// APIMS is time spent waiting on the model, as opposed to wall time.
	APIMS int64 `json:"api_ms,omitempty"`
}

// Add folds other into u, model by model.
func (u *Usage) Add(other Usage) {
	if len(other.Models) > 0 && u.Models == nil {
		u.Models = map[string]ModelUsage{}
	}
	for name, m := range other.Models {
		cur := u.Models[name]
		cur.InputTokens += m.InputTokens
		cur.OutputTokens += m.OutputTokens
		cur.CacheReadTokens += m.CacheReadTokens
		cur.CacheWriteTokens += m.CacheWriteTokens
		cur.CostUSD += m.CostUSD
		u.Models[name] = cur
	}
	u.Turns += other.Turns
	u.APIMS += other.APIMS
}

// Tokens is the total of every kind across every model.
func (u Usage) Tokens() int64 {
	var n int64
	for _, m := range u.Models {
		n += m.InputTokens + m.OutputTokens + m.CacheReadTokens + m.CacheWriteTokens
	}
	return n
}
