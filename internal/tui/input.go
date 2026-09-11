package tui

import (
	"strconv"
	"unicode/utf8"
)

// Raw-stdin key decoding for the TUI (FR-TUI). Terminals deliver keys as byte
// sequences: plain characters, control bytes (Ctrl+C = 0x03, Enter = 0x0d) and
// CSI/SS3 escape sequences (arrows, PgUp/PgDn, Home/End). The v1 handler read
// only the first byte, so every arrow key leaked a stray ESC and closed the
// help overlay — this module consumes whole sequences instead.
//
// A chunk may hold several keys at once (fast j/k typing), and a sequence may
// be split across chunks (ESC and `[A` arriving separately). DecodeKeys
// returns `pending` for a trailing partial sequence so the caller can prepend
// it to the next chunk — and the caller must arm a short flush timer, or a
// lone Esc press would sit in pending forever and "Esc does nothing" until
// the next key arrives (2026-09-05 keybinding incident #1).
//
// Unknown CSI sequences are consumed whole and dropped: decoding them as
// ESC + leftover chars leaked keystrokes into the app (Delete `\x1b[3~`
// became Esc + '3' + '~' — Esc closed the overlay and '3' switched the view;
// incident #2). Port of src/tui/input.ts.

// KeyKind discriminates the decoded key union.
type KeyKind int

const (
	KeyChar KeyKind = iota
	KeyCtrl
	KeyEnter
	KeyEsc
	KeyTab
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyPgUp
	KeyPgDn
	KeyHome
	KeyEnd
	KeyDelete
	// KeyNewline is an explicit "insert a line break" key inside a
	// multi-line input (Alt+Enter / Ctrl+N). Plain Enter stays the submit
	// key, and \n stays Enter: several terminals deliver Enter as 0x0a, so
	// remapping \n would break Enter for them.
	KeyNewline
)

// Key is one decoded keypress. Ch is set only for KeyChar (a possibly
// multi-rune grapheme, as delivered by one UTF-8 chunk element) and KeyCtrl
// (the raw control byte).
type Key struct {
	Kind KeyKind
	Ch   string
}

// Key names for tests/logging, mirroring the TS kind strings.
func (k Key) String() string {
	switch k.Kind {
	case KeyChar:
		return "char:" + k.Ch
	case KeyCtrl:
		return "ctrl:" + strconv.Quote(k.Ch)
	case KeyEnter:
		return "enter"
	case KeyEsc:
		return "esc"
	case KeyTab:
		return "tab"
	case KeyUp:
		return "up"
	case KeyDown:
		return "down"
	case KeyLeft:
		return "left"
	case KeyRight:
		return "right"
	case KeyPgUp:
		return "pgup"
	case KeyPgDn:
		return "pgdn"
	case KeyHome:
		return "home"
	case KeyEnd:
		return "end"
	case KeyDelete:
		return "delete"
	case KeyNewline:
		return "newline"
	}
	return "unknown"
}

// seqMax is the runaway-sequence bound: past this many bytes without a final
// byte, give up.
const seqMax = 32

// DecodeResult carries the keys decoded from one chunk plus the trailing
// partial sequence ("" when the chunk ended on a key boundary).
type DecodeResult struct {
	Keys    []Key
	Pending string
}

// csiFinals: \x1b[<final> (letter finals, no parameters).
var csiFinals = map[byte]Key{
	'A': {Kind: KeyUp},
	'B': {Kind: KeyDown},
	'C': {Kind: KeyRight},
	'D': {Kind: KeyLeft},
	'H': {Kind: KeyHome},
	'F': {Kind: KeyEnd},
}

// csiTilde finals: \x1b[<n>~. Unknown n (F-keys, 2=Insert) → dropped.
var csiTilde = map[int]Key{
	1: {Kind: KeyHome},
	3: {Kind: KeyDelete},
	4: {Kind: KeyEnd},
	5: {Kind: KeyPgUp},
	6: {Kind: KeyPgDn},
	7: {Kind: KeyHome},
	8: {Kind: KeyEnd},
}

// csiKey resolves one CSI sequence: parameters + final byte → key, ok=false
// (drop silently) when unknown.
func csiKey(params string, final byte) (Key, bool) {
	if final == '~' {
		n, ok := parseIntPrefix(params)
		if !ok {
			return Key{}, false
		}
		k, ok := csiTilde[n]
		return k, ok
	}
	// Parameterized letter finals ("\x1b[1;5A" = Ctrl+Up) resolve by base final.
	k, ok := csiFinals[final]
	return k, ok
}

