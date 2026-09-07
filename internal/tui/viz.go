package tui

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Visual primitives for the TUI, learned from the reference dashboards:
// pilot renders sparkline metric cards; htop renders proportional meter bars.
// Everything here is pure string math — no I/O — so layout bugs are unit
// testable (port of src/tui/viz.ts + test/tui-viz.test.ts).

// CharCellWidth returns the terminal cells one code point occupies (UAX #11
// East Asian Width, the range subset that matters here): CJK/kana/Hangul/
// fullwidth glyphs render 2 cells wide. Counting them as 1 made every width
// computation under-measure, so a line containing Chinese goal text rendered
// past its column budget, wrapped, and desynced the incremental frame diff.
func CharCellWidth(cp rune) int {
	if cp > 0xffff {
		return 2 // astral (emoji etc.) ≈ 2 cells
	}
	if (cp >= 0x1100 && cp <= 0x115f) || // Hangul Jamo
		(cp >= 0x2e80 && cp <= 0x303e) || // CJK radicals + symbols/punctuation
		(cp >= 0x3041 && cp <= 0x33ff) || // kana + CJK compatibility
		(cp >= 0x3400 && cp <= 0x4dbf) || // CJK extension A
		(cp >= 0x4e00 && cp <= 0x9fff) || // CJK unified ideographs
		(cp >= 0xa000 && cp <= 0xa4cf) || // Yi syllables
		(cp >= 0xac00 && cp <= 0xd7a3) || // Hangul syllables
		(cp >= 0xf900 && cp <= 0xfaff) || // CJK compatibility ideographs
		(cp >= 0xfe30 && cp <= 0xfe4f) || // CJK compatibility forms
		(cp >= 0xff00 && cp <= 0xff60) || // fullwidth forms
		(cp >= 0xffe0 && cp <= 0xffe6) { // fullwidth signs
		return 2
	}
	return 1
}

// VisibleLen returns the visible width of a string in terminal cells: ANSI
// SGR codes do not count.
func VisibleLen(s string) int {
	n := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			// SGR sequence \x1b[[0-9;]*m — skip it whole; an incomplete
			// sequence does NOT match the TS strip regex and counts as text.
			j := i + 2
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == ';') {
				j++
			}
			if j < len(s) && s[j] == 'm' {
				i = j + 1
				continue
			}
		}
		n += CharCellWidth(r)
		i += size
	}
	return n
}

// Sparkline maps samples onto half-block bars (pilot's metric-card cue).
// Scale is relative to the series' own max, so an all-zero series renders as
// a flat baseline rather than an empty string. "" only for no samples yet.
func Sparkline(samples []float64) string {
	return sparklineBlocks(samples, "▁▂▃▄▅▆▇█")
}

func sparklineBlocks(samples []float64, blocks string) string {
	if len(samples) == 0 {
		return ""
	}
	runes := []rune(blocks)
	n := len(runes)
	hi := samples[0]
	for _, v := range samples {
		if v > hi {
			hi = v
		}
	}
	lo := 0.0
	for _, v := range samples {
		if v < lo {
			lo = v
		}
	}
	span := hi - lo
	var b strings.Builder
	for _, v := range samples {
		frac := 0.0
		if span > 0 {
			frac = (v - lo) / span
		}
		idx := int(frac * float64(n))
		if idx > n-1 {
			idx = n - 1
		}
		b.WriteRune(runes[idx])
	}
	return b.String()
}

// MeterBar renders a proportional meter bar (htop's gauge cue): fill+empty
// glyphs padded to width. total <= 0 renders an empty bar — a negative gauge
// never lights up.
func MeterBar(filled, total float64, width int, fill, empty string) string {
	w := width
	if w < 0 {
		w = 0
	}
	ratio := 0.0
	if total > 0 {
		ratio = filled / total
		if ratio > 1 {
			ratio = 1
		}
		if ratio < 0 {
			ratio = 0
		}
	}
	n := int(mathRound(ratio * float64(w)))
	if n > w {
		n = w
	}
	return strings.Repeat(fill, n) + strings.Repeat(empty, w-n)
}

// mathRound mirrors JS Math.round (half towards +∞); math.Round is half away
// from zero, which differs for negatives only — unused here but kept exact.
func mathRound(v float64) float64 {
	if v < 0 {
		return -mathFloor(-v + 0.5)
	}
	return mathFloor(v + 0.5)
}

