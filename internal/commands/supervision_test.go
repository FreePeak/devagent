package commands

// Issue #321: supervision-mode resolution — how `devagent-go loop` is
// supervised on this box. The doctor check and `devagent supervision` both
// consume DetectSupervision; these tests pin the classification contract:
// restart=always / KeepAlive=true units warn, on-failure units pass,
// nothing registered reports unsupervised, and sibling agents
// (build-loop.sh / orchestrate-loop.sh) never match.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeUnit writes a unit file into dir under home for a DetectSupervision scan.
func writeUnit(t *testing.T, home, rel, content string) string {
	t.Helper()
	path := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const alwaysPlist = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.example.loop</string>
	<key>ProgramArguments</key>
	<array>
		<string>/usr/local/bin/devagent-go</string>
		<string>loop</string>
	</array>
	<key>KeepAlive</key>
	<true/>
</dict>
</plist>`

const onFailurePlist = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.example.loop</string>
	<key>ProgramArguments</key>
	<array>
		<string>/usr/local/bin/devagent-go</string>
		<string>loop</string>
	</array>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
</dict>
</plist>`

// builderPlist mirrors launchagents/com.devagent.builder.plist: a sibling
// agent running build-loop.sh. "devagent" and "loop" both appear as
// substrings — the adjacency matcher must not count it.
const builderPlist = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.devagent.builder</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/sh</string>
		<string>/Users/x/work/harvey/freepeak/devagent/scripts/build-loop.sh</string>
	</array>
	<key>KeepAlive</key>
	<true/>
</dict>
</plist>`

const alwaysService = `[Unit]
Description=devagent selfbuild loop

[Service]
ExecStart=/usr/local/bin/devagent-go loop
Restart=always

[Install]
WantedBy=default.target
`

const onFailureService = `[Unit]
Description=devagent selfbuild loop

[Service]
ExecStart=/usr/local/bin/devagent-go loop
Restart=on-failure
`

// TestDetectSupervisionClassifications: every policy verdict — no unit,
// KeepAlive=true, KeepAlive={SuccessfulExit=false}, no KeepAlive, systemd
// always, systemd on-failure — resolved from fixture unit files.
func TestDetectSupervisionClassifications(t *testing.T) {
	cases := []struct {
		name   string
		unit   string // relative path under home, "" for none
		body   string
		source string
		policy string
	}{
		{"none registered", "", "", "", ""},
		{"launchd KeepAlive=true", "Library/LaunchAgents/com.example.loop.plist", alwaysPlist, "launchd", "always"},
		{"launchd KeepAlive SuccessfulExit=false", "Library/LaunchAgents/com.example.loop.plist", onFailurePlist, "launchd", "on-failure"},
		{"launchd no KeepAlive", "Library/LaunchAgents/com.example.loop.plist", noKeepAlivePlist(), "launchd", "none"},
		{"systemd Restart=always", ".config/systemd/user/devagent-loop.service", alwaysService, "systemd", "always"},
		{"systemd Restart=on-failure", ".config/systemd/user/devagent-loop.service", onFailureService, "systemd", "on-failure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.unit != "" {
				writeUnit(t, home, tc.unit, tc.body)
			}
			m := DetectSupervision(home)
			if m.Source != tc.source || m.Policy != tc.policy {
				t.Fatalf("DetectSupervision = {source %q, unit %q, policy %q}, want {source %q, policy %q}", m.Source, m.Unit, m.Policy, tc.source, tc.policy)
			}
			if tc.unit != "" && m.Unit != filepath.Join(home, tc.unit) {
				t.Fatalf("unit path = %q, want %q", m.Unit, filepath.Join(home, tc.unit))
			}
		})
	}
}

// TestDoctorSupervisionEndToEndWithFixtureUnit: the #321 acceptance read
// literally — doctor run with the REAL default resolution (no seam), a
// fixture unit file using restart=always on disk under a redirected HOME,
// and the row must flip to warn naming the unit path. No live supervisor.
func TestDoctorSupervisionEndToEndWithFixtureUnit(t *testing.T) {
	home := t.TempDir()
	writeUnit(t, home, "Library/LaunchAgents/com.example.loop.plist", alwaysPlist)
	f := newDoctorFixture(t)
	f.stampVersion("1.2.3")
	f.opts.Supervision = nil // exercise the real default path
	t.Setenv("HOME", home)   // os.UserHomeDir reads $HOME on unix
	res := f.run()
	c := checkByName(t, res, "supervision")
	if !c.OK || !c.Warn {
		t.Fatalf("fixture restart=always unit must warn-pass, got ok=%v warn=%v (detail %q)", c.OK, c.Warn, c.Detail)
	}
	unit := filepath.Join(home, "Library/LaunchAgents/com.example.loop.plist")
	if !strings.Contains(c.Detail, unit) {
		t.Errorf("detail %q must name the unit path %q", c.Detail, unit)
	}
}

func noKeepAlivePlist() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.example.loop</string>
	<key>ProgramArguments</key>
	<array>
		<string>/usr/local/bin/devagent-go</string>
		<string>loop</string>
	</array>
</dict>
</plist>`
}

