// Port of src/sessionguard/transcript.ts: detect sessions whose last
// assistant turn died on an API error. Interrupted turns persist as
// synthetic assistant messages (`isApiErrorMessage: true`) with no
// successful assistant message after them. Resume such a session with
// `claude --resume <sessionId>`.

package sessionguard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// TranscriptStatus summarizes one Claude Code transcript file.
type TranscriptStatus struct {
	File          string
	SessionID     string // "" when the transcript never named a session
	Interrupted   bool
	LastErrorText string // "" when none
	LastTimestamp string // "" when no entry carried a timestamp
}

type transcriptLine struct {
	Type              string          `json:"type,omitempty"`
	SessionID         string          `json:"sessionId,omitempty"`
	SessionIDSnake    string          `json:"session_id,omitempty"`
	Timestamp         string          `json:"timestamp,omitempty"`
	IsAPIErrorMessage bool            `json:"isApiErrorMessage,omitempty"`
	Message           *rawMessageBody `json:"message,omitempty"`
}

// InspectTranscript walks a JSONL transcript and reports whether the last
// assistant turn is a synthetic API error. A read failure returns an error.
func InspectTranscript(path string) (TranscriptStatus, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return TranscriptStatus{}, err
	}
	status := TranscriptStatus{File: path, Interrupted: false}

	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "{") {
			continue
		}
		var entry transcriptLine
		if err := json.Unmarshal([]byte(trimmed), &entry); err != nil {
			continue
		}
		if status.SessionID == "" {
			status.SessionID = entry.SessionID
			if status.SessionID == "" {
				status.SessionID = entry.SessionIDSnake
			}
		}
		if entry.Timestamp != "" {
			status.LastTimestamp = entry.Timestamp
		}
		if entry.Type == "assistant" {
			if entry.IsAPIErrorMessage || (entry.Message != nil && entry.Message.Model == "<synthetic>") {
				status.Interrupted = true
				status.LastErrorText = ""
				if entry.Message != nil {
					status.LastErrorText = extractText(entry.Message.Content)
				}
			} else {
				// A real assistant response after the failure means the session
				// already continued past it.
				status.Interrupted = false
				status.LastErrorText = ""
			}
		}
	}
	return status, nil
}

// extractText renders assistant message content: a plain string passes
// through; a content array joins its non-empty text blocks with spaces.
// An empty join yields "" (the TS `|| undefined`).
func extractText(content json.RawMessage) string {
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

// LatestTranscript returns the newest .jsonl transcript (by mtime) in a
// project slug dir (e.g. ~/.claude/projects/<slug>); "" when none.
func LatestTranscript(projectDir string) string {
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return ""
	}
	var newestFile string
	var newestMtime int64
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		file := filepath.Join(projectDir, name)
		info, err := entry.Info()
		if err != nil {
			continue
		}
		mtime := info.ModTime().UnixMilli()
		if newestFile == "" || mtime > newestMtime {
			newestFile = file
			newestMtime = mtime
		}
	}
	return newestFile
}

// ClaudeProjectsDir returns the default projects directory honoring
// CLAUDE_CONFIG_DIR like Claude Code does.
func ClaudeProjectsDir(home string) string {
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if configDir == "" {
		configDir = filepath.Join(home, ".claude")
	}
	return filepath.Join(configDir, "projects")
}

var slugPattern = regexp.MustCompile(`[/.]`)

// ProjectSlug encodes a working directory into a project slug the way
// Claude Code does (every '/' and '.' becomes '-').
func ProjectSlug(cwd string) string {
	return slugPattern.ReplaceAllString(cwd, "-")
}
