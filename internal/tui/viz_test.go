package tui

import (
	"strings"
	"testing"
)

// Port of test/tui-viz.test.ts: visual primitives (pilot sparkline, htop
// meter) + log-line formatting + the incremental frame renderer + CJK width.

func TestSparklineEmptyAndFlat(t *testing.T) {
	if got := Sparkline(nil); got != "" {
		t.Fatalf("sparkline(nil) = %q, want \"\"", got)
	}
	if got := Sparkline([]float64{0, 0, 0}); got != "▁▁▁" {
		t.Fatalf("all-zero series = %q, want flat baseline", got)
	}
}

func TestSparklineScalesToMax(t *testing.T) {
	if got := Sparkline([]float64{0, 4}); got != "▁█" {
		t.Fatalf("sparkline([0,4]) = %q, want ▁█", got)
	}
	if got := Sparkline([]float64{1, 2, 3}); got != "▃▆█" {
		t.Fatalf("sparkline([1,2,3]) = %q, want ▃▆█", got)
	}
}

func TestMeterBarFillsProportionally(t *testing.T) {
	if got := MeterBar(5, 10, 10, "█", "░"); got != "█████░░░░░" {
		t.Fatalf("meterBar(5,10,10) = %q", got)
	}
}

func TestMeterBarDegenerate(t *testing.T) {
	if got := MeterBar(0, 0, 5, "█", "░"); got != "░░░░░" {
		t.Fatalf("zero total = %q", got)
	}
	if got := MeterBar(-1, 5, 5, "█", "░"); got != "░░░░░" {
		t.Fatalf("negative fill = %q", got)
	}
	if got := MeterBar(12, 6, 6, "█", "░"); got != "██████" {
		t.Fatalf("overfill clamps = %q", got)
	}
}

func TestParseLogLineRunsShape(t *testing.T) {
	l := ParseLogLine(`{"ts":"2026-09-05T10:00:00.000Z","level":"warn","stage":"clarify","runId":"r1","message":"m"}`)
	if l.Level != "warn" || l.Stage != "clarify" || l.RunID != "r1" || l.Message != "m" {
		t.Fatalf("parsed = %+v", l)
	}
	if l.Raw {
		t.Fatal("structured line must not be raw")
	}
}

func TestParseLogLineDegrades(t *testing.T) {
	if l := ParseLogLine("oops"); !l.Raw || l.Message != "oops" {
		t.Fatalf("non-JSON = %+v", l)
	}
	if l := ParseLogLine("{oops"); !l.Raw {
		t.Fatalf("malformed JSON = %+v", l)
	}
}

func TestParseLogLineOrchestrationRow(t *testing.T) {
	l := ParseLogLine(`{"kind":"event","event":"loop-phase","loop":82,"phase":"task","detail":"Ship it"}`)
	if l.Stage != "loop-phase" {
		t.Fatalf("stage = %q, want loop-phase", l.Stage)
	}
	if l.Message != "phase: task — Ship it — (loop 82)" {
		t.Fatalf("synthesized message = %q", l.Message)
	}
}

