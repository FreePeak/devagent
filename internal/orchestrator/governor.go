// Package file mirrors src/orchestrator/governor.ts (FR-GO-07, issue #194).

package orchestrator

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Resource governor — sizes the worker pool from live OS signals.
// Pure decisions over an OsSnapshot; the only impurity is the cached OS
// read, injected here so tests stay deterministic (fake clock + fake
// snapshots + scripted `ps` output).

// OsSnapshot mirrors the TS OsSnapshot interface.
type OsSnapshot struct {
	TotalMem int64     `json:"totalMem"`
	FreeMem  int64     `json:"freeMem"`
	Cpus     int       `json:"cpus"`
	LoadAvg  []float64 `json:"loadAvg,omitempty"`
}

// GovernorOptions mirrors the TS GovernorOptions interface; pointer fields
// mean "unset → default".
type GovernorOptions struct {
	// Safety headroom ratio (0..1), default 0.7.
	SafetyRatio *float64
	// Estimated mem per worker in bytes, default 1 GiB.
	EstMemPerWorkerBytes *int64
	// CPU factor for cpu-bound cap, default 1.0.
	CPUFactor *float64
	// Floor for estimated mem (never calibrate below), default 256 MiB.
	MinEstFloorBytes *int64
	// Timeout waiting for pressure to lift, default 60s.
	PressureWaitTimeoutMs *int
	// Cache TTL for OS reads in ms, default 1000.
	CacheTtlMs *int
}

const (
	defaultEst        = int64(1) * 1024 * 1024 * 1024 // 1 GiB
	defaultMinFloor   = int64(256) * 1024 * 1024
	defaultSafety     = 0.7
	defaultCPUFactor  = 1.0
	defaultCacheTTL   = 1000
	defaultPressureMs = 60_000
)

// percentile mirrors the TS percentile: nearest-rank (ceil), clamped.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil((p/100)*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx > len(sorted)-1 {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// CommandRunner shells out to platform helpers (ps, vm_stat, sysctl) for
// live OS samples; tests script fixture output. Default: os/exec with a
// short timeout; any error means "signal unavailable" (TS execFile
// rejects on non-zero exit and callers swallow it the same way).
type CommandRunner = func(name string, args []string, timeoutMs int) (string, error)

