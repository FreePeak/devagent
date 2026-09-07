package ledger

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// This file is the Go port of src/logger.ts (FR-OPS-01): a structured JSONL
// run logger, one append-only file per run under <home>/runs/. Credential
// values must never reach `data` — callers are responsible; known key names
// are redacted defensively.

// RunStage mirrors the TS RunStage union.
type RunStage string

const (
	StageFetch      RunStage = "fetch"
	StagePlan       RunStage = "plan"
	StageImplement  RunStage = "implement"
	StageValidate   RunStage = "validate"
	StagePublish    RunStage = "publish"
	StageFailed     RunStage = "failed"
	StageClarify    RunStage = "clarify"
	StageTask       RunStage = "task"
	StageAudit      RunStage = "audit"
	StageConsume    RunStage = "consume"
	StageSelfUpdate RunStage = "self-update"
	StageScout      RunStage = "scout"
	StageQueue      RunStage = "queue"
	StageCreate     RunStage = "create"
	StageGovernor   RunStage = "governor"
)

// LogLevel mirrors the TS LogEntry level union.
type LogLevel string

const (
	LevelInfo  LogLevel = "info"
	LevelWarn  LogLevel = "warn"
	LevelError LogLevel = "error"
)

// KV is one ordered entry of the `data` object. TS serializes the caller's
// object-literal insertion order; Go maps have random iteration order, so
// callers pass ordered pairs to keep run logs byte-stable.
type KV struct {
	Key   string
	Value any
}

// LogEntry mirrors the TS LogEntry: ts, runId, stage, level, message, data?
// (field order = JSON key order). Data is nil when absent (TS omits the key).
type LogEntry struct {
	TS      string
	RunID   string
	Stage   RunStage
	Level   LogLevel
	Message string
	Data    []KV
}

// MarshalJSON builds the object by hand so the `data` key is omitted when nil
// and its own keys stay in caller order (matching JSON.stringify).
func (e LogEntry) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteString(`{"ts":`)
	b.WriteString(jsonStr(e.TS))
	b.WriteString(`,"runId":`)
	b.WriteString(jsonStr(e.RunID))
	b.WriteString(`,"stage":`)
	b.WriteString(jsonStr(string(e.Stage)))
	b.WriteString(`,"level":`)
	b.WriteString(jsonStr(string(e.Level)))
	b.WriteString(`,"message":`)
	b.WriteString(jsonStr(e.Message))
	if e.Data != nil {
		b.WriteString(`,"data":{`)
		for i, kv := range e.Data {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(jsonStr(kv.Key))
			b.WriteByte(':')
			b.WriteString(jsonValue(kv.Value))
		}
		b.WriteString(`}`)
	}
	b.WriteString(`}`)
	return []byte(b.String()), nil
}

// sensitiveKeys is the TS SENSITIVE_KEYS regex,
// /^(.*?(api[_-]?key|token|secret|password|credential).*?)$/i. The anchored
// lazy form matches any key containing one of the alternatives (case
// insensitively), so a plain substring test is behavior-identical.
var sensitiveKeys = regexp.MustCompile(`(?i)api[_-]?key|token|secret|password|credential`)

// Redact replaces values whose key carries a sensitive substring with
// "[REDACTED]". Top level only — the TS redact never recurses.
func Redact(data []KV) []KV {
	out := make([]KV, len(data))
	for i, kv := range data {
		if sensitiveKeys.MatchString(kv.Key) {
			out[i] = KV{Key: kv.Key, Value: "[REDACTED]"}
		} else {
			out[i] = kv
		}
	}
	return out
}

// RunLogger is the Go RunLogger.
type RunLogger struct {
	runID    string
	filePath string
}

// NewRunLogger opens a new run log under homeDir/runs/<runId>.jsonl. An empty
// homeDir resolves like the TS default: $DEVAGENT_HOME, else $HOME/.devagent.
func NewRunLogger(homeDir string) (*RunLogger, error) {
	if homeDir == "" {
		if v := os.Getenv("DEVAGENT_HOME"); v != "" {
			homeDir = v
		} else if v := os.Getenv("HOME"); v != "" {
			homeDir = filepath.Join(v, ".devagent")
		} else {
			homeDir = ".devagent"
		}
	}
	runsDir := filepath.Join(homeDir, "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		return nil, err
	}
	runID, err := newUUIDv4()
	if err != nil {
		return nil, err
	}
	return &RunLogger{
		runID:    runID,
		filePath: filepath.Join(runsDir, runID+".jsonl"),
	}, nil
}

// RunID returns the run's UUID (TS: readonly runId).
func (l *RunLogger) RunID() string { return l.runID }

// Path returns the JSONL file the logger appends to (TS: get path).
func (l *RunLogger) Path() string { return l.filePath }

// Log appends one entry: mkdir'd at construction, best-effort append.
func (l *RunLogger) Log(stage RunStage, level LogLevel, message string, data []KV) {
	entry := LogEntry{
		TS:      NowISO(),
		RunID:   l.runID,
		Stage:   stage,
		Level:   level,
		Message: message,
	}
	if data != nil {
		entry.Data = Redact(data)
	}
	line, err := marshalLine(entry)
	if err != nil {
		return
	}
	f, err := os.OpenFile(l.filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(line)
}

// Info logs at info level.
func (l *RunLogger) Info(stage RunStage, message string, data []KV) {
	l.Log(stage, LevelInfo, message, data)
}

// Warn logs at warn level.
func (l *RunLogger) Warn(stage RunStage, message string, data []KV) {
	l.Log(stage, LevelWarn, message, data)
}

// Error logs at error level.
func (l *RunLogger) Error(stage RunStage, message string, data []KV) {
	l.Log(stage, LevelError, message, data)
}

// newUUIDv4 is the Go randomUUID: a RFC 4122 version-4 UUID from crypto/rand
// (no external dependency, same 8-4-4-4-12 shape as the TS run ids).
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx
	const hex = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out = append(out, '-')
		}
		out = append(out, hex[v>>4], hex[v&0x0f])
	}
	return string(out), nil
}