func TestFormatLogLineWidth(t *testing.T) {
	out := FormatLogLine(ParseLogLine(`{"ts":"2026-09-05T10:00:00.000Z","level":"warn","stage":"clarify","message":"Insufficient specification"}`), 100, Reset)
	if VisibleLen(out) > 100 {
		t.Fatalf("overflow: %q", out)
	}
	for _, want := range []string{"warn", "clarify", "Insufficient specification", "\x1b[33m"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
}

func TestFormatLogLineTruncates(t *testing.T) {
	out := FormatLogLine(ParseLogLine(`{"level":"info","message":"`+strings.Repeat("x", 200)+`"}`), 60, Reset)
	if VisibleLen(out) > 60 {
		t.Fatalf("overflow: %q", out)
	}
	if !strings.Contains(out, "…") {
		t.Fatal("missing ellipsis")
	}
}

func TestClampLineShort(t *testing.T) {
	if ClampLine("abc", 80) != "abc" {
		t.Fatal("short line altered")
	}
}

func TestClampLineANSI(t *testing.T) {
	line := "\x1b[31m" + strings.Repeat("a", 200) + "\x1b[0m"
	out := ClampLine(line, 80)
	if VisibleLen(out) > 80 {
		t.Fatalf("overflow: %q", out)
	}
	if !strings.HasPrefix(out, "\x1b[31m") {
		t.Fatal("lost leading SGR")
	}
	if !strings.Contains(out, "…") {
		t.Fatal("missing ellipsis")
	}
}

func TestRenderFrameFullPaint(t *testing.T) {
	seq := RenderFrame(nil, []string{"a", "b"}, 80)
	if !strings.HasPrefix(seq, "\x1b[H") {
		t.Fatal("missing cursor home")
	}
	if !strings.Contains(seq, "a\x1b[K") || !strings.Contains(seq, "b\x1b[K") {
		t.Fatal("missing per-line erase-to-EOL")
	}
	if strings.Contains(seq, "\x1b[2J") {
		t.Fatal("full repaint must not clear the screen")
	}
	if !strings.HasSuffix(seq, "\x1b[J") {
		t.Fatal("missing trailing erase-leftover")
	}
}

func TestRenderFrameIdentical(t *testing.T) {
	if got := RenderFrame([]string{"a"}, []string{"a"}, 80); got != "\x1b[H\x1b[1B" {
		t.Fatalf("identical frame = %q", got)
	}
}

func TestRenderFrameShrink(t *testing.T) {
	if seq := RenderFrame([]string{"a", "b", "c"}, []string{"a"}, 80); !strings.Contains(seq, "\x1b[J") {
		t.Fatal("shrinking frame must erase leftover rows")
	}
}

// TestRenderFrameShrinkErasesFromFirstLeftoverRow pins the erase-targeting
// fix: when the frame shrinks, leftover rows must be erased from the FIRST
// leftover row at column 1 via absolute positioning — never from wherever
// the row loop happened to leave the cursor (a skipped last row leaves it
// one row below the frame; a rewritten one, mid-row at the content column;
// an ED from either spot chops preserved rows or leaves stale cells).
func TestRenderFrameShrinkErasesFromFirstLeftoverRow(t *testing.T) {
	// Shrink with a SKIPPED last row (next[1] identical to prev[1]): the
	// cursor exits one row below the frame, so ED-from-cursor would target
	// the wrong row/column.
	seq := RenderFrame([]string{"a", "b", "c", "d"}, []string{"a", "b"}, 80)
	if !strings.Contains(seq, "\x1b[3;1H\x1b[J") {
		t.Fatalf("shrink must erase from row len(next)+1 col 1, got %q", seq)
	}
	// Full repaint (prev == nil) positions the erase the same way.
	full := RenderFrame(nil, []string{"one"}, 80)
	if !strings.Contains(full, "\x1b[2;1H\x1b[J") {
		t.Fatalf("full repaint must erase below the last row, got %q", full)
	}
}

func TestRenderFrameChangedMiddleRow(t *testing.T) {
	seq := RenderFrame([]string{"a", "b", "c"}, []string{"a", "X", "c"}, 80)
	if !strings.Contains(seq, "\x1b[1B") {
		t.Fatal("identical rows must be skipped")
	}
	if !strings.Contains(seq, "X\x1b[K") {
		t.Fatal("changed row must be rewritten")
	}
	if strings.Contains(seq, "a\x1b[K") {
		t.Fatal("unchanged row must not be rewritten")
	}
}

func TestRenderFrameClampsWideLines(t *testing.T) {
	seq := RenderFrame(nil, []string{strings.Repeat("x", 300)}, 80)
	row := seq[len("\x1b[H"):]
	if !strings.Contains(row, "…") {
		t.Fatal("missing clamp ellipsis")
	}
	content := strings.SplitN(row, "\x1b[K", 2)[0]
	content = strings.TrimPrefix(content, "\r")
	if VisibleLen(content) > 80 {
		t.Fatalf("row overflows: %d cells", VisibleLen(content))
	}
}

func TestWideCharWidth(t *testing.T) {
	if VisibleLen("中文") != 4 || VisibleLen("a中b") != 4 {
		t.Fatal("CJK must count as 2 cells")
	}
	if VisibleLen("ｆｕｌｌ") != 8 {
		t.Fatal("fullwidth latin must count as 2 cells")
	}
	if VisibleLen("👍") != 2 {
		t.Fatal("astral glyphs must count as 2 cells")
	}
	if VisibleLen("\x1b[31m中文\x1b[0m") != 4 {
		t.Fatal("SGR codes must not count")
	}
}

func TestClampLineCJK(t *testing.T) {
	out := ClampLine(strings.Repeat("目", 50), 20)
	if VisibleLen(out) > 20 {
		t.Fatalf("CJK clamp overflow: %d", VisibleLen(out))
	}
	if !strings.Contains(out, "…") {
		t.Fatal("missing ellipsis")
	}
}

func TestRenderFrameCJKRows(t *testing.T) {
	seq := RenderFrame(nil, []string{strings.Repeat("目标", 40)}, 30)
	content := seq[len("\x1b[H"):]
	content = content[:strings.Index(content, "\x1b[K")]
	content = strings.TrimPrefix(content, "\r")
	if VisibleLen(content) > 30 {
		t.Fatalf("CJK row overflow: %d", VisibleLen(content))
	}
}