func defaultCommandRunner(name string, args []string, timeoutMs int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// ResourceGovernor mirrors the TS ResourceGovernor class: a pure state
// machine over OsSnapshot values plus RSS calibration. The clock and the OS
// reader are injectable seams (TS tests fake timers; Go tests use
// SetClock/SetOsReader/SetCommandRunner).
type ResourceGovernor struct {
	safetyRatio           float64
	estMemPerWorkerBytes  float64
	cpuFactor             float64
	minEstFloorBytes      float64
	pressureWaitTimeoutMs int
	cacheTtlMs            int

	// RSS calibration
	observedRss []float64
	// OS cache
	cachedSnapshot *OsSnapshot
	cachedAt       int64

	// Seams (test injection): clock + OS reader + platform helper runner.
	nowMs       func() int64
	osReader    func() OsSnapshot
	commandRuns CommandRunner
}

// NewResourceGovernor mirrors the TS constructor.
func NewResourceGovernor(opts *GovernorOptions) *ResourceGovernor {
	g := &ResourceGovernor{
		safetyRatio:           defaultSafety,
		estMemPerWorkerBytes:  float64(defaultEst),
		cpuFactor:             defaultCPUFactor,
		minEstFloorBytes:      float64(defaultMinFloor),
		pressureWaitTimeoutMs: defaultPressureMs,
		cacheTtlMs:            defaultCacheTTL,
	}
	if opts != nil {
		if opts.SafetyRatio != nil {
			g.safetyRatio = *opts.SafetyRatio
		}
		if opts.EstMemPerWorkerBytes != nil {
			g.estMemPerWorkerBytes = float64(*opts.EstMemPerWorkerBytes)
		}
		if opts.CPUFactor != nil {
			g.cpuFactor = *opts.CPUFactor
		}
		if opts.MinEstFloorBytes != nil {
			g.minEstFloorBytes = float64(*opts.MinEstFloorBytes)
		}
		if opts.PressureWaitTimeoutMs != nil {
			g.pressureWaitTimeoutMs = *opts.PressureWaitTimeoutMs
		}
		if opts.CacheTtlMs != nil {
			g.cacheTtlMs = *opts.CacheTtlMs
		}
	}
	g.nowMs = func() int64 { return time.Now().UnixMilli() }
	g.commandRuns = defaultCommandRunner
	g.osReader = func() OsSnapshot { return defaultOsSnapshot(g.commandRuns) }
	return g
}

// SetClock injects a fake clock (Date.now equivalent) — for tests.
func (g *ResourceGovernor) SetClock(now func() int64) { g.nowMs = now }

// SetOsReader injects a fake OS snapshot source — for tests (the TS tests
// fake os.totalmem/freemem/cpus via module mocks).
func (g *ResourceGovernor) SetOsReader(reader func() OsSnapshot) { g.osReader = reader }

// SetCommandRunner replaces the platform helper runner (ps/vm_stat/sysctl)
// — for tests. The OS reader closure re-binds to it.
func (g *ResourceGovernor) SetCommandRunner(run CommandRunner) {
	if run == nil {
		run = defaultCommandRunner
	}
	g.commandRuns = run
	g.osReader = func() OsSnapshot { return defaultOsSnapshot(g.commandRuns) }
}

// RecordRss mirrors recordRss: record an observed worker RSS (bytes) and
// recalibrate p75.
func (g *ResourceGovernor) RecordRss(rssBytes float64) {
	if math.IsNaN(rssBytes) || math.IsInf(rssBytes, 0) || rssBytes <= 0 {
		return
	}
	g.observedRss = append(g.observedRss, rssBytes)
	// keep bounded history (last 100)
	if len(g.observedRss) > 100 {
		g.observedRss = g.observedRss[1:]
	}
	sorted := append([]float64(nil), g.observedRss...)
	sort.Float64s(sorted)
	p75 := percentile(sorted, 75)
	// calibration can only raise the estimate, never lower below configured floor
	if p75 > g.estMemPerWorkerBytes {
		g.estMemPerWorkerBytes = p75
	}
	if g.estMemPerWorkerBytes < g.minEstFloorBytes {
		g.estMemPerWorkerBytes = g.minEstFloorBytes
	}
}

// GetEstMemPerWorker mirrors getEstMemPerWorker.
func (g *ResourceGovernor) GetEstMemPerWorker() int64 { return int64(g.estMemPerWorkerBytes) }

// GetObservedCount mirrors getObservedCount.
func (g *ResourceGovernor) GetObservedCount() int { return len(g.observedRss) }

// ResetCalibration mirrors resetCalibration: clear calibration history (for
// tests).
func (g *ResourceGovernor) ResetCalibration() { g.observedRss = nil }

// EffectiveConcurrency mirrors effectiveConcurrency: compute effective
// concurrency from an explicit OS snapshot. configuredConcurrency follows
// the TS number semantics (non-finite → 1).
func (g *ResourceGovernor) EffectiveConcurrency(snapshot OsSnapshot, configuredConcurrency float64) int {
	if math.IsNaN(configuredConcurrency) || math.IsInf(configuredConcurrency, 0) || configuredConcurrency < 1 {
		configuredConcurrency = 1
	}
	avail := snapshot.FreeMem
	cpus := snapshot.Cpus
	if cpus == 0 {
		cpus = 1
	}
	memCap := int(math.Floor((float64(avail) * g.safetyRatio) / g.estMemPerWorkerBytes))
	cpuCap := int(math.Ceil(float64(cpus) * g.cpuFactor))
	raw := memCap
	if cpuCap < raw {
		raw = cpuCap
	}
	// clamp to [1, configured]
	clamped := raw
	if int(configuredConcurrency) < clamped {
		clamped = int(configuredConcurrency)
	}
	if clamped < 1 {
		clamped = 1
	}
	return clamped
}

// EffectiveAuto mirrors effectiveAuto: for 'auto' mode — no explicit
// ceiling, just mem/cpu bound, at least 1.
func (g *ResourceGovernor) EffectiveAuto(snapshot OsSnapshot) int {
	avail := snapshot.FreeMem
	cpus := snapshot.Cpus
	if cpus == 0 {
		cpus = 1
	}
	memCap := int(math.Floor((float64(avail) * g.safetyRatio) / g.estMemPerWorkerBytes))
	cpuCap := int(math.Ceil(float64(cpus) * g.cpuFactor))
	raw := memCap
	if cpuCap < raw {
		raw = cpuCap
	}
	if raw < 1 {
		return 1
	}
	return raw
}

// ResolveConcurrency mirrors resolveConcurrency: resolve a parsed
// concurrency input ('auto' or number) against live OS. Unparseable raw
// strings were already folded to 2 by ParseConcurrencyInput.
func (g *ResourceGovernor) ResolveConcurrency(input ConcurrencyValue) int {
	if input.Auto {
		return g.EffectiveAuto(*g.GetSnapshotSync())
	}
	return maxInt(1, input.Value)
}

// GetSnapshotSync mirrors getSnapshotSync: synchronous snapshot (cached).
// Returns the cached pointer so identity semantics match the TS object
// reference (tests assert cache identity).
func (g *ResourceGovernor) GetSnapshotSync() *OsSnapshot {
	now := g.nowMs()
	if g.cachedSnapshot != nil && now-g.cachedAt < int64(g.cacheTtlMs) {
		return g.cachedSnapshot
	}
	snap := g.osReader()
	g.cachedSnapshot = &snap
	g.cachedAt = now
	return g.cachedSnapshot
}

// RefreshSnapshot mirrors refreshSnapshot: force refresh (bypass cache).
func (g *ResourceGovernor) RefreshSnapshot() *OsSnapshot {
	snap := g.osReader()
	g.cachedSnapshot = &snap
	g.cachedAt = g.nowMs()
	return g.cachedSnapshot
}

// FormatStatus mirrors formatStatus: human-readable governor line for
// `devagent status`. A nil snapshot falls back to the cached/live read;
// the sample-age segment is only meaningful for the governor's own cached
// snapshot (TS reference identity).
func (g *ResourceGovernor) FormatStatus(concurrencyInput ConcurrencyValue, effective int, snapshot *OsSnapshot) string {
	snap := snapshot
	if snap == nil {
		snap = g.GetSnapshotSync()
	}
	freeGb := strconv.FormatFloat(float64(snap.FreeMem)/(1024*1024*1024), 'f', 1, 64)
	totalGb := strconv.FormatFloat(float64(snap.TotalMem)/(1024*1024*1024), 'f', 1, 64)
	estGb := strconv.FormatFloat(g.estMemPerWorkerBytes/(1024*1024*1024), 'f', 1, 64)
	// Sample freshness: only meaningful for the governor's own cached snapshot.
	ageStr := "n/a"
	if g.cachedSnapshot != nil && snap == g.cachedSnapshot {
		ageStr = fmt.Sprintf("%ss", strconv.FormatFloat(float64(g.nowMs()-g.cachedAt)/1000, 'f', 1, 64))
	}
	// Calibration state: aggregate count only, never per-pid RSS.
	cal := g.GetObservedCount()
	tail := fmt.Sprintf("sample %s, cal %d", ageStr, cal)
	if concurrencyInput.Auto {
		return fmt.Sprintf("workers auto->%d (mem %s GB free of %s, est %s GB/worker, cpus %d, %s)",
			effective, freeGb, totalGb, estGb, snap.Cpus, tail)
	}
	return fmt.Sprintf("workers %d/%d (mem %s GB free of %s, est %s GB/worker, cpus %d, %s)",
		effective, concurrencyInput.Value, freeGb, totalGb, estGb, snap.Cpus, tail)
}

// InjectSnapshot mirrors injectSnapshot: for tests — inject a fake snapshot
// and bypass cache. The pointer is stored as-is so the TS reference-identity
// check in FormatStatus behaves identically.
func (g *ResourceGovernor) InjectSnapshot(snap *OsSnapshot) {
	g.cachedSnapshot = snap
	g.cachedAt = g.nowMs()
}

// ClearCache mirrors clearCache: clear OS cache (for tests).
func (g *ResourceGovernor) ClearCache() {
	g.cachedSnapshot = nil
	g.cachedAt = 0
}

// vmRssRe mirrors the TS /VmRSS:\s+(\d+)\s+kB/ probe.
var vmRssRe = regexp.MustCompile(`VmRSS:\s+(\d+)\s+kB`)

// SampleWorkerRss mirrors sampleWorkerRss: sample RSS of a pid in bytes.
// Linux: /proc/<pid>/status VmRSS; macOS: ps -o rss= -p <pid> (KB → bytes);
// returns nil on unsupported platforms or when the process is not found.
func SampleWorkerRss(pid int) *int64 {
	return SampleWorkerRssWith(nil, pid)
}

// SampleWorkerRssWith is SampleWorkerRss with an injected command runner
// (tests script the `ps` output); nil runner = default os/exec.
func SampleWorkerRssWith(run CommandRunner, pid int) *int64 {
	if pid <= 0 {
		return nil
	}
	if runtime.GOOS == "linux" {
		text, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			return nil
		}
		m := vmRssRe.FindStringSubmatch(string(text))
		if m == nil {
			return nil
		}
		kb, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return nil
		}
		rss := int64(kb) * 1024
		return &rss
	}
	if runtime.GOOS == "darwin" {
		if run == nil {
			run = defaultCommandRunner
		}
		stdout, err := run("ps", []string{"-o", "rss=", "-p", strconv.Itoa(pid)}, 10_000)
		if err != nil {
			return nil
		}
		kb, err := strconv.ParseFloat(strings.TrimSpace(stdout), 64)
		if err != nil || math.IsNaN(kb) || math.IsInf(kb, 0) || kb <= 0 {
			return nil
		}
		rss := int64(kb) * 1024
		return &rss
	}
	// Unsupported platform
	return nil
}

