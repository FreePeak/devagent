// Stale-worker reaper: Go port of src/resilience/reaper.ts.
//
// Best-effort stale process reaper. Scans `ps` for headless devagent
// worker processes older than a threshold and kills their whole process
// group. Kill decisions are always gated on the devagent headless command
// shape, so interactive operator sessions are never reap-eligible (the
// 2026-08-26 incident class).

package pipeline

import (
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/FreePeak/devagent/internal/spawn"
)

// workerPattern mirrors the TS WORKER_PATTERN.
var workerPattern = regexp.MustCompile(`(?i)\b(opencode|claude|omp|pi)(\s|$|--)`)

// ReapOptions mirrors the TS { cwdPrefix?: string } opts object.
type ReapOptions struct {
	// CWDPrefix restricts the scan to worker processes whose cwd starts
	// with this path (the repo's .devagent-worktrees). "" = no filter.
	CWDPrefix string
}

// StaleWorker mirrors the TS StaleWorker interface.
type StaleWorker struct {
	Pid       int    `json:"pid"`
	ElapsedMs int64  `json:"elapsedMs"`
	Command   string `json:"command"`
}

// reapProbes is the test seam for the probe funcs: tests inject fake
// ps/lsof output through these; nil fields fall back to the real
// spawn.RunCli probe.
var reapProbes = struct {
	// cmdline runs `ps -o command= -p <pid>` (2s timeout).
	cmdline func(pid int) string
	// ppid runs `ps -o ppid= -p <pid>` (2s timeout); false maps the TS
	// throw.
	ppid func(pid int) (int, bool)
	// cwd runs the lsof cwd probe for pid (2s timeout).
	cwd func(pid int) string
	// scan runs `ps -eo pid,etime,command` (3s timeout); false maps the TS
	// throw (caller returns no stale workers).
	scan func() (out string, ok bool)
}{}

// reapKillTree is the test seam for the kill step (tests record instead of
// signaling real processes).
var reapKillTree = killStaleProcessTree

func probeCmdline(pid int) string {
	if reapProbes.cmdline != nil {
		return reapProbes.cmdline(pid)
	}
	out := spawn.RunCli("ps", []string{"-o", "command=", "-p", strconv.Itoa(pid)},
		spawn.Options{Dir: "/", TimeoutMs: 2_000})
	return strings.TrimSpace(out.Stdout)
}

func probePpid(pid int) (int, bool) {
	if reapProbes.ppid != nil {
		return reapProbes.ppid(pid)
	}
	out := spawn.RunCli("ps", []string{"-o", "ppid=", "-p", strconv.Itoa(pid)},
		spawn.Options{Dir: "/", TimeoutMs: 2_000})
	n, err := strconv.Atoi(strings.TrimSpace(out.Stdout))
	if err != nil {
		return 0, false
	}
	return n, true
}

// OwnAncestryPids mirrors ownAncestryPids: PIDs of this process plus all
// its ancestors. Reaping must never kill itself, its parent shell, or the
// devagent/LaunchAgent chain that invoked it — otherwise a mid-run reap
// terminates the very pipeline it protects.
func OwnAncestryPids() map[int]bool {
	pids := map[int]bool{}
	pid := os.Getpid()
	for i := 0; i < 64 && pid > 1 && !pids[pid]; i++ {
		pids[pid] = true
		next, ok := probePpid(pid)
		if !ok || next <= 1 {
			break
		}
		pid = next
	}
	return pids
}

// isWorkerPid mirrors the TS isWorkerPid.
func isWorkerPid(pid int) bool {
	return workerPattern.MatchString(probeCmdline(pid))
}

// KillStaleProcessTree is the pinned export: kill a stale worker's whole
// process tree. Returns whether the pid was deemed a killable worker.
func KillStaleProcessTree(pid int) bool {
	return killStaleProcessTree(pid)
}

var digitsOnly = regexp.MustCompile(`^\d+$`)
var etimePattern = regexp.MustCompile(`^(?:(\d+)-)?(?:(\d+):)?(\d+):(\d+)$`)

// ParseEtimeToMs mirrors parseEtimeToMs: parse ps `etime` output
// ([[dd-]hh:]mm:ss or seconds) to milliseconds. Portable across Linux
// procps and macOS BSD ps; `etimes` (raw seconds column) is Linux-only and
// silently breaks the scan on macOS.
func ParseEtimeToMs(etime string) int64 {
	s := strings.TrimSpace(etime)
	if digitsOnly.MatchString(s) {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0
		}
		return n * 1000
	}
	m := etimePattern.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	days := int64(0)
	if m[1] != "" {
		days, _ = strconv.ParseInt(m[1], 10, 64)
	}
	hours := int64(0)
	if m[2] != "" {
		hours, _ = strconv.ParseInt(m[2], 10, 64)
	}
	minutes, _ := strconv.ParseInt(m[3], 10, 64)
	seconds, _ := strconv.ParseInt(m[4], 10, 64)
	return (((days*24+hours)*60+minutes)*60 + seconds) * 1000
}

