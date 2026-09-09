package tui

import "strings"

// CloddsBot-style onboarding banner (the skin source is CloddsBot's
// onboard wizard: cyan wordmark art, bold product line + dim tagline, a
// dim `·`-separated stats row, then a dim rule). Mono mode keeps the art
// and drops the color, like every other surface in this package.

// bannerRows is a hand-set ANSI-Shadow-style wordmark for "DEVAGENT",
// 6 rows. Only the block glyphs ╗ ╔ ═ ║ ╝ ╚ ╣ ╦ ╩ ╬ appear, so the art is
// terminal-safe; trailing spaces are trimmed per row.
var bannerRows = []string{
	"██████╗ ███████╗██╗   ██╗ █████╗  ██████╗ ███████╗███╗   ██╗████████╗",
	"██╔══██╗██╔════╝╚██╗ ██╔╝██╔══██╗██╔════╝ ██╔════╝████╗  ██║╚══██╔══╝",
	"██║  ██║█████╗  ╚████╔╝ ███████║██║  ███╗█████╗  ██╔██╗ ██║   ██║   ",
	"██║  ██║██╔══╝   ╚██╔╝  ██╔══██║██║   ██║██╔══╝  ██║╚██╗██║   ██║   ",
	"██████╔╝███████╗   ██║   ██║  ██║╚██████╔╝███████╗██║ ╚████║   ██║   ",
	"╚═════╝ ╚══════╝   ╚═╝   ╚═╝  ╚═╝ ╚═════╝ ╚══════╝╚═╝  ╚═══╝   ╚═╝   ",
}

// OnboardBanner renders the init-wizard welcome block: the wordmark in the
// accent cyan, then the CloddsBot title composition — bold product name +
// dim tagline, dim stats row, dim rule.
func OnboardBanner(version string) []string {
	rows := make([]string, 0, len(bannerRows)+6)
	for _, r := range bannerRows {
		rows = append(rows, Cyan+strings.TrimRight(r, " ")+Reset)
	}
	rows = append(rows,
		"",
		"  "+Bold+"DevAgent"+Reset+" "+Dim+"— autonomous backend delivery agent"+Reset,
		"  "+Dim+"ticket → tested PR · git-first · self-hosted · v"+version+Reset,
		"  "+Dim+strings.Repeat("═", 56)+Reset,
		"",
	)
	return rows
}