// ConcurrencyValue is the parsed concurrency CLI value (TS:
// number | 'auto'). Auto distinguishes the 'auto' sentinel; Value carries
// the concrete worker count otherwise.
type ConcurrencyValue struct {
	Auto  bool
	Value int
}

// ParseConcurrencyInput mirrors parseConcurrencyInput: number or 'auto';
// numeric strings become numbers, 'auto' stays auto. Unparseable values
// fall back to 2.
func ParseConcurrencyInput(raw any) ConcurrencyValue {
	if s, ok := raw.(string); ok {
		if s == "auto" || s == "Auto" {
			return ConcurrencyValue{Auto: true}
		}
		if strings.ToLower(jsTrim(s)) == "auto" {
			return ConcurrencyValue{Auto: true}
		}
		if n, ok := jsStringToNumber(s); ok {
			return ConcurrencyValue{Value: maxInt(1, int(math.Floor(n)))}
		}
		return ConcurrencyValue{Value: 2}
	}
	if f, ok := raw.(float64); ok && !math.IsNaN(f) && !math.IsInf(f, 0) {
		return ConcurrencyValue{Value: maxInt(1, int(math.Floor(f)))}
	}
	if f, ok := raw.(int); ok {
		return ConcurrencyValue{Value: maxInt(1, f)}
	}
	return ConcurrencyValue{Value: 2}
}