// TestDetectSupervisionIgnoresSiblingAgents: com.devagent.builder /
// com.devagent.orchestrator plists run *-loop.sh — both contain "devagent"
// and "loop" as substrings, but neither supervises `devagent-go loop`, so
// the box must still resolve unsupervised even with a KeepAlive=true
// sibling registered.
func TestDetectSupervisionIgnoresSiblingAgents(t *testing.T) {
	home := t.TempDir()
	writeUnit(t, home, "Library/LaunchAgents/com.devagent.builder.plist", builderPlist)
	orchestrator := orchestratorPlist()
	writeUnit(t, home, "Library/LaunchAgents/com.devagent.orchestrator.plist", orchestrator)
	m := DetectSupervision(home)
	if m.Unit != "" {
		t.Fatalf("sibling agent plists must not match, got unit %q (policy %q)", m.Unit, m.Policy)
	}
}

func orchestratorPlist() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.devagent.orchestrator</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/sh</string>
		<string>/Users/x/work/harvey/freepeak/devagent/scripts/orchestrate-loop.sh</string>
		<string>--loop</string>
	</array>
	<key>KeepAlive</key>
	<true/>
</dict>
</plist>`
}

// TestDoctorSupervisionRowVerdicts: the #321 acceptance — a fixture unit
// using restart=always flips the doctor verdict (warn, naming the unit),
// on-failure passes, and unsupervised passes labeled nohup.
func TestDoctorSupervisionRowVerdicts(t *testing.T) {
	cases := []struct {
		name     string
		mode     SupervisionMode
		wantOK   bool
		wantWarn bool
		wantIn   []string
	}{
		{"restart=always warns", SupervisionMode{Source: "systemd", Unit: "/etc/systemd/system/devagent-loop.service", Policy: "always"}, true, true,
			[]string{"/etc/systemd/system/devagent-loop.service", "hollow restart"}},
		{"KeepAlive=true warns", SupervisionMode{Source: "launchd", Unit: "/Users/x/Library/LaunchAgents/com.example.loop.plist", Policy: "always"}, true, true,
			[]string{"/Users/x/Library/LaunchAgents/com.example.loop.plist", "hollow restart"}},
		{"on-failure passes", SupervisionMode{Source: "systemd", Unit: "/etc/systemd/system/devagent-loop.service", Policy: "on-failure"}, true, false,
			[]string{"/etc/systemd/system/devagent-loop.service", "restarts on failure only"}},
		{"unsupervised passes", SupervisionMode{}, true, false,
			[]string{"unsupervised (nohup)"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDoctorFixture(t)
			f.stampVersion("1.2.3")
			f.opts.Supervision = func() SupervisionMode { return tc.mode }
			res := f.run()
			c := checkByName(t, res, "supervision")
			if c.OK != tc.wantOK || c.Warn != tc.wantWarn {
				t.Fatalf("row = ok %v warn %v (detail %q), want ok %v warn %v", c.OK, c.Warn, c.Detail, tc.wantOK, tc.wantWarn)
			}
			for _, s := range tc.wantIn {
				if !strings.Contains(c.Detail, s) {
					t.Errorf("detail %q missing %q", c.Detail, s)
				}
			}
		})
	}
	// The always unit must not fail the box on its own (warn row), but the
	// hint names the fix.
	f := newDoctorFixture(t)
	f.stampVersion("1.2.3")
	f.opts.Supervision = func() SupervisionMode {
		return SupervisionMode{Source: "systemd", Unit: "/etc/systemd/system/devagent-loop.service", Policy: "always"}
	}
	res := f.run()
	if !res.OK {
		t.Fatalf("warn row must not fail the box: %+v", res)
	}
	if h := checkByName(t, res, "supervision").Hint; h == "" {
		t.Fatal("restart=always row must carry a remediation hint")
	}
}