func mathFloor(v float64) float64 {
	i := float64(int64(v))
	if v < 0 && v != i {
		// int64() truncates toward zero; JS Math.floor rounds toward -∞.
		if i > v {
			i--
		}
	}
	return i
}

// LevelColors maps a log level to its SGR color (TS LEVEL_COLORS).
var LevelColors = map[string]string{
	"error": Red,
	"warn":  Yellow,
	"info":  Cyan,
	"debug": Dim,
}

// LogLine is one structured line of a run log (DEVAGENT_HOME/runs/*.jsonl
// shape). Empty optional fields mean absent, matching TS undefined.
type LogLine struct {
	Ts      string
	Level   string
	Stage   string
	RunID   string
	Message string
	// Raw is true when the raw line was not JSON and is shown verbatim.
	Raw bool
}

// ParseLogLine parses one SSE `data:` payload from the daemon's /events —
// either a DEVAGENT_HOME/runs/*.jsonl line ({ts,level,stage,message}) or a
// repo orchestration row ({kind:'event',event:'loop-phase',loop,phase,detail}).
// Malformed lines degrade to a raw entry instead of throwing — a corrupt line
// must never kill the tail view.
func ParseLogLine(data string) LogLine {
	text := strings.TrimSpace(data)
	if !strings.HasPrefix(text, "{") {
		return LogLine{Message: text, Raw: true}
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(text), &obj); err != nil || obj == nil {
		return LogLine{Message: text, Raw: true}
	}
	stage := jsonString(obj["stage"])
	if stage == "" {
		if ev, ok := obj["event"].(string); ok {
			stage = ev
		}
	}
	message := ""
	if m, ok := obj["message"].(string); ok && m != "" {
		message = m
	}
	if message == "" {
		// Orchestration rows carry no message: synthesize one from their parts.
		var bits []string
		if p, ok := obj["phase"].(string); ok {
			bits = append(bits, "phase: "+p)
		}
		if d, ok := obj["detail"].(string); ok && d != "" {
			bits = append(bits, d)
		}
		if loop, ok := obj["loop"].(float64); ok {
			bits = append(bits, fmt.Sprintf("(loop %s)", formatJSNumber(loop)))
		}
		message = strings.Join(bits, " — ")
	}
	return LogLine{
		Ts:      jsonString(obj["ts"]),
		Level:   strings.ToLower(jsonString(obj["level"])),
		Stage:   stage,
		RunID:   jsonString(obj["runId"]),
		Message: messageOrText(message, text),
	}
}

func jsonString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func messageOrText(message, text string) string {
	if message != "" {
		return message
	}
	return text
}

// formatJSNumber mirrors JS String(number) for the loop counter (integral
// float64 → no decimal point).
func formatJSNumber(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// logClock formats an ISO ts as local HH:MM:SS; 8-space placeholder when
// absent/unparseable.
func logClock(ts string) string {
	if ts == "" {
		return "        "
	}
	d, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return "        "
	}
	return d.Local().Format("15:04:05")
}

// FormatLogLine renders one log line to width visible columns: clock, level,
// stage, message.
func FormatLogLine(l LogLine, width int, reset string) string {
	color := ""
	if l.Level != "" {
		color = LevelColors[l.Level]
	}
	lvl := "evt"
	if l.Raw {
		lvl = "raw"
	} else if l.Level != "" {
		lvl = l.Level
	}
	if len(lvl) > 5 {
		lvl = lvl[:5]
	}
	for len(lvl) < 5 {
		lvl += " "
	}
	// rune-safe pad/truncate beyond 5 is unnecessary: level names are ASCII.
	clock := logClock(l.Ts)
	stage := ""
	if l.Stage != "" {
		stage = runeSlice(l.Stage, 0, 12) + "  "
	}
	head := " " + clock + " " + color + lvl + reset + " " + stage
	msgW := width - VisibleLen(head) - 1
	if msgW < 10 {
		msgW = 10
	}
	msg := l.Message
	if len(l.Message) > msgW {
		cut := msgW - 1
		if cut < 0 {
			cut = 0
		}
		msg = runeSlice(l.Message, 0, cut) + "…"
	}
	return head + msg
}

// runeSlice mirrors s.slice(a, b) over UTF-16 code units for the ASCII-range
// strings this package truncates; runes are the closest Go equivalent.
func runeSlice(s string, a, b int) string {
	r := []rune(s)
	if a < 0 {
		a = 0
	}
	if b > len(r) {
		b = len(r)
	}
	if a >= b {
		return ""
	}
	return string(r[a:b])
}
