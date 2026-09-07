// Package trust is the Go port of the trust-gate helpers in src/prompt.ts
// (PRD §18 Q11): the one-time per-repo `.devagent/AGENTS.md` auto-load confirm.
package trust

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// File is the repo-relative one-time trust record written by
// `devagent trust agents-md` (TS: TRUST_FILE).
const File = ".devagent/trust.json"

// AgentsMdFile is the auto-loaded per-repo context file (TS: AGENTS_MD_FILE).
const AgentsMdFile = ".devagent/AGENTS.md"

// Mode is the trust-gate mode for config `context.agentsMd`; default "ask".
type Mode = string

const (
	ModeAsk Mode = "ask"
	ModeOn  Mode = "on"
	ModeOff Mode = "off"
)

// IsAgentsMdTrusted: whether the operator approved auto-loading
// `<repo>/.devagent/AGENTS.md`. The record is written by RecordAgentsMd
// (`devagent trust agents-md`); absent or malformed = untrusted.
func IsAgentsMdTrusted(repoPath string) bool {
	raw, err := os.ReadFile(filepath.Join(repoPath, File))
	if err != nil {
		return false
	}
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		return false
	}
	b, ok := rec["agentsMd"].(bool)
	return ok && b
}

// RecordAgentsMd records the one-time per-repo approval into
// `<repo>/.devagent/trust.json`, preserving any other trust keys already
// present. Returns the trust-file path written.
func RecordAgentsMd(repoPath string) (string, error) {
	p := filepath.Join(repoPath, File)
	rec := map[string]any{}
	if raw, err := os.ReadFile(p); err == nil {
		var existing map[string]any
		if json.Unmarshal(raw, &existing) == nil && existing != nil {
			rec = existing
		}
	}
	rec["agentsMd"] = true
	rec["agentsMdTrustedAt"] = time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	blob, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(p, append(blob, '\n'), 0o644); err != nil {
		return "", err
	}
	return p, nil
}

// LoadAgentsMd loads `<repo>/.devagent/AGENTS.md` behind the Q11 trust gate:
// ModeOff never reads the file; ModeOn always injects; ModeAsk (the default)
// injects nothing until the operator confirms once via
// `devagent trust agents-md`. A missing or unreadable file degrades to "" so
// prompts stay byte-identical.
func LoadAgentsMd(repoPath string, mode Mode) string {
	if mode == ModeOff {
		return ""
	}
	if mode == ModeAsk && !IsAgentsMdTrusted(repoPath) {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(repoPath, AgentsMdFile))
	if err != nil {
		return ""
	}
	return trimSpace(string(raw))
}

// trimSpace mirrors the TS .trim() (whitespace incl. BOM-adjacent spaces).
func trimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\v', '\f', '\r':
		return true
	}
	return false
}
