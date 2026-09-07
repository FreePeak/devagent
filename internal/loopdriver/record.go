package loopdriver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// rowTimeFormat is the second-precision UTC stamp the bash driver bakes into
// every ledger/event row (`date -u +%FT%TZ`).
const rowTimeFormat = "2006-01-02T15:04:05Z"

func rowTimestamp(now func() time.Time) string {
	return now().UTC().Format(rowTimeFormat)
}

// truncateRunes mirrors `cut -c1-<n>` in a UTF-8 locale (characters, not
// bytes).
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// rowText mirrors the bash row-text transform:
// `tr '\n\t' '  ' | tr -d '"' | cut -c1-<cap>` — each newline/tab becomes a
// single space, every double quote is deleted, capped at cap characters.
func rowText(s string, cap int) string {
	replacer := strings.NewReplacer("\n", " ", "\t", " ", `"`, "")
	return truncateRunes(replacer.Replace(s), cap)
}

func marshalRow(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // bash printf writes raw text, no HTML escaping
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func appendJSONL(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := marshalRow(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

// ledgerRow is the .selfbuild/ledger.jsonl row shape (key order loop, ts,
// status, goal — byte-compatible with the bash printf).
type ledgerRow struct {
	Loop   int    `json:"loop"`
	TS     string `json:"ts"`
	Status string `json:"status"`
	Goal   string `json:"goal"`
}

// loopResultEvent mirrors the Q39 loop-result telemetry row in
// .devagent/runs/orchestration/events.jsonl.
type loopResultEvent struct {
	TS     string `json:"ts"`
	Kind   string `json:"kind"`
	Event  string `json:"event"`
	Loop   int    `json:"loop"`
	Status string `json:"status"`
	Goal   string `json:"goal"`
}

// loopPhaseEvent is the human-visible phase breadcrumb row
// (sync → preflight → research → issue/po → task) on the same events stream
// the daemon SSE + TUI history follow. Detail is omitted when empty, exactly
// like the bash conditional `,,"detail":"..."` splice.
type loopPhaseEvent struct {
	TS     string `json:"ts"`
	Kind   string `json:"kind"`
	Event  string `json:"event"`
	Loop   int    `json:"loop"`
	Phase  string `json:"phase"`
	Detail string `json:"detail,omitempty"`
}

// driver bundles the per-run configuration the record/phase writers and the
// dispatch helpers need.
type driver struct {
	cfg      LoopConfig
	stateDir string
	// fails points at RunLoop's consecutive-failure counter (the bash
	// `fails` variable) so nested phase helpers can increment it.
	fails *int
}

func eventsPath(repo string) string {
	return filepath.Join(repo, ".devagent", "runs", "orchestration", "events.jsonl")
}

// record ports the bash record(): append the ledger row, publish state
// immediately (even for failures) so the next run continues here, then
// mirror the loop-result event row.
func (d *driver) record(w io.Writer, loopNum int, status, goal string) {
	goalTxt := rowText(goal, 160)
	ts := rowTimestamp(d.cfg.Now)
	_ = appendJSONL(filepath.Join(d.stateDir, "ledger.jsonl"), ledgerRow{
		Loop:   loopNum,
		TS:     ts,
		Status: status,
		Goal:   goalTxt,
	})
	sync := newStateSync(d.cfg.Repo, w, d.cfg.Now)
	if err := sync.Push(); err != nil {
		fmt.Fprintln(w, "[state] push deferred")
	}
	_ = appendJSONL(eventsPath(d.cfg.Repo), loopResultEvent{
		TS:     rowTimestamp(d.cfg.Now),
		Kind:   "event",
		Event:  "loop-result",
		Loop:   loopNum,
		Status: status,
		Goal:   goalTxt,
	})
}

// phase ports the bash phase(): one loop-phase breadcrumb row per phase
// boundary.
func (d *driver) phase(loopNum int, name, detail string) {
	_ = appendJSONL(eventsPath(d.cfg.Repo), loopPhaseEvent{
		TS:     rowTimestamp(d.cfg.Now),
		Kind:   "event",
		Event:  "loop-phase",
		Loop:   loopNum,
		Phase:  name,
		Detail: rowText(detail, 120),
	})
}

var loopNumRe = regexp.MustCompile(`"loop":([0-9]+)`)

// nextLoopNumber ports the bash numbering: max("loop":N)+1 over the ledger;
// a missing or empty ledger yields 1. Line-count numbering collided after
// state-merge dedupe collapsed repeated numbers, hence max-key numbering.
func nextLoopNumber(ledgerPath string) int {
	data, err := os.ReadFile(ledgerPath)
	if err != nil {
		return 1
	}
	max := 0
	for _, m := range loopNumRe.FindAllStringSubmatch(string(data), -1) {
		n := 0
		fmt.Sscanf(m[1], "%d", &n)
		if n > max {
			max = n
		}
	}
	return max + 1
}
