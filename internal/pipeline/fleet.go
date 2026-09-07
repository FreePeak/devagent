// Fleet execution (v2 gap: multi-repo management): run one or more tickets
// against N repositories over a bounded concurrency pool. Failure isolation:
// a repo that errors is recorded and does not stall the rest (Orca's
// task-dispatch lesson — workers fail independently).
package pipeline

import (
	"fmt"
	"sync"
	"time"

	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/orchestrator"
)

// FleetEntry mirrors fleet.ts FleetEntry.
type FleetEntry struct {
	// Logical repo name for the result surface.
	Name string `json:"name"`
	// Absolute path to the repository checkout.
	Path string `json:"path"`
}

// FleetRunArgs mirrors the args object fleet.ts hands to the injected runOne.
type FleetRunArgs struct {
	RepoPath  string
	TicketID  string
	Worker    WorkerName
	AutoPr    bool
	MaxLoops  int
	TimeoutMs int
	Log       RunLog
}

// FleetRunResult mirrors the { ok, summary } shape runOne returns.
type FleetRunResult struct {
	OK      bool
	Summary string
}

// FleetOptions mirrors fleet.ts FleetRunOptions.
type FleetOptions struct {
	TicketIDs []string
	Entries   []FleetEntry
	// Concurrency mirrors TS `number | 'auto'`; parse CLI input with
	// orchestrator.ParseConcurrencyInput.
	Concurrency orchestrator.ConcurrencyValue
	// Governor resolves the effective pool size in auto mode; nil falls back
	// to the TS default of 2.
	Governor *orchestrator.ResourceGovernor
	// GovernorSnapshot injects a pre-read snapshot (TS: governorSnapshot).
	GovernorSnapshot *orchestrator.OsSnapshot
	TimeoutMs        int
	Worker           WorkerName
	AutoPr           bool
	MaxLoops         int
	// RunOne is injected so fleet stays unit-testable; same shape the CLI
	// builds. It must not return an error (TS runOne rejects are recorded
	// per-item; Go panics are recovered the same way).
	RunOne func(FleetRunArgs) FleetRunResult
}

// FleetItem mirrors fleet.ts FleetResultItem.
type FleetItem struct {
	Entry    string `json:"entry"`
	TicketID string `json:"ticketId"`
	OK       bool   `json:"ok"`
	Summary  string `json:"summary"`
	LogPath  string `json:"logPath,omitempty"` // "" = undefined
}

// FleetResult mirrors fleet.ts FleetResult.
type FleetResult struct {
	Items     []FleetItem `json:"items"`
	Succeeded int         `json:"succeeded"`
	Failed    int         `json:"failed"`
}

// RunFleet maps tickets × repos onto at most `concurrency` concurrent runs.
func RunFleet(opts FleetOptions) FleetResult {
	var jobs []struct {
		entry    FleetEntry
		ticketID string
	}
	for _, entry := range opts.Entries {
		for _, ticketID := range opts.TicketIDs {
			jobs = append(jobs, struct {
				entry    FleetEntry
				ticketID string
			}{entry, ticketID})
		}
	}

	isAuto := opts.Concurrency.Auto
	resolveEffective := func() int {
		if !isAuto {
			return max(1, opts.Concurrency.Value)
		}
		if opts.Governor == nil {
			return 2
		}
		snap := opts.Governor.GetSnapshotSync()
		if opts.GovernorSnapshot != nil {
			snap = opts.GovernorSnapshot
		}
		if snap == nil {
			snap = &orchestrator.OsSnapshot{}
		}
		return opts.Governor.EffectiveAuto(*snap)
	}

	effective := resolveEffective()
	poolSize := max(1, min(effective, len(jobs)))

	var mu sync.Mutex
	var cursor int
	var active int
	items := make([]FleetItem, 0)

	worker := func() {
		for {
			if isAuto && opts.Governor != nil {
				// TS reads governor.pressureWaitTimeoutMs (default 60_000);
				// the Go governor keeps it unexported at the same default.
				const pressureWaitTimeoutMs = 60_000
				start := time.Now()
				for {
					mu.Lock()
					curEff := resolveEffective()
					below := active < curEff
					mu.Unlock()
					if below {
						break
					}
					if time.Since(start) >= time.Duration(pressureWaitTimeoutMs)*time.Millisecond {
						break
					}
					time.Sleep(50 * time.Millisecond)
				}
			}
			mu.Lock()
			idx := cursor
			cursor++
			mu.Unlock()
			if idx >= len(jobs) {
				return
			}
			job := jobs[idx]
			mu.Lock()
			active++
			mu.Unlock()

			log, logPath := fltNewRunLogger()
			item := FleetItem{Entry: job.entry.Name, TicketID: job.ticketID}
			func() {
				defer func() {
					// Isolation: record and continue (TS catch arm).
					if r := recover(); r != nil {
						item.OK = false
						item.Summary = fmt.Sprint(r)
					}
					mu.Lock()
					if active > 0 {
						active--
					}
					mu.Unlock()
				}()
				r := opts.RunOne(FleetRunArgs{
					RepoPath:  job.entry.Path,
					TicketID:  job.ticketID,
					Worker:    opts.Worker,
					AutoPr:    opts.AutoPr,
					MaxLoops:  opts.MaxLoops,
					TimeoutMs: opts.TimeoutMs,
					Log:       log,
				})
				item.OK = r.OK
				item.Summary = r.Summary
			}()
			item.LogPath = logPath
			mu.Lock()
			items = append(items, item)
			mu.Unlock()
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < poolSize; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker()
		}()
	}
	wg.Wait()

	succeeded, failed := 0, 0
	for _, item := range items {
		if item.OK {
			succeeded++
		} else {
			failed++
		}
	}
	return FleetResult{Items: items, Succeeded: succeeded, Failed: failed}
}

// fltNewRunLogger opens a per-run JSONL log the way `new RunLogger()` does.
// On open failure the run still proceeds with a no-op logger and no path
// (TS: the constructor never fails).
func fltNewRunLogger() (RunLog, string) {
	l, err := ledger.NewRunLogger("")
	if err != nil || l == nil {
		return fltNoopLog{}, ""
	}
	return l, l.Path()
}

// fltNoopLog is the failure-path stand-in for a *ledger.RunLogger.
type fltNoopLog struct{}

func (fltNoopLog) Info(ledger.RunStage, string, []ledger.KV)  {}
func (fltNoopLog) Warn(ledger.RunStage, string, []ledger.KV)  {}
func (fltNoopLog) Error(ledger.RunStage, string, []ledger.KV) {}
