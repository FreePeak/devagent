package tui

import (
	"strings"
	"testing"
)

// Contract tests for the CloddsBot skin (2026-09 restyle): the onboarding
// banner, the centered cyan box titles, the outcome glyphs, the step chips,
// and the header tagline. The skin changes presentation only — state still
// reads through labels and glyphs, and mono mode strips every color.

func TestOnboardBannerShape(t *testing.T) {
	for _, mode := range []struct {
		name   string
		setMono func(bool) func()
	}{
		{"color", func(bool) func() { return func() {} }},
		{"mono", SetMono},
	} {
		t.Run(mode.name, func(t *testing.T) {
			if mode.name == "mono" {
				restore := mode.setMono(true)
				defer restore()
			}
			rows := OnboardBanner("1.2.3")
			joined := strings.Join(rows, "\n")
			if !strings.Contains(joined, "DevAgent") {
				t.Fatal("banner must carry the product name")
			}
			if !strings.Contains(joined, "v1.2.3") {
				t.Fatal("banner must carry the version")
			}
			if len(bannerRows) < 4 {
				t.Fatal("banner wordmark must be multi-row")
			}
			for _, r := range rows {
				if VisibleLen(r) > 76 {
					t.Fatalf("banner row too wide (%d): %q", VisibleLen(r), r)
				}
			}
		})
	}
}

func TestBoxLinesCentersTitle(t *testing.T) {
	out := plain(BoxLines("Queue", []string{" row one ", " row two "}, 50))
	if !strings.Contains(out, "╭") || !strings.Contains(out, "Queue") {
		t.Fatalf("box missing frame/title:\n%s", out)
	}
	lines := strings.Split(out, "\n")
	// The title must not start at the left gutter (the old ╭─ Title form).
	if strings.Contains(lines[0], "╭─ Queue") {
		t.Fatalf("title must be centered, not gutter-pinned:\n%s", lines[0])
	}
	// Visible width of every line must match.
	for _, l := range lines {
		if VisibleLen(l) != VisibleLen(lines[0]) {
			t.Fatalf("ragged box line (%d vs %d): %q", VisibleLen(l), VisibleLen(lines[0]), l)
		}
	}
}

func TestStatusGlyphOutcomes(t *testing.T) {
	if got := plain(StatusGlyph("ok")); got != "✓" {
		t.Fatalf("ok glyph = %q, want ✓", got)
	}
	if got := plain(StatusGlyph("failed")); got != "✗" {
		t.Fatalf("failed glyph = %q, want ✗", got)
	}
	if got := plain(StatusGlyph("warn")); got != "⚠" {
		t.Fatalf("warn glyph = %q, want ⚠", got)
	}
	if got := plain(StatusGlyph("unknown")); got != "●" {
		t.Fatalf("unknown glyph = %q, want ●", got)
	}
}

func TestStepChipShape(t *testing.T) {
	if got := plain(StepChip(3)); got != " 3 " {
		t.Fatalf("mono step chip = %q, want \" 3 \"", got)
	}
}

func TestHeaderTagline(t *testing.T) {
	snap := testSnapshot()
	out := plain(RenderDashboard(snap, RenderOptions{Width: 120, Rows: 24}))
	if !strings.Contains(out, "DevAgent · autonomous backend delivery agent") {
		t.Fatalf("header missing CloddsBot tagline:\n%s", out)
	}
	// Narrow terminals drop the tagline instead of clamping the bar.
	narrow := plain(RenderDashboard(snap, RenderOptions{Width: 40, Rows: 24}))
	first := strings.SplitN(narrow, "\n", 2)[0]
	if !strings.Contains(first, "DevAgent") || strings.Contains(first, "autonomous backend") {
		t.Fatalf("narrow header must keep name, drop tagline: %q", first)
	}
}
