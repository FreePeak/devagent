package tui

import "testing"

// Port of test/tui-input.test.ts: whole-sequence key decoding contract.

func keysOf(res DecodeResult) []Key { return res.Keys }

func expectKeys(t *testing.T, chunk string, flush bool, want []Key) {
	t.Helper()
	got := DecodeKeys(chunk, flush)
	if len(got.Keys) != len(want) {
		t.Fatalf("decode(%q) keys = %v, want %v", chunk, got.Keys, want)
	}
	for i := range want {
		if got.Keys[i] != want[i] {
			t.Fatalf("decode(%q) key[%d] = %+v, want %+v", chunk, i, got.Keys[i], want[i])
		}
	}
}

func TestDecodeArrowsWholeSequences(t *testing.T) {
	expectKeys(t, "\x1b[A", false, []Key{{Kind: KeyUp}})
	expectKeys(t, "\x1b[B", false, []Key{{Kind: KeyDown}})
	expectKeys(t, "\x1b[C", false, []Key{{Kind: KeyRight}})
	expectKeys(t, "\x1b[D", false, []Key{{Kind: KeyLeft}})
	if p := DecodeKeys("\x1b[A", false).Pending; p != "" {
		t.Fatalf("pending after full sequence: %q", p)
	}
}

func TestDecodePartialSequenceHeld(t *testing.T) {
	a := DecodeKeys("\x1b", false)
	if len(a.Keys) != 0 || a.Pending != "\x1b" {
		t.Fatalf("bare ESC = %+v", a)
	}
	b := DecodeKeys(a.Pending+"[B", false)
	expectKeys(t, b.Keys[0:1][0].Ch, false, nil) // sanity: no panic path
	if len(b.Keys) != 1 || b.Keys[0].Kind != KeyDown || b.Pending != "" {
		t.Fatalf("continued chunk = %+v", b)
	}
	c := DecodeKeys("\x1b[", false)
	if c.Pending != "\x1b[" {
		t.Fatalf("ESC[ prefix must be held: %+v", c)
	}
}

func TestDecodePagingKeys(t *testing.T) {
	expectKeys(t, "\x1b[5~", false, []Key{{Kind: KeyPgUp}})
	expectKeys(t, "\x1b[6~", false, []Key{{Kind: KeyPgDn}})
	expectKeys(t, "\x1b[H", false, []Key{{Kind: KeyHome}})
	expectKeys(t, "\x1b[F", false, []Key{{Kind: KeyEnd}})
	expectKeys(t, "\x1bOH", false, []Key{{Kind: KeyHome}})
}

func TestDecodeSeveralKeysOneChunk(t *testing.T) {
	expectKeys(t, "kq", false, []Key{{Kind: KeyChar, Ch: "k"}, {Kind: KeyChar, Ch: "q"}})
	expectKeys(t, "\x1b[Ay", false, []Key{{Kind: KeyUp}, {Kind: KeyChar, Ch: "y"}})
}

func TestDecodeControlBytes(t *testing.T) {
	expectKeys(t, "\r", false, []Key{{Kind: KeyEnter}})
	expectKeys(t, "\n", false, []Key{{Kind: KeyEnter}})
	expectKeys(t, "\t", false, []Key{{Kind: KeyTab}})
	expectKeys(t, "\x03", false, []Key{{Kind: KeyCtrl, Ch: "\x03"}})
}

func TestDecodeUnknownEscapeDegrades(t *testing.T) {
	expectKeys(t, "\x1bZj", false, []Key{{Kind: KeyEsc}, {Kind: KeyChar, Ch: "Z"}, {Kind: KeyChar, Ch: "j"}})
}

func TestDecodeMultibyteChar(t *testing.T) {
	expectKeys(t, "ü", false, []Key{{Kind: KeyChar, Ch: "ü"}})
}

func TestDecodeSplitTildeSequence(t *testing.T) {
	// 2026-09-05 keybinding incident: a tilde sequence split across chunks
	// was decoded early and the trailing '~' leaked as a literal.
	a := DecodeKeys("\x1b[5", false)
	if len(a.Keys) != 0 || a.Pending != "\x1b[5" {
		t.Fatalf("split prefix = %+v", a)
	}
	b := DecodeKeys(a.Pending+"~", false)
	if len(b.Keys) != 1 || b.Keys[0].Kind != KeyPgUp || b.Pending != "" {
		t.Fatalf("completed tilde = %+v", b)
	}
}

func TestDecodeFlushMode(t *testing.T) {
	for _, chunk := range []string{"\x1b", "\x1b[", "\x1b[5", "\x1bO"} {
		got := DecodeKeys(chunk, true)
		if len(got.Keys) != 1 || got.Keys[0].Kind != KeyEsc {
			t.Fatalf("flush(%q) = %+v, want single esc", chunk, got)
		}
	}
	if DecodeKeys("\x1b", false).Pending != "\x1b" {
		t.Fatal("without flush the partial is still held")
	}
}

func TestDecodeUnknownTildeConsumedWhole(t *testing.T) {
	// Delete used to decode as Esc + '3' + '~'; Insert and F5 must not leak
	// home+'5'+'~' garbage either.
	expectKeys(t, "\x1b[3~", false, []Key{{Kind: KeyDelete}})
	expectKeys(t, "\x1b[2~", false, nil)
	expectKeys(t, "\x1b[15~", false, nil)
}

func TestDecodeUnknownCSIFinalsConsumedWhole(t *testing.T) {
	expectKeys(t, "\x1b[Zj", false, []Key{{Kind: KeyChar, Ch: "j"}})
}

func TestDecodeParameterizedArrows(t *testing.T) {
	expectKeys(t, "\x1b[1;5A", false, []Key{{Kind: KeyUp}})
}

func TestDecodeRunawaySequence(t *testing.T) {
	// Past SEQ_MAX without a final byte: emit ESC and redecode the rest.
	chunk := "\x1b[" + string(make([]byte, 40))
	for i := range chunk[2:] {
		chunk = chunk[:2+i] + string(rune(0x30)) + chunk[3+i:]
	}
	res := DecodeKeys(chunk, false)
	if len(res.Keys) == 0 || res.Keys[0].Kind != KeyEsc {
		t.Fatalf("runaway must degrade to ESC, got %+v", res)
	}
}