// Reap-eligibility patterns mirror the TS consts: a process is only
// reap-eligible when it is provably a devagent-spawned worker: headless
// print-mode with JSON output. Interactive sessions (`claude` TUI in any
// project, or the user's bare `omp` TUIs) never match, so the reaper can
// never kill the user's live work (2026-08-26 incident: pattern-only
// matching reaped unrelated interactive claude processes machine-wide).
var (
	devagentWorkerCmd = regexp.MustCompile(`(^|\s)(--print|-p)(\s|$)`)
	devagentResumeCmd = regexp.MustCompile(`(^|\s)(--continue|-c)(\s|$)`)
	ompModeJSONCmd    = regexp.MustCompile(`(^|\s)--mode(\s|=)json(\s|$)`)
	piCmdPattern      = regexp.MustCompile(`(^|\b)pi(\s|$)`)
	opencodeClaudeRe  = regexp.MustCompile(`(?i)\b(opencode|claude)\b`)
	ompWordRe         = regexp.MustCompile(`\bomp\b`)
	outputFormatRe    = regexp.MustCompile(`--output-format\b`)
)

// IsDevagentWorkerCmd mirrors isDevagentWorkerCmd.
func IsDevagentWorkerCmd(cmd string) bool {
	if !workerPattern.MatchString(cmd) {
		return false
	}
	headless := devagentWorkerCmd.MatchString(cmd) || devagentResumeCmd.MatchString(cmd)
	if !headless {
		return false
	}
	// claude-code / opencode: --output-format json (unchanged)
	if opencodeClaudeRe.MatchString(cmd) {
		return outputFormatRe.MatchString(cmd)
	}
	// omp / pi: --mode json (adapters always emit `--mode json`; interactive
	// omp/pi have no flag and never match)
	if ompWordRe.MatchString(cmd) {
		return ompModeJSONCmd.MatchString(cmd)
	}
	if piCmdPattern.MatchString(cmd) {
		return ompModeJSONCmd.MatchString(cmd)
	}
	return false
}

// probeCwd mirrors the TS cwdFor: the lsof cwd probe (`-Fn` prints the
// path on the line starting with `n`).
func probeCwd(pid int) string {
	if reapProbes.cwd != nil {
		return reapProbes.cwd(pid)
	}
	out := spawn.RunCli("lsof", []string{"-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn"},
		spawn.Options{Dir: "/", TimeoutMs: 2_000})
	for _, l := range strings.Split(out.Stdout, "\n") {
		if strings.HasPrefix(l, "n") {
			return l[1:]
		}
	}
	return ""
}

// psLinePattern mirrors the TS /^(\d+)\s+(\S+)\s+(.*)$/ row matcher.
var psLinePattern = regexp.MustCompile(`^(\d+)\s+(\S+)\s+(.*)$`)

// defaultOlderThanMs mirrors the TS default threshold.
const defaultOlderThanMs = 10 * 60_000

// FindStaleWorkerPids mirrors findStaleWorkerPids. etime (not etimes): BSD
// ps on macOS rejects etimes, which made the whole scan throw and silently
// report "no stale workers" forever.
func FindStaleWorkerPids(olderThanMs int, opts *ReapOptions) []StaleWorker {
	if olderThanMs <= 0 {
		olderThanMs = defaultOlderThanMs
	}
	cwdPrefix := ""
	if opts != nil {
		cwdPrefix = opts.CWDPrefix
	}
	var raw string
	var ok bool
	if reapProbes.scan != nil {
		raw, ok = reapProbes.scan()
	} else {
		out := spawn.RunCli("ps", []string{"-eo", "pid,etime,command"},
			spawn.Options{Dir: "/", TimeoutMs: 3_000})
		raw, ok = out.Stdout, out.ExitCode == 0
	}
	if !ok {
		return []StaleWorker{}
	}
	lines := strings.Split(raw, "\n")
	if len(lines) > 0 {
		lines = lines[1:] // drop the header row like the TS slice(1)
	}
	own := OwnAncestryPids()
	stale := []StaleWorker{}
	for _, line := range lines {
		m := psLinePattern.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		pid, err := strconv.Atoi(m[1])
		if err != nil || own[pid] {
			continue
		}
		cmd := m[3]
		if !IsDevagentWorkerCmd(cmd) {
			continue
		}
		if cwdPrefix != "" && !strings.HasPrefix(probeCwd(pid), cwdPrefix) {
			continue
		}
		elapsedMs := ParseEtimeToMs(m[2])
		if elapsedMs >= int64(olderThanMs) {
			command := cmd
			if len(command) > 300 {
				command = command[:300]
			}
			stale = append(stale, StaleWorker{Pid: pid, ElapsedMs: elapsedMs, Command: command})
		}
	}
	return stale
}

// ReapStaleWorkers mirrors reapStaleWorkers: kill all stale workers older
// than the threshold. Returns the stale set (signaled when !dryRun).
func ReapStaleWorkers(olderThanMs int, dryRun bool, opts *ReapOptions) []StaleWorker {
	if olderThanMs <= 0 {
		olderThanMs = defaultOlderThanMs
	}
	stale := FindStaleWorkerPids(olderThanMs, opts)
	if dryRun {
		return stale
	}
	for _, s := range stale {
		reapKillTree(s.Pid)
	}
	return stale
}
