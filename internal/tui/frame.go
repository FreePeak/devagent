package tui

import (
	"strings"
	"unicode/utf8"
)

// Incremental frame renderer (the htop lesson): never clear the screen per
// refresh. The v1 loop wrote `\x1b[H\x1b[2J` + the whole frame every poll —
// a visible strobe on every 2s refresh and unusable for any faster animation.
//
// RenderFrame diffs two line arrays and emits the smallest escape sequence
// that turns the terminal's current content into `next`: cursor home, then
// per line either a skip (`\x1b[1B` cursor-down — identical lines) or a
// rewrite (content + `\x1b[K` erase-to-end-of-line), and `\x1b[J` to erase
// leftover rows when the frame shrank. Lines are clamped to `width` visible
// columns first so no line ever wraps (a wrapped line would desync the diff).
// Port of src/tui/frame.ts.

// sgrAt copies any leading ANSI SGR sequence at position i without burning
// width budget. Returns "" when no SGR sequence starts at i.
func sgrAt(line string, i int) string {
	if i+1 >= len(line) || line[i] != 0x1b || line[i+1] != '[' {
		return ""
	}
	j := i + 2
	for j < len(line) && (line[j] >= '0' && line[j] <= '9' || line[j] == ';') {
		j++
	}
	if j < len(line) && line[j] == 'm' {
		return line[i : j+1]
	}
	return ""
}

// ClampLine cuts a line to `width` visible columns, ANSI-aware, appending …
// on overflow.
func ClampLine(line string, width int) string {
	if VisibleLen(line) <= width {
		return line
	}
	var out strings.Builder
	vis := 0
	limit := width - 1
	if limit < 1 {
		limit = 1
	}
	for i := 0; i < len(line); {
		if sgr := sgrAt(line, i); sgr != "" {
			out.WriteString(sgr)
			i += len(sgr)
			continue
		}
		r, size := utf8.DecodeRuneInString(line[i:])
		w := CharCellWidth(r)
		if vis+w > limit {
			break
		}
		out.WriteRune(r)
		vis += w
		i += size
	}
	out.WriteString("…")
	out.WriteString(Reset)
	return out.String()
}

// RenderFrame returns the escape sequence transforming the screen from `prev`
// to `next`. A nil `prev` (or a width change the caller detected) means full
// repaint without `\x1b[2J` — rows not covered by `next` are still erased by
// the trailing `\x1b[J`.
func RenderFrame(prev, next []string, width int) string {
	lines := make([]string, len(next))
	for i, l := range next {
		lines[i] = ClampLine(l, width)
	}
	var out strings.Builder
	out.WriteString("\x1b[H")
	for row := 0; row < len(lines); row++ {
		if prev != nil && row < len(prev) && ClampLine(prev[row], width) == lines[row] {
			out.WriteString("\x1b[1B") // skip an identical row without touching it
			continue
		}
		// Rewriting the row: a leading \r guards against a wrapped earlier frame.
		out.WriteString("\r")
		out.WriteString(lines[row])
		out.WriteString("\x1b[K")
		if row < len(lines)-1 {
			out.WriteString("\n")
		}
	}
	if prev == nil || len(prev) > len(lines) {
		out.WriteString("\x1b[J") // erase leftover rows
	}
	return out.String()
}
