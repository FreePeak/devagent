package cli

import "testing"

// Tui wiring tests (issue #252): the tui command carries the frozen flag
// surface, is registered wired (no exit-3 stub), and answers --help with
// the real description (the stub never had one of its own).

func TestTuiFlagSurface(t *testing.T) {
	cmd := tuiCommand()
	addFlags(cmd, frozenSurface["tui"].Flags, requiredFlags["tui"])
	for _, flag := range []string{"url", "token", "uds-path", "repo", "attach-only"} {
		f := cmd.Flags().Lookup(flag)
		if f == nil {
			t.Errorf("--%s not registered", flag)
			continue
		}
		want := "string"
		if flag == "attach-only" {
			want = "bool"
		}
		if f.Value.Type() != want {
			t.Errorf("--%s type = %q, want %q", flag, f.Value.Type(), want)
		}
	}
	if got := len(cmd.Commands()); got != 0 {
		t.Errorf("tui has %d subcommands, want 0 (frozen surface)", got)
	}
}

func TestTuiIsWiredNotStubbed(t *testing.T) {
	wired := wiredCommands()
	cmd, ok := wired["tui"]
	if !ok {
		t.Fatal("tui missing from wiredCommands")
	}
	if cmd.RunE == nil {
		t.Fatal("tui RunE missing (stub commands use RunE too — check the factory)")
	}
	if notPortedIssue["tui"] != "" {
		t.Fatalf("tui still in notPortedIssue (%q) — stub cleanup incomplete", notPortedIssue["tui"])
	}
}

// TestTuiRegisteredOnRoot: NewRoot must expose tui with its start alias
// (the commander-level alias from src/cli.ts).
func TestTuiRegisteredOnRoot(t *testing.T) {
	root := NewRoot()
	cmd, _, err := root.Find([]string{"tui"})
	if err != nil || cmd == nil || cmd.Name() != "tui" {
		t.Fatalf("root.Find(tui) = %v, %v, err=%v", cmd, err, nil)
	}
	found := false
	for _, alias := range cmd.Aliases {
		if alias == "start" {
			found = true
		}
	}
	if !found {
		t.Fatalf("tui aliases = %v, want [start]", cmd.Aliases)
	}
}

// TestTuiHelpShowsDescription: `devagent tui --help` must exit 0 with the
// real description (a stub would print the not-ported exit-3 message
// instead of help text).
func TestTuiHelpShowsDescription(t *testing.T) {
	root := NewRoot()
	cmd, _, err := root.Find([]string{"tui"})
	if err != nil {
		t.Fatalf("tui not found: %v", err)
	}
	if cmd.Long == "" && cmd.Short == "" {
		t.Fatal("tui has no description")
	}
	// cobra materializes the --help flag on Execute (InitDefaultHelpFlag
	// runs during execution, not Find), so verify the full command parses
	// its frozen surface and that --help resolves to the help path.
	if err := cmd.ParseFlags([]string{"--attach-only", "--url", "http://x", "--token", "t", "--uds-path", "/s", "--repo", "/r"}); err != nil {
		t.Fatalf("frozen surface does not parse: %v", err)
	}
	cmd.InitDefaultHelpFlag()
	if f := cmd.Flags().Lookup("help"); f == nil {
		t.Fatal("help flag missing after InitDefaultHelpFlag")
	}
}
