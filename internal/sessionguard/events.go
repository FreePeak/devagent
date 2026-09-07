// Port of src/sessionguard/events.ts: classification of Claude Code
// headless stream-json output.
//
// Reference: https://code.claude.com/docs/en/headless.md and empirical
// inspection of v2.1.x output. Terminal API failures are never retried by
// Claude Code once a response started streaming ("Connection lost
// mid-response"); recovery requires starting a new turn in the same session,
// which is what the guard supervisor does.

package sessionguard

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Stream event kinds returned by ParseStreamLine.
const (
	KindInit           = "init"
	KindAPIRetry       = "api_retry"
	KindResult         = "result"
	KindSyntheticError = "synthetic_error"
	KindOther          = "other"
)

// StreamEvent is the classified form of one stream-json output line. Fields
// not relevant to the event's Kind are left at their zero value (pointers nil
// for "absent", mirroring the TS optional fields).
type StreamEvent struct {
	Kind string // one of the Kind* constants

	// init / result
	SessionID string

	// api_retry
	Attempt     int
	MaxRetries  *int
	DelayMs     *int
	ErrorStatus *int // nil = null in the TS payload
	Error       string

	// result
	IsError bool

	// synthetic_error
	Text string
}

type rawMessageBody struct {
	Model   string          `json:"model,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
}

type rawStreamLine struct {
	Type              string          `json:"type,omitempty"`
	Subtype           string          `json:"subtype,omitempty"`
	SessionID         string          `json:"session_id,omitempty"`
	SessionIDCamel    string          `json:"sessionId,omitempty"`
	Attempt           *int            `json:"attempt,omitempty"`
	MaxRetries        *int            `json:"max_retries,omitempty"`
	RetryDelayMs      *int            `json:"retry_delay_ms,omitempty"`
	ErrorStatus       *int            `json:"error_status,omitempty"`
	Error             *string         `json:"error,omitempty"`
	IsError           bool            `json:"is_error,omitempty"`
	Message           *rawMessageBody `json:"message,omitempty"`
	IsAPIErrorMessage bool            `json:"isApiErrorMessage,omitempty"`
}

// nonRetryablePatterns: errors that will never succeed on retry; abort
// instead of burning attempts.
var nonRetryablePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)invalid api key`),
	regexp.MustCompile(`(?i)authentication`),
	regexp.MustCompile(`(?i)unauthorized`),
	regexp.MustCompile(`(?i)credit balance`),
	regexp.MustCompile(`(?i)billing`),
	regexp.MustCompile(`(?i)not found.*model|model.*not found`),
}

// IsNonRetryableApiError reports whether the text matches one of the
// terminal, never-retryable API failure patterns.
func IsNonRetryableApiError(text string) bool {
	for _, p := range nonRetryablePatterns {
		if p.MatchString(text) {
			return true
		}
	}
	return false
}

// ParseStreamLine classifies one line of Claude Code stream-json output.
func ParseStreamLine(line string) StreamEvent {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return StreamEvent{Kind: KindOther}
	}
	var raw rawStreamLine
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return StreamEvent{Kind: KindOther}
	}
	if raw.Type == "system" && raw.Subtype == "init" {
		sessionID := raw.SessionID
		if sessionID == "" {
			sessionID = raw.SessionIDCamel
		}
		if sessionID != "" {
			return StreamEvent{Kind: KindInit, SessionID: sessionID}
		}
		return StreamEvent{Kind: KindOther}
	}
	if raw.Type == "system" && raw.Subtype == "api_retry" {
		event := StreamEvent{Kind: KindAPIRetry, Error: "unknown"}
		if raw.Attempt != nil {
			event.Attempt = *raw.Attempt
		}
		event.MaxRetries = raw.MaxRetries
		event.DelayMs = raw.RetryDelayMs
		event.ErrorStatus = raw.ErrorStatus
		if raw.Error != nil {
			event.Error = *raw.Error
		}
		return event
	}
	if raw.Type == "assistant" &&
		(raw.IsAPIErrorMessage || (raw.Message != nil && raw.Message.Model == "<synthetic>")) {
		text := ""
		if raw.Message != nil {
			text = syntheticText(raw.Message.Content)
		}
		return StreamEvent{Kind: KindSyntheticError, Text: text}
	}
	if raw.Type == "result" {
		sessionID := raw.SessionID
		if sessionID == "" {
			sessionID = raw.SessionIDCamel
		}
		return StreamEvent{Kind: KindResult, IsError: raw.IsError, SessionID: sessionID}
	}
	return StreamEvent{Kind: KindOther}
}

// syntheticText renders a synthetic assistant error message content: a plain
// string passes through; a content array joins its text blocks with spaces.
func syntheticText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var decoded any
	if err := json.Unmarshal(content, &decoded); err != nil {
		return ""
	}
	switch v := decoded.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, block := range v {
			obj, ok := block.(map[string]any)
			if !ok {
				parts = append(parts, "")
				continue
			}
			if text, ok := obj["text"]; ok {
				parts = append(parts, fmt.Sprint(text))
			} else {
				parts = append(parts, "")
			}
		}
		kept := make([]string, 0, len(parts))
		for _, p := range parts {
			if p != "" {
				kept = append(kept, p)
			}
		}
		return strings.Join(kept, " ")
	default:
		return ""
	}
}
