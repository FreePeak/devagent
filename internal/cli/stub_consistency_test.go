package cli

import "testing"

// stubAllowlist records stubs that may legitimately remain in notPortedIssue
// even though their command is wired — wired-but-still-stubbed exceptions.
// It is empty today: tui is the last stub and its wiring lands via #252,
// which removes the notPortedIssue entry outright. Any row added here must
// still exist in notPortedIssue (checked below); prune the row when the
// stub goes away.
var stubAllowlist = map[string]string{}

// TestNotPortedIssueConsistent guards the stub map against the #251 drift
// class: a command that is wired in wiredCommands() must not linger in
// notPortedIssue — running it would exit 3 "not yet ported" instead of
// dispatching the real action (pane-run sat wired-but-stubbed this way,
// even though the exit-3 path is unreachable for handled commands).
// The allowlist is two-way: entries must exist in notPortedIssue, so pruning
// a stub also prunes its allowlist row.
func TestNotPortedIssueConsistent(t *testing.T) {
	wired := wiredCommands()
	for dotted := range notPortedIssue {
		if _, isWired := wired[dotted]; !isWired {
			continue
		}
		if _, allowed := stubAllowlist[dotted]; allowed {
			continue
		}
		t.Errorf("notPortedIssue carries %q but the command is wired — remove the stale entry (devagent %s would exit 3 instead of running)", dotted, dotted)
	}
	for dotted := range stubAllowlist {
		if _, stubbed := notPortedIssue[dotted]; !stubbed {
			t.Errorf("stubAllowlist carries %q but it is no longer stubbed in notPortedIssue — prune the allowlist entry", dotted)
		}
	}
}
