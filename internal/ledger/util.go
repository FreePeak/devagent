package ledger

import (
	"encoding/json"
	"strings"
	"time"
)

// NowISO mirrors `new Date().toISOString()`: UTC, millisecond precision,
// Z suffix (e.g. 2026-09-07T12:34:56.789Z).
func NowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// truncateUTF16 mirrors TS String.prototype.slice(0, n): n counts UTF-16 code
// units, not bytes or runes. A rune beyond the limit is dropped whole (the
// only divergence from V8, which can emit a lone surrogate — not representable
// in Go strings and never JSON-identical anyway).
func truncateUTF16(s string, n int) string {
	units := 0
	for i, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if units+w > n {
			return s[:i]
		}
		units += w
	}
	return s
}

// jsWhitespace reports whether r is in the JavaScript \s character class
// (RegExp \s and String#trim whitespace). Go's unicode.IsSpace differs on
// U+FEFF, which JS trims and Go does not.
func jsWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x0085, 0x00A0, 0x1680, 0x2028,
		0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// trimJS mirrors TS String.prototype.trim().
func trimJS(s string) string {
	return strings.TrimFunc(s, jsWhitespace)
}

// normalizeCriterion mirrors the clusterFailures grouping key:
// criterion.toLowerCase().replace(/\s+/g, ' ').trim().
func normalizeCriterion(s string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range strings.ToLower(s) {
		if jsWhitespace(r) {
			if !prevSpace {
				b.WriteByte(' ')
			}
			prevSpace = true
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return strings.TrimPrefix(strings.TrimSuffix(b.String(), " "), " ")
}

// jsonStr encodes one string the way JSON.stringify does (no HTML escaping),
// for hand-built JSON objects where key order matters.
func jsonStr(s string) string {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return `""`
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// jsonValue encodes an arbitrary value without HTML escaping, or "null" when
// the value cannot be marshaled (TS JSON.stringify throws there; the ledger
// best-effort path never propagates).
func jsonValue(v any) string {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "null"
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// strPtr / floatPtr build the optional-field pointers used by the record
// structs (TS `field?:` semantics: nil = key absent, non-nil = key present
// even when zero-valued).
func strPtr(s string) *string     { return &s }
func floatPtr(f float64) *float64 { return &f }
