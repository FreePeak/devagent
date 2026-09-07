//go:build !(darwin || linux)

// term_other.go mirrors the TS terminal-width read for card sizing
// (`process.stdout?.columns ?? 100`, src/cli.ts) on platforms without the
// POSIX TIOCGWINSZ ioctl: stdout.columns stays undefined off-TTY everywhere,
// so the 100 fallback (also tui.DefaultColumns) is the only value.

package cli

// terminalColumns reports the fallback card width on non-POSIX platforms,
// matching the TS `?? 100` when stdout.columns is undefined.
func terminalColumns() int {
	return 100
}
