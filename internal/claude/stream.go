package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// EventKind is the decoded shape of one stream-json line, flattened to what
// roundclaw actually acts on. Anything the CLI emits that does not map to one
// of these becomes KindOther and is preserved verbatim, so a newer CLI cannot
// break the decoder.
type EventKind string

const (
	KindInit       EventKind = "init"        // session started; carries session_id
	KindText       EventKind = "text"        // assistant prose
	KindToolUse    EventKind = "tool_use"    // assistant invoked a tool
	KindToolResult EventKind = "tool_result" // tool returned
	KindAPIRetry   EventKind = "api_retry"   // transient API failure, retrying
	KindRateLimit  EventKind = "rate_limit"  // account rate-limit status
	KindResult     EventKind = "result"      // terminal: final text, cost, error flag
	KindOther      EventKind = "other"
)

// Event is one decoded stream-json line.
type Event struct {
	Kind EventKind

	// SessionID is set on most events; used only to sanity-check that the CLI
	// honoured the session ID we asked for.
	SessionID string

	// Text carries assistant prose (KindText) or the final result (KindResult).
	Text string

	// ToolName and ToolInput describe a KindToolUse. ToolInput is already
	// summarised for display — the raw input can be large and is not stored.
	ToolName  string
	ToolInput string

	// ToolResult is a truncated preview for KindToolResult.
	ToolResult string

	// Retry detail for KindAPIRetry.
	RetryAttempt int
	RetryMax     int
	RetryError   string

	// RateLimitStatus is set on KindRateLimit. The CLI emits one of these on
	// every turn, almost always with status "allowed"; only the other values
	// are worth telling a human about.
	RateLimitStatus string
	RateLimitType   string

	// Terminal fields for KindResult.
	CostUSD float64
	IsError bool

	// Raw is the original line, kept for KindOther so nothing is silently lost.
	Raw string
}

// maxPreview bounds how much of a tool input or result reaches SQLite. Tool
// results can be megabytes; the live log is for a human watching progress, not
// an archive.
const maxPreview = 2000

// wire mirrors the subset of stream-json roundclaw reads. Unknown fields are
// ignored by encoding/json, which is what keeps this forward-compatible.
type wire struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`

	Message *struct {
		Content []struct {
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Name    string          `json:"name"`
			Input   json.RawMessage `json:"input"`
			Content json.RawMessage `json:"content"`
		} `json:"content"`
	} `json:"message"`

	// result
	Result       string  `json:"result"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	IsError      bool    `json:"is_error"`

	// api_retry
	Attempt    int    `json:"attempt"`
	MaxRetries int    `json:"max_retries"`
	Error      string `json:"error"`

	// rate_limit_event
	RateLimitInfo *struct {
		Status        string `json:"status"`
		RateLimitType string `json:"rateLimitType"`
	} `json:"rate_limit_info"`
}

// Decode parses a single stream-json line into zero or more events. One
// assistant message can carry both prose and several tool calls, which is why
// this returns a slice.
func Decode(line []byte) ([]Event, error) {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return nil, nil
	}

	var w wire
	if err := json.Unmarshal([]byte(trimmed), &w); err != nil {
		return nil, fmt.Errorf("decode stream-json line: %w", err)
	}

	switch w.Type {
	case "system":
		switch w.Subtype {
		case "init":
			return []Event{{Kind: KindInit, SessionID: w.SessionID, Raw: trimmed}}, nil
		case "api_retry":
			return []Event{{
				Kind:         KindAPIRetry,
				SessionID:    w.SessionID,
				RetryAttempt: w.Attempt,
				RetryMax:     w.MaxRetries,
				RetryError:   w.Error,
				Raw:          trimmed,
			}}, nil
		}
		return []Event{{Kind: KindOther, SessionID: w.SessionID, Raw: trimmed}}, nil

	case "assistant", "user":
		if w.Message == nil {
			return []Event{{Kind: KindOther, SessionID: w.SessionID, Raw: trimmed}}, nil
		}
		var out []Event
		for _, block := range w.Message.Content {
			switch block.Type {
			case "text":
				if strings.TrimSpace(block.Text) == "" {
					continue
				}
				out = append(out, Event{Kind: KindText, SessionID: w.SessionID, Text: block.Text})
			case "tool_use":
				out = append(out, Event{
					Kind:      KindToolUse,
					SessionID: w.SessionID,
					ToolName:  block.Name,
					ToolInput: preview(string(block.Input)),
				})
			case "tool_result":
				out = append(out, Event{
					Kind:       KindToolResult,
					SessionID:  w.SessionID,
					ToolResult: preview(string(block.Content)),
				})
			}
		}
		return out, nil

	case "rate_limit_event":
		ev := Event{Kind: KindRateLimit, SessionID: w.SessionID, Raw: trimmed}
		if w.RateLimitInfo != nil {
			ev.RateLimitStatus = w.RateLimitInfo.Status
			ev.RateLimitType = w.RateLimitInfo.RateLimitType
		}
		return []Event{ev}, nil

	case "result":
		return []Event{{
			Kind:      KindResult,
			SessionID: w.SessionID,
			Text:      w.Result,
			CostUSD:   w.TotalCostUSD,
			IsError:   w.IsError,
			Raw:       trimmed,
		}}, nil
	}

	return []Event{{Kind: KindOther, SessionID: w.SessionID, Raw: trimmed}}, nil
}

func preview(s string) string {
	if len(s) <= maxPreview {
		return s
	}
	return s[:maxPreview] + fmt.Sprintf("… (%d bytes truncated)", len(s)-maxPreview)
}

// maxLineBytes bounds a single stream-json line. A tool result embedded in one
// line can be very large, and bufio.Scanner's default 64KiB limit would turn
// that into a spurious "token too long" failure mid-turn.
const maxLineBytes = 16 << 20

// Scan reads stream-json lines from r and calls fn for every decoded event.
//
// A line that fails to decode is reported through onError and skipped rather
// than aborting the turn: losing one log line is far better than killing an
// agent run that is otherwise healthy. A fn error does abort, because that
// means persistence is broken.
func Scan(r io.Reader, fn func(Event) error, onError func(error)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	for sc.Scan() {
		events, err := Decode(sc.Bytes())
		if err != nil {
			if onError != nil {
				onError(err)
			}
			continue
		}
		for _, ev := range events {
			if err := fn(ev); err != nil {
				return err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read claude output: %w", err)
	}
	return nil
}