// jsStringToNumber mirrors Number(raw) for the string branch: ” is 0,
// whitespace-padded numerics parse, non-numeric strings fail (TS NaN).
func jsStringToNumber(s string) (float64, bool) {
	t := jsTrim(s)
	if t == "" {
		return 0, true
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// EffectiveConcurrencyResult mirrors the TS resolveEffectiveConcurrency
// return shape.
type EffectiveConcurrencyResult struct {
	Effective    int        `json:"effective"`
	Snapshot     OsSnapshot `json:"snapshot"`
	WasThrottled bool       `json:"wasThrottled"`
}

// ResolveEffectiveConcurrency mirrors resolveEffectiveConcurrency: resolve
// effective concurrency for scheduler/fleet. A nil injectedSnapshot falls
// back to the governor's cached OS read. wasThrottled is the heuristic
// "throttled if below cpu cap" (TS: effective < ceil(cpus * 1.0)).
func ResolveEffectiveConcurrency(input ConcurrencyValue, governor *ResourceGovernor, injectedSnapshot *OsSnapshot) EffectiveConcurrencyResult {
	snap := injectedSnapshot
	if snap == nil {
		snap = governor.GetSnapshotSync()
	}
	if !input.Auto {
		return EffectiveConcurrencyResult{Effective: input.Value, Snapshot: *snap, WasThrottled: false}
	}
	effective := governor.EffectiveAuto(*snap)
	wasThrottled := float64(effective) < math.Ceil(float64(snap.Cpus)*1.0) // heuristic: throttled if below cpu cap
	return EffectiveConcurrencyResult{Effective: effective, Snapshot: *snap, WasThrottled: wasThrottled}
}

// defaultOsSnapshot reads live OS signals through the runner seam (Node's
// os.* module in the TS). Linux uses /proc; darwin shells sysctl/vm_stat;
// other platforms return zeroed signals with the runtime CPU count.
// Best-effort: unreadable signals stay 0.
func defaultOsSnapshot(run CommandRunner) OsSnapshot {
	snap := OsSnapshot{Cpus: runtime.NumCPU()}
	if run == nil {
		run = defaultCommandRunner
	}
	switch runtime.GOOS {
	case "linux":
		if data, err := os.ReadFile("/proc/meminfo"); err == nil {
			total := int64(0)
			avail := int64(0)
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "MemTotal:") {
					total = parseMeminfoKb(line) * 1024
				} else if strings.HasPrefix(line, "MemAvailable:") {
					avail = parseMeminfoKb(line) * 1024
				}
			}
			snap.TotalMem = total
			snap.FreeMem = avail
		}
		if data, err := os.ReadFile("/proc/loadavg"); err == nil {
			fields := strings.Fields(string(data))
			if len(fields) >= 3 {
				snap.LoadAvg = parseLoadAvg3(fields[:3])
			}
		}
	case "darwin":
		if out, err := run("sysctl", []string{"-n", "hw.memsize"}, 10_000); err == nil {
			if v, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64); err == nil {
				snap.TotalMem = v
			}
		}
		if out, err := run("vm_stat", nil, 10_000); err == nil {
			snap.FreeMem = parseVmStatFree(out)
		}
		if out, err := run("sysctl", []string{"-n", "vm.loadavg"}, 10_000); err == nil {
			snap.LoadAvg = parseDarwinLoadAvg(out)
		}
	}
	return snap
}

