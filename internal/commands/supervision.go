package commands

// Issue #321: resolve how `devagent-go loop` is supervised on this box.
// Nothing in-tree registers the loop with a supervisor (`make loop-start`
// is plain nohup), but the 2026-09-10 incident (#286 secondary) showed the
// real exposure is *external* supervisor config: a `restart=always` /
// `KeepAlive=true` unit turns every intentional exit-0 starvation halt
// into an instant hollow restart. This module scans the standard per-user
// (and system) supervisor unit directories for a unit that runs the loop
// and classifies its restart policy — strictly read-only, no launchctl /
// systemctl calls.

import (
	"os"
	"path/filepath"
	"strings"
)

// SupervisionMode is the resolved answer for `devagent-go loop`.
type SupervisionMode struct {
	Source string // "launchd" | "systemd" | "" (nothing registered)
	Unit   string // absolute path of the supervising unit, "" if none
	Policy string // "always" | "on-failure" | "none" (no restart policy)
	Detail string // human explanation
}

// DetectSupervision scans the supervisor unit directories under home (plus
// the system-level dirs) for a unit that runs `devagent-go loop`. First
// match wins — running the loop under two supervisors is already a
// misconfiguration the row reports via the first unit found.
func DetectSupervision(home string) SupervisionMode {
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return SupervisionMode{Detail: "home directory unresolvable (" + err.Error() + "): treated as unsupervised"}
		}
		home = h
	}
	dirs := []struct{ source, dir, ext string }{
		{"launchd", filepath.Join(home, "Library", "LaunchAgents"), ".plist"},
		{"launchd", "/Library/LaunchAgents", ".plist"},
		{"launchd", "/Library/LaunchDaemons", ".plist"},
		{"systemd", filepath.Join(home, ".config", "systemd", "user"), ".service"},
		{"systemd", "/etc/systemd/system", ".service"},
	}
	for _, d := range dirs {
		entries, err := os.ReadDir(d.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), d.ext) {
				continue
			}
			path := filepath.Join(d.dir, e.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var matches bool
			var policy string
			if d.ext == ".plist" {
				matches, policy = classifyLaunchdPlist(string(data))
			} else {
				matches, policy = classifySystemdUnit(string(data))
			}
			if matches {
				return SupervisionMode{Source: d.source, Unit: path, Policy: policy}
			}
		}
	}
	return SupervisionMode{}
}

// runsTheLoopArgs: a unit supervises the loop only when an argument whose
// base is the devagent binary (`devagent-go` / `devagent`) is immediately
// followed by a standalone `loop` argument. A bare "devagent"+"loop"
// substring match false-positives on sibling agents (com.devagent.builder
// runs build-loop.sh, com.devagent.orchestrator runs orchestrate-loop.sh —
// neither supervises the loop).
func runsTheLoopArgs(vals []string) bool {
	for i, v := range vals {
		base := filepath.Base(strings.TrimSpace(v))
		if (base == "devagent-go" || base == "devagent") && i+1 < len(vals) && strings.TrimSpace(vals[i+1]) == "loop" {
			return true
		}
	}
	return false
}

// plistStringValues extracts the text of every <string>…</string> element
// in document order (Program, ProgramArguments, other keys — harmless:
// adjacency of the binary to `loop` is what decides).
func plistStringValues(s string) []string {
	var vals []string
	for {
		open := strings.Index(s, "<string>")
		if open < 0 {
			return vals
		}
		s = s[open+len("<string>"):]
		closing := strings.Index(s, "</string>")
		if closing < 0 {
			return vals
		}
		vals = append(vals, s[:closing])
		s = s[closing:]
	}
}

// serviceExecStartValues returns the whitespace-split words of every
// ExecStart* line, joining systemd's trailing-backslash continuations.
func serviceExecStartValues(s string) []string {
	var words []string
	cont := ""
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if cont != "" {
			t = cont + " " + strings.TrimSuffix(t, "\\")
		} else if !strings.HasPrefix(t, "ExecStart") {
			continue
		}
		cont = ""
		if strings.HasSuffix(t, "\\") {
			cont = strings.TrimSuffix(t, "\\")
			continue
		}
		words = append(words, strings.Fields(t)...)
	}
	return words
}

// classifySystemdUnit inspects a systemd unit's Restart= policy for the
// loop. Policies that restart on the exit-0 intentional halt (always,
// on-success, unless-stopped) map to "always"; the failure-only family
// maps to "on-failure"; no Restart= (or Restart=no) maps to "none".
func classifySystemdUnit(s string) (matches bool, policy string) {
	if !runsTheLoopArgs(serviceExecStartValues(s)) {
		return false, ""
	}
	restart := ""
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(t, "Restart="); ok {
			restart = strings.TrimSpace(v)
		}
	}
	switch restart {
	case "on-failure", "on-abnormal", "on-abort", "on-watchdog":
		return true, "on-failure"
	case "", "no":
		return true, "none"
	default: // always, on-success, unless-stopped: the exit-0 halt restarts
		return true, "always"
	}
}

// classifyLaunchdPlist inspects a launchd plist's KeepAlive policy for the
// loop. KeepAlive semantics that matter for the exit-0-halt contract
// (loopdriver/run.go: intentional halts exit 0):
//   - <true/> or a condition dict WITHOUT SuccessfulExit=false → restarts
//     on the intentional halt too → "always";
//   - {SuccessfulExit=false} → restarts only on abnormal exit → "on-failure";
//   - no KeepAlive → launchd never restarts → "none".
//
// ponytail: string-level plist sniffing (a real plist parser needs a new
// dependency); the balanced-dict scan covers the two shapes launchd
// documents for KeepAlive — a nested dict with a stray "</dict>" inside a
// string value would misclassify; upgrade path is howett.net/plist.
func classifyLaunchdPlist(s string) (matches bool, policy string) {
	if !runsTheLoopArgs(plistStringValues(s)) {
		return false, ""
	}
	lower := strings.ToLower(s)
	idx := strings.Index(lower, "<key>keepalive</key>")
	if idx < 0 {
		return true, "none"
	}
	rest := strings.TrimSpace(s[idx+len("<key>keepalive</key>"):])
	switch {
	case strings.HasPrefix(rest, "<true/>"):
		return true, "always"
	case strings.HasPrefix(rest, "<false/>"):
		return true, "none"
	}
	dict := balancedDict(rest)
	if strings.Contains(dict, "SuccessfulExit") && strings.Contains(dict, "<false/>") {
		return true, "on-failure"
	}
	return true, "always"
}

// balancedDict returns the first top-level <dict>…</dict> body.
func balancedDict(s string) string {
	start := strings.Index(s, "<dict>")
	if start < 0 {
		return ""
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch {
		case strings.HasPrefix(s[i:], "<dict>"):
			depth++
			i += len("<dict>") - 1
		case strings.HasPrefix(s[i:], "</dict>"):
			depth--
			if depth == 0 {
				return s[start : i+len("</dict>")]
			}
			i += len("</dict>") - 1
		}
	}
	return s[start:] // unterminated: treat the tail as the dict body
}
