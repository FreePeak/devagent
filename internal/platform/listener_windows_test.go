//go:build windows

package platform

import (
	"errors"
	"testing"
)

// Windows-side unit tests: the pipe path normalization and the documented
// ErrNotImplemented stubs. These compile only under GOOS=windows; the
// GOOS=windows CI build step vet-compiles the whole package including this
// file (go vet refuses build-constrained files that do not type-check).

func TestPipePath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", `\\.\pipe\devagent`},
		{"devagent", `\\.\pipe\devagent`},
		{`\\.\pipe\mine`, `\\.\pipe\mine`},
		{`\\.\PIPE\mine`, `\\.\PIPE\mine`}, // verbatim, prefix matched case-insensitively
		{`C:\run\devagent.sock`, `\\.\pipe\devagent`},
		{"/run/devagent.sock", `\\.\pipe\devagent`},
		{"session.sock", `\\.\pipe\session`},
	}
	for _, c := range cases {
		if got := pipePath(c.in); got != c.want {
			t.Errorf("pipePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestListenNotImplemented(t *testing.T) {
	_, err := Listen("devagent")
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Listen error = %v, want ErrNotImplemented", err)
	}
}

func TestDialNotImplemented(t *testing.T) {
	_, err := DialContext("devagent")(nil, "", "")
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("DialContext error = %v, want ErrNotImplemented", err)
	}
}
