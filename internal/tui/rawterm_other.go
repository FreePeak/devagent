//go:build !(darwin || linux)

package tui

import "errors"

// rawTerm is a no-op stub on platforms without the POSIX termios interface:
// the interactive loop refuses to start (the non-TTY one-shot path never
// reaches it), matching TS behavior where setRawMode throws.
type rawTerm struct{}

func rawProbe(fd int) (*rawTerm, bool) { return nil, false }

func (r *rawTerm) enterRaw() error { return errors.New("raw mode unsupported on this platform") }

func (r *rawTerm) restore() {}

func termSize(fd int) (rows, cols int) { return DefaultRows, DefaultColumns }
