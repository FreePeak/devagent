// Package file mirrors test/governor.test.ts (FR-GO-07, issue #194) — the
// ResourceGovernor core and sampleWorkerRss describes. The scheduler/fleet
// governor-enforcement describes belong to scheduler.ts/fleet.ts ports.

package orchestrator

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func gi64(v int64) *int64     { return &v }
func gf64(v float64) *float64 { return &v }
func gi(v int) *int           { return &v }

func govSnapshot(total, free int64, cpus int) OsSnapshot {
	return OsSnapshot{TotalMem: total, FreeMem: free, Cpus: cpus}
}

func TestResourceGovernorCore(t *testing.T) {
	t.Run("effectiveConcurrency: plenty RAM uses cpu cap", func(t *testing.T) {
		gov := NewResourceGovernor(&GovernorOptions{EstMemPerWorkerBytes: gi64(1_000_000_000), SafetyRatio: gf64(0.7), CPUFactor: gf64(1)})
		snap := govSnapshot(16_000_000_000, 8_000_000_000, 4)
		// memCap = floor(8e9*0.7/1e9)=5, cpuCap=4, min=4, clamp [1,8]=4
		if got := gov.EffectiveConcurrency(snap, 8); got != 4 {
			t.Fatalf("effectiveConcurrency = %d, want 4", got)
		}
	})

	t.Run("effectiveConcurrency: exactly one worker of RAM", func(t *testing.T) {
		gov := NewResourceGovernor(&GovernorOptions{EstMemPerWorkerBytes: gi64(1_000_000_000), SafetyRatio: gf64(0.7), CPUFactor: gf64(1)})
		// avail 1.5GB -> memCap floor(1.5*0.7)=1, cpus 4 -> cpuCap 4 -> min 1
		snap := govSnapshot(8_000_000_000, 1_500_000_000, 4)
		if got := gov.EffectiveConcurrency(snap, 4); got != 1 {
			t.Fatalf("effectiveConcurrency = %d, want 1", got)
		}
	})

	t.Run("effectiveConcurrency: zero free RAM still >=1", func(t *testing.T) {
		gov := NewResourceGovernor(&GovernorOptions{EstMemPerWorkerBytes: gi64(1_000_000_000), SafetyRatio: gf64(0.7), CPUFactor: gf64(1)})
		snap := govSnapshot(8_000_000_000, 0, 8)
		if got := gov.EffectiveConcurrency(snap, 4); got != 1 {
			t.Fatalf("effectiveConcurrency = %d, want 1", got)
		}
	})

	t.Run("effectiveConcurrency: cpu-bound cap", func(t *testing.T) {
		gov := NewResourceGovernor(&GovernorOptions{EstMemPerWorkerBytes: gi64(200_000_000), SafetyRatio: gf64(0.7), CPUFactor: gf64(0.5)})
		// 8 cpus *0.5=4 -> ceil 4, memCap huge -> limited to 4, configured 8 -> 4
		snap := govSnapshot(64_000_000_000, 32_000_000_000, 8)
		if got := gov.EffectiveConcurrency(snap, 8); got != 4 {
			t.Fatalf("effectiveConcurrency = %d, want 4", got)
		}
	})

	t.Run("effectiveConcurrency clamps to configured ceiling", func(t *testing.T) {
		gov := NewResourceGovernor(&GovernorOptions{EstMemPerWorkerBytes: gi64(100_000_000), SafetyRatio: gf64(1), CPUFactor: gf64(1)})
		snap := govSnapshot(32_000_000_000, 16_000_000_000, 32)
		// memCap huge, cpuCap 32, min 32, but configured 2 => 2
		if got := gov.EffectiveConcurrency(snap, 2); got != 2 {
			t.Fatalf("effectiveConcurrency = %d, want 2", got)
		}
	})

	t.Run("effectiveAuto returns mem/cpu bound without configured ceiling", func(t *testing.T) {
		gov := NewResourceGovernor(&GovernorOptions{EstMemPerWorkerBytes: gi64(1_000_000_000), SafetyRatio: gf64(0.7), CPUFactor: gf64(1)})
		snap := govSnapshot(16_000_000_000, 3_100_000_000, 16)
		// memCap floor(3.1*0.7)=2, cpuCap 16 => min 2
		if got := gov.EffectiveAuto(snap); got != 2 {
			t.Fatalf("effectiveAuto = %d, want 2", got)
		}
	})

	t.Run("performs no network calls and <5ms per check (benchmark)", func(t *testing.T) {
		gov := NewResourceGovernor(nil)
		snap := govSnapshot(16_000_000_000, 8_000_000_000, 4)
		start := time.Now()
		for i := 0; i < 100; i++ {
			gov.EffectiveConcurrency(snap, 4)
		}
		elapsed := time.Since(start)
		if elapsed/100 > 5*time.Millisecond {
			t.Fatalf("avg check = %v, want < 5ms", elapsed/100)
		}
	})

	t.Run("OS reads are cached for <=1s", func(t *testing.T) {
		gov := NewResourceGovernor(&GovernorOptions{CacheTtlMs: gi(1000)})
		s1 := gov.GetSnapshotSync()
		s2 := gov.GetSnapshotSync()
		if s1 != s2 { // same pointer from cache
			t.Fatal("expected the same cached snapshot")
		}
		// force refresh
		s3 := gov.RefreshSnapshot()
		if s3 == s1 {
			t.Fatal("refresh must return a new snapshot")
		}
	})

	t.Run("calibration raises estMemPerWorker via p75 and never below floor", func(t *testing.T) {
		gov := NewResourceGovernor(&GovernorOptions{EstMemPerWorkerBytes: gi64(200_000_000), MinEstFloorBytes: gi64(100_000_000)})
		if got := gov.GetEstMemPerWorker(); got != 200_000_000 {
			t.Fatalf("initial est = %d", got)
		}
		// push 4 samples: 100,200,300,400 -> p75 is 300
		for _, rss := range []float64{100_000_000, 200_000_000, 300_000_000, 400_000_000} {
			gov.RecordRss(rss)
		}
		if got := gov.GetEstMemPerWorker(); got != 300_000_000 {
			t.Fatalf("calibrated est = %d, want 300000000", got)
		}
		// smaller sample doesn't lower it
		gov.RecordRss(50_000_000)
		if got := gov.GetEstMemPerWorker(); got != 300_000_000 {
			t.Fatalf("est after small sample = %d, want 300000000", got)
		}
	})

	t.Run("parseConcurrencyInput handles auto and numbers", func(t *testing.T) {
		cases := []struct {
			raw  any
			auto bool
			n    int
		}{
			{"auto", true, 0},
			{"Auto", true, 0},
			{"4", false, 4},
			{2, false, 2},
			{"  auto  ", true, 0},
		}
		for _, c := range cases {
			got := ParseConcurrencyInput(c.raw)
			if got.Auto != c.auto || (!c.auto && got.Value != c.n) {
				t.Fatalf("parseConcurrencyInput(%v) = %+v, want auto=%v n=%d", c.raw, got, c.auto, c.n)
			}
		}
	})

	t.Run("formatStatus contains expected fields", func(t *testing.T) {
		gov := NewResourceGovernor(&GovernorOptions{EstMemPerWorkerBytes: gi64(1_000_000_000)})
		snap := govSnapshot(16*1024*1024*1024, 8*1024*1024*1024, 8)
		line := gov.FormatStatus(ConcurrencyValue{Auto: true}, 2, &snap)
		for _, want := range []string{"workers auto->2", "mem", "est"} {
			if !contains(line, want) {
				t.Fatalf("line %q missing %q", line, want)
			}
		}
	})

	t.Run("formatStatus reports sample age from cachedAt and calibration count (Q14)", func(t *testing.T) {
		now := int64(1_000_000)
		gov := NewResourceGovernor(&GovernorOptions{EstMemPerWorkerBytes: gi64(1_000_000_000)})
		gov.SetClock(func() int64 { return now })
		snap := govSnapshot(16*1024*1024*1024, 8*1024*1024*1024, 8)
		gov.InjectSnapshot(&snap)
		gov.RecordRss(1_500_000_000)
		gov.RecordRss(1_600_000_000)
		line := gov.FormatStatus(ConcurrencyValue{Auto: true}, 2, &snap)
		if !contains(line, "sample 0.0s") || !contains(line, "cal 2") {
			t.Fatalf("line = %q", line)
		}
		// age grows with cachedAt distance
		now += 2_500
		line = gov.FormatStatus(ConcurrencyValue{Auto: true}, 2, &snap)
		if !contains(line, "sample 2.5s") {
			t.Fatalf("line = %q", line)
		}
		// numeric input keeps the same one-line shape
		line = gov.FormatStatus(ConcurrencyValue{Value: 4}, 3, &snap)
		if !contains(line, "workers 3/4") || !contains(line, "sample 2.5s, cal 2") {
			t.Fatalf("line = %q", line)
		}
	})

	t.Run("formatStatus marks age n/a for uncached snapshots and cal 0 before calibration", func(t *testing.T) {
		gov := NewResourceGovernor(&GovernorOptions{EstMemPerWorkerBytes: gi64(1_000_000_000)})
		external := govSnapshot(16_000_000_000, 8_000_000_000, 8)
		line := gov.FormatStatus(ConcurrencyValue{Auto: true}, 2, &external)
		if !contains(line, "sample n/a") || !contains(line, "cal 0") {
			t.Fatalf("line = %q", line)
		}
		// resetCalibration drops the count back to 0 (aggregate only, no per-pid data)
		gov.InjectSnapshot(&external)
		gov.RecordRss(2_000_000_000)
		if line := gov.FormatStatus(ConcurrencyValue{Auto: true}, 2, &external); !contains(line, "cal 1") {
			t.Fatalf("line = %q", line)
		}
		gov.ResetCalibration()
		if line := gov.FormatStatus(ConcurrencyValue{Auto: true}, 2, &external); !contains(line, "cal 0") {
			t.Fatalf("line = %q", line)
		}
	})
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func TestSampleWorkerRss(t *testing.T) {
	t.Run("returns real RSS for current process on macOS/Linux or null on unsupported", func(t *testing.T) {
		rss := SampleWorkerRss(os.Getpid())
		if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
			if rss == nil || *rss <= 0 {
				t.Fatalf("rss = %v, want > 0", rss)
			}
		} else if rss != nil {
			t.Fatalf("unsupported platform must return nil, got %v", *rss)
		}
	})

	t.Run("returns null for invalid pid", func(t *testing.T) {
		if SampleWorkerRss(-1) != nil {
			t.Fatal("pid -1 must be nil")
		}
		if SampleWorkerRss(9999999) != nil {
			t.Fatal("nonexistent pid must be nil")
		}
	})

	t.Run("parses scripted darwin ps output through the runner seam", func(t *testing.T) {
		if runtime.GOOS != "darwin" {
			t.Skip("darwin-only seam check")
		}
		run := CommandRunner(func(name string, args []string, timeoutMs int) (string, error) {
			if name != "ps" {
				return "", fmt.Errorf("unexpected %s", name)
			}
			return "  4242\n", nil
		})
		rss := SampleWorkerRssWith(run, 123)
		if rss == nil || *rss != 4242*1024 {
			t.Fatalf("rss = %v, want %d", rss, 4242*1024)
		}
		// non-numeric / non-positive output → nil
		run2 := CommandRunner(func(name string, args []string, timeoutMs int) (string, error) { return "abc\n", nil })
		if SampleWorkerRssWith(run2, 123) != nil {
			t.Fatal("garbage ps output must be nil")
		}
		run3 := CommandRunner(func(name string, args []string, timeoutMs int) (string, error) { return "0\n", nil })
		if SampleWorkerRssWith(run3, 123) != nil {
			t.Fatal("zero rss must be nil")
		}
	})
}