// parseMeminfoKb parses the kb value of a "/proc/meminfo" line.
func parseMeminfoKb(line string) int64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// parseLoadAvg3 converts three load-average strings to floats.
func parseLoadAvg3(fields []string) []float64 {
	out := make([]float64, 0, 3)
	for _, f := range fields {
		v, err := strconv.ParseFloat(f, 64)
		if err != nil {
			return nil
		}
		out = append(out, v)
	}
	return out
}

// vmStatPagesFreeRe matches vm_stat's "Pages free: 12345." line.
var vmStatPagesFreeRe = regexp.MustCompile(`Pages free:\s+(\d+)`)

// vmStatPageSizeRe matches vm_stat's "page size of 16384 bytes" line.
var vmStatPageSizeRe = regexp.MustCompile(`page size of (\d+) bytes`)

// parseVmStatFree computes free bytes from vm_stat output.
func parseVmStatFree(out string) int64 {
	m := vmStatPagesFreeRe.FindStringSubmatch(out)
	if m == nil {
		return 0
	}
	pages, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0
	}
	pageSize := int64(16384)
	if pm := vmStatPageSizeRe.FindStringSubmatch(out); pm != nil {
		if v, err := strconv.ParseInt(pm[1], 10, 64); err == nil && v > 0 {
			pageSize = v
		}
	}
	return pages * pageSize
}

// parseDarwinLoadAvg parses sysctl vm.loadavg output "{ 1.79 2.15 2.30 }".
func parseDarwinLoadAvg(out string) []float64 {
	s := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(out), "{ "), " }")
	fields := strings.Fields(s)
	if len(fields) < 3 {
		return nil
	}
	return parseLoadAvg3(fields[:3])
}