// parseIntPrefix mirrors JS Number.parseInt: optional sign then a leading
// decimal-digit prefix ("1;5" → 1); no digits → not finite (ok=false).
func parseIntPrefix(s string) (int, bool) {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == start {
		return 0, false
	}
	n, err := strconv.Atoi(s[:i])
	if err != nil {
		return 0, false
	}
	return n, true
}

// ss3Finals: \x1bO<final> (application-cursor-key mode).
var ss3Finals = map[byte]Key{
	'A': {Kind: KeyUp},
	'B': {Kind: KeyDown},
	'C': {Kind: KeyRight},
	'D': {Kind: KeyLeft},
	'H': {Kind: KeyHome},
	'F': {Kind: KeyEnd},
}

// DecodeKeys decodes a decoded-as-utf8 stdin chunk. Never panics. `Pending`
// is a trailing partial escape sequence that must be prepended to the next
// chunk; with flush (the caller's ESC-disambiguation timer fired) the partial
// is force-decoded to a single Esc press instead of being held.
func DecodeKeys(chunk string, flush bool) DecodeResult {
	keys := []Key{}
	s := chunk
	for s != "" {
		r, size := utf8.DecodeRuneInString(s)
		switch {
		case r == 0x1b:
			// A bare trailing ESC is ambiguous (Esc key vs a sequence the
			// terminal split across chunks): hold it as pending until the
			// caller flushes.
			if size == len(s) {
				// s == "\x1b" exactly.
				return finish(keys, s, flush)
			}
			c1 := s[size]
			if c1 == '[' {
				// CSI: parameter bytes (0x30–0x3f) then intermediates
				// (0x20–0x2f), terminated by one final byte (0x40–0x7e).
				// Scan for the final.
				i := size + 1
				for i < len(s) {
					c := s[i]
					if c < 0x20 || c > 0x3f {
						break
					}
					i++
				}
				if i >= len(s) {
					// Still mid-sequence: pending, unless it ran away (no
					// final ever).
					if len(s) > seqMax {
						keys = append(keys, Key{Kind: KeyEsc})
						s = s[size:]
						continue
					}
					return finish(keys, s, flush)
				}
				final := s[i]
				if final >= 0x40 && final <= 0x7e {
					if key, ok := csiKey(s[size+1:i], final); ok {
						keys = append(keys, key) // unknown sequence: drop whole, no junk keys
					}
					s = s[i+1:]
					continue
				}
				// Malformed (control byte inside the sequence): ESC, redecode the rest.
				keys = append(keys, Key{Kind: KeyEsc})
				s = s[size:]
				continue
			}
			if c1 == 'O' {
				if len(s) < size+2 {
					return finish(keys, s, flush) // SS3 is always 3 bytes; wait for the final
				}
				if key, ok := ss3Finals[s[size+1]]; ok {
					keys = append(keys, key)
				}
				s = s[size+2:]
				continue
			}
			// Alt+Enter (ESC CR, how terminals deliver Option+Enter with
			// meta-sends-escape): the explicit line-break key. Emitting
			// ESC + Enter instead would close the overlay and then submit
			// it — the opposite of what the operator asked for.
			if c1 == '\r' {
				keys = append(keys, Key{Kind: KeyNewline})
				s = s[size+1:]
				continue
			}
			// ESC + non-sequence char (e.g. alt-j): treat as ESC, redecode the char.
			keys = append(keys, Key{Kind: KeyEsc})
			s = s[size:]
			continue
		case r == '\r' || r == '\n':
			keys = append(keys, Key{Kind: KeyEnter})
			s = s[size:]
			continue
		case r == '\t':
			keys = append(keys, Key{Kind: KeyTab})
			s = s[size:]
			continue
		case r == 0x0e:
			// Ctrl+N: the layout-independent line-break key (Alt+Enter
			// needs meta-sends-escape configured; this one always works).
			keys = append(keys, Key{Kind: KeyNewline})
			s = s[size:]
			continue
		case r < ' ' || r == 0x7f:
			keys = append(keys, Key{Kind: KeyCtrl, Ch: string(r)})
			s = s[size:]
			continue
		default:
			// Plain (possibly multi-byte utf8) character.
			keys = append(keys, Key{Kind: KeyChar, Ch: s[:size]})
			s = s[size:]
			continue
		}
	}
	return DecodeResult{Keys: keys, Pending: ""}
}

// finish handles the only exit point that can hold a pending partial (an
// ESC-initiated prefix).
func finish(keys []Key, pending string, flush bool) DecodeResult {
	if pending != "" {
		if flush {
			keys = append(keys, Key{Kind: KeyEsc})
			return DecodeResult{Keys: keys, Pending: ""}
		}
		return DecodeResult{Keys: keys, Pending: pending}
	}
	return DecodeResult{Keys: keys, Pending: ""}
}
