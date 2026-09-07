// actions_status.go ports the `status` and `dashboard` command bodies from
// src/cli.ts (lines 574-678) plus the read-only ports of
// src/resilience/proxy-state.ts (readProxyState) and
// src/resilience/degradation.ts (degradationStreak / readEvents). Every
// output literal is byte-identical to the TypeScript original — stdout
// strings, blank lines, and exit codes included (FR-GO-02, issue #193).
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/herdr"
	"github.com/FreePeak/devagent/internal/tui"
	"github.com/FreePeak/devagent/internal/workers/modelid"
	"github.com/spf13/cobra"
)

// statusCommand ports the `status` command: the §20.8 phase card + recent
// runs (FR-SIMPLE-03/04), the --json machine payload, and the --providers
// observability report — proxy-probe state, the trailing degradation
// streak, and the dispatch model-id preflight verdict (Q32/Q41).
func statusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show status: plain-language phase card + recent runs (§20.8 FR-SIMPLE-03/04); --json emits the phase view as JSON",
		RunE: func(cmd *cobra.Command, args []string) error {
			limit, _ := cmd.Flags().GetInt("limit")
			repo, _ := cmd.Flags().GetString("repo")
			providers, _ := cmd.Flags().GetBool("providers")
			degradeThreshold, _ := cmd.Flags().GetInt("degrade-threshold")
			jsonMode, _ := cmd.Flags().GetBool("json")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			if jsonMode {
				fmt.Println(tui.StatusJSON(buildStatusViewLocal(repo)))
				return nil
			}
			if providers {
				return runStatusProviders(repo, degradeThreshold)
			}
			runStatusHuman(repo, limit)
			return nil
		},
	}
}

// dashboardCommand ports the `dashboard` command (src/cli.ts:670-678).
func dashboardCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "dashboard",
		Short: "Generate a static HTML status board from run logs",
		RunE: func(cmd *cobra.Command, args []string) error {
			res := tui.WriteDashboard(devagentHome(), nil)
			fmt.Printf("%d run(s) -> %s\n", res.Runs, res.Path)
			return nil
		},
	}
}

// buildStatusViewLocal assembles the inputs the TS buildStatusView
// (src/commands/status.ts) reads — orchestrator board, queue counts, live
// herdr pane roster, config presence — and delegates composition to
// tui.ComposeStatusView (the port of buildStatusView itself).
func buildStatusViewLocal(repoPath string) tui.StatusView {
	board := loadProjectBoard(repoPath)
	queue := queueCounts(repoPath)
	roster := herdr.ListSessionPanes(herdr.ExecRunner{}, herdr.ResolveSession(""))
	panes := make([]tui.StatusPane, 0, len(roster))
	for _, p := range roster {
		panes = append(panes, tui.StatusPane{TaskID: p.TaskID, State: p.State})
	}
	hasConfig := false
	for _, name := range []string{"devagent.json", ".devagent.json"} {
		if _, err := os.Stat(filepath.Join(repoPath, name)); err == nil {
			hasConfig = true
			break
		}
	}
	return tui.ComposeStatusView(board, queue, panes, hasConfig)
}

// devagentHome mirrors the TS home resolution shared by the status run
// listing and the dashboard writer:
// `process.env.DEVAGENT_HOME || join(process.env.HOME || '.', '.devagent')`.
func devagentHome() string {
	if h := os.Getenv("DEVAGENT_HOME"); h != "" {
		return h
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = "."
	}
	return filepath.Join(home, ".devagent")
}

// runStatusHuman prints the phase card, a blank line, and the recent-runs
// table, then the ResourceGovernor line — src/cli.ts:630-668.
func runStatusHuman(repoPath string, limit int) {
	view := buildStatusViewLocal(repoPath)
	// Math.max(46, Math.min(process.stdout?.columns ?? 100, 100))
	columns := terminalColumns()
	if columns > 100 {
		columns = 100
	}
	if columns < 46 {
		columns = 46
	}
	for _, line := range strings.Split(tui.RenderStatusCard(view, columns), "\n") {
		fmt.Println(line)
	}
	fmt.Println("")

	home := devagentHome()
	runsDir := filepath.Join(home, "runs")
	var files []string
	if entries, err := os.ReadDir(runsDir); err != nil {
		fmt.Println("No runs yet.")
	} else {
		for _, e := range entries {
			// TS filters names only: a directory named *.jsonl would make
			// Node crash on readFileSync; the port skips the unreadable
			// entry instead of crashing (pathological, never produced).
			if strings.HasSuffix(e.Name(), ".jsonl") {
				files = append(files, e.Name())
			}
		}
		sort.Strings(files)
		files = sliceFromEnd(files, limit)
	}
	if len(files) == 0 {
		fmt.Println("No runs yet.")
	} else {
		for _, file := range files {
			printRunTail(filepath.Join(runsDir, file))
		}
	}

	// Governor line, best-effort (src/cli.ts:659-667 wraps it in try/catch;
	// the Go reads cannot fail — they fall back to zeros/defaults — so no
	// recover is needed). A fresh snapshot means sample age 0.0s and zero
	// RSS calibration samples, exactly like a new TS ResourceGovernor.
	snap := collectOsSnapshot()
	eff := effectiveAuto(snap)
	fmt.Println(formatStatusAuto(eff, snap))
}

// sliceFromEnd mirrors the TS `files.slice(-(opts.limit))`: a positive
// limit takes the trailing `limit` entries; limit 0 takes everything
// (Array#slice(-0) === Array#slice(0)); a negative limit drops that many
// leading entries.
func sliceFromEnd(files []string, limit int) []string {
	start := -limit
	if start < 0 {
		start = len(files) + start
		if start < 0 {
			start = 0
		}
	}
	if start > len(files) {
		start = len(files)
	}
	return files[start:]
}

// printRunTail mirrors the per-file summary row: trim the file, take the
// last line, parse it, print `<runId[:8]>  <ts>  [<stage>/<level>] <message>`.
// The TS try block wraps both the JSON.parse and the template evaluation,
// so a line without a string runId throws and is skipped; missing fields
// print as "undefined" exactly like JS template literals.
func printRunTail(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return // TS: trim().split('\n') → [''], !last → continue
	}
	last := trimmed
	if idx := strings.LastIndexByte(trimmed, '\n'); idx >= 0 {
		last = trimmed[idx+1:]
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(last), &row); err != nil {
		return
	}
	runID, ok := row["runId"].(string)
	if !ok {
		return // TS: e.runId.slice throws for undefined/non-string → caught
	}
	if len(runID) > 8 {
		runID = runID[:8]
	}
	fmt.Printf("%s  %s  [%s/%s] %s\n", runID, jsField(row, "ts"), jsField(row, "stage"), jsField(row, "level"), jsField(row, "message"))
}

// jsField reads one row field with the TS `e.key` semantics: an absent key
// is JS `undefined` ("undefined" in a template literal), while a present
// JSON null is JS `null` ("null"). A plain map lookup conflates the two in
// Go, so presence is checked first.
func jsField(row map[string]any, key string) string {
	if v, ok := row[key]; ok {
		return jsStr(v)
	}
	return "undefined"
}

// jsStr renders a JSON-decoded value the way a JS template literal would
// (`${value}`): strings pass through, JSON null is "null", booleans and
// numbers stringify, arrays join with ",", objects are "[object Object]".
// (The `undefined` case — absent keys — is handled by jsField.) Run-log
// rows carry string fields, so the number/edge branches only ever fire on
// hand-written files (JS number formatting is approximated for values
// outside the integer range).
func jsStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e21 {
			return strconv.FormatFloat(t, 'f', -1, 64)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = jsStr(e)
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	default:
		return fmt.Sprint(v)
	}
}

// jsonStringify mirrors JSON.stringify for a single string value (the only
// type the status surfaces stringify): compact JSON with UTF-8 passthrough
// and NO HTML escaping — json.Marshal alone would escape < > &, which Node
// does not.
func jsonStringify(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return "" // unreachable for a string value
	}
	return strings.TrimSuffix(buf.String(), "\n") // Encode appends a newline
}

// runStatusProviders ports the --providers block (src/cli.ts:587-629): the
// proxy-probe state, the trailing degradation streak, and the model-id
// preflight verdict. The block RETURNS here (TS `return`), so the run
// listing below is never reached.
func runStatusProviders(repoPath string, degradeThreshold int) error {
	state := readProxyState(repoPath)
	if state == nil {
		fmt.Println("No provider state recorded yet. The orchestrate-loop probe gate populates .devagent/proxy-state.json.")
	} else {
		fmt.Println("Providers:")
		if state.LastProbe != nil {
			probeWord := "fail"
			if state.LastProbe.Ok != nil && *state.LastProbe.Ok {
				probeWord = "ok"
			}
			probeLine := fmt.Sprintf("probe: %s @ %s", probeWord, state.LastProbe.At)
			if state.LastProbe.Detail != nil && *state.LastProbe.Detail != "" {
				probeLine += " — " + *state.LastProbe.Detail
			}
			fmt.Println("  " + probeLine)
		} else {
			fmt.Println("  probe: never recorded")
		}
		if state.LastTransient != nil {
			excerpt := state.LastTransient.Excerpt
			if runes := []rune(excerpt); len(runes) > 120 {
				excerpt = string(runes[:120]) // TS excerpt.slice(0, 120)
			}
			fmt.Printf("  transient: %s @ %s — %s\n", state.LastTransient.Class, state.LastTransient.At, excerpt)
		} else {
			fmt.Println("  transient: none recorded")
		}
		since := state.CircuitChangedAt
		if since == "" {
			since = state.UpdatedAt // TS: circuitChangedAt ?? updatedAt
		}
		fmt.Printf("  circuit: %s (since %s)\n", state.Circuit, since)
	}

	// Consecutive cross-role provider degradation (Q41): degraded rows are
	// starvation-gate-exempt (scripts/selfbuild-loop.sh:190), so a sustained
	// outage is invisible on every human surface unless aggregated here.
	// Breach line at >= --degrade-threshold trailing degraded rows.
	streak := readDegradationStreak(repoPath, degradeThreshold)
	if streak.Breach {
		window := ""
		if streak.WindowMs != nil {
			window = fmt.Sprintf(" in %ds", int64(math.Floor(*streak.WindowMs/1000+0.5))) // Math.round
		}
		roles := ""
		if len(streak.Roles) > 0 {
			roles = " across roles: " + strings.Join(streak.Roles, ", ")
		}
		fmt.Printf("degradation: %d consecutive degraded rows%s%s — provider outage (threshold %d)\n",
			streak.Count, window, roles, streak.Threshold)
	}

	// Dispatch model-id preflight verdict (PRD Phase 4 Q32): the same gate
	// executor.ts/deps.ts run before any worker spend, surfaced here so a
	// smoke run can show validation applied without needing a live dispatch.
	cfg, err := config.Load(repoPath)
	if err != nil {
		return err // TS loadConfig throw surfaces as an unhandled error
	}
	worker := cfg.Worker
	if worker == "both" {
		worker = "claude-code"
	}
	problem := modelid.ValidateModelId(worker, cfg.Model)
	wName := cfg.Worker
	if wName == "both" {
		wName = "claude-code | opencode"
	}
	verdict := "ok"
	if problem != "" {
		verdict = "REJECTED"
	}
	fmt.Printf("Model validation (%s): %s\n", wName, verdict)
	if cfg.Model != "" {
		fmt.Printf("  model: %s\n", jsonStringify(cfg.Model))
	} else {
		fmt.Println("  model: (unset — adapter default)")
	}
	if problem != "" {
		fmt.Printf("  reason: %s\n", problem)
		os.Exit(1)
	}
	return nil
}

// proxyProbeRecord mirrors ProxyProbeRecord (proxy-state.ts:22-29). Ok is a
// pointer so a missing key renders as TS `undefined` → 'fail'.
type proxyProbeRecord struct {
	Ok     *bool   `json:"ok"`
	At     string  `json:"at"`
	Detail *string `json:"detail"`
}

// transientRecord mirrors TransientRecord (proxy-state.ts:31-38).
type transientRecord struct {
	Class   string `json:"class"`
	At      string `json:"at"`
	Excerpt string `json:"excerpt"`
}

// proxyState mirrors ProxyState (proxy-state.ts:40-48). CircuitChangedAt is
// a plain string because the TS writer always emits it; a hand-stripped
// empty value coalesces to updatedAt, matching the `??` gate for the
// null/undefined cases the writer cannot produce.
type proxyState struct {
	Circuit          string            `json:"circuit"`
	CircuitChangedAt string            `json:"circuitChangedAt"`
	LastProbe        *proxyProbeRecord `json:"lastProbe"`
	LastTransient    *transientRecord  `json:"lastTransient"`
	UpdatedAt        string            `json:"updatedAt"`
}

// readProxyState mirrors proxy-state.ts readProxyState(): the durable
// <repo>/.devagent/proxy-state.json, nil when missing, unreadable, corrupt,
// not an object, or carrying no truthy `circuit` (the TS falsy gate
// `!raw || typeof raw !== 'object' || !raw.circuit`; the circuit is always
// one of closed|open|half-open from recordProxyProbe, so truthiness reduces
// to a non-empty string).
func readProxyState(repoPath string) *proxyState {
	raw, err := os.ReadFile(filepath.Join(repoPath, ".devagent", "proxy-state.json"))
	if err != nil {
		return nil
	}
	var st proxyState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil
	}
	if st.Circuit == "" {
		return nil
	}
	return &st
}

// degradedLoopResultStatuses mirrors DEGRADED_LOOP_RESULT_STATUSES
// (degradation.ts:29): loop-result statuses meaning "the loop paused
// without provider progress" — provider-degraded (preflight found the
// provider down; no spend) and operator-diverged. Every other loop-result
// status means the provider answered — productive.
var degradedLoopResultStatuses = map[string]bool{
	"provider-degraded": true,
	"operator-diverged": true,
}

// degradeStreak mirrors the DegradationStreak aggregation
// (degradation.ts:32-47); nil WindowMs models TS null.
type degradeStreak struct {
	Count     int
	Threshold int
	Breach    bool
	LatestTs  string   // "" when Count == 0 (TS null)
	OldestTs  string   // "" when no degraded row carried a string ts
	WindowMs  *float64 // nil unless both streak edges parse
	Roles     []string // distinct roles on operator-degraded rows, newest first
}

// isDegradationRow mirrors degradation.ts isDegradationRow(): operator-
// degraded events with ok !== true (deliberately cross-role; an ok:true row
// is a passed probe, i.e. recovery → productive), or loop-result rows whose
// status is provider-degraded | operator-diverged. Everything else is
// productive and stops the walk.
func isDegradationRow(row map[string]any) bool {
	if event, ok := row["event"].(string); ok && event == "operator-degraded" {
		if okVal, isBool := row["ok"].(bool); isBool && okVal {
			return false // ok === true: passed probe
		}
		return true
	}
	if event, ok := row["event"].(string); ok && event == "loop-result" {
		status, isStr := row["status"].(string)
		return isStr && degradedLoopResultStatuses[status]
	}
	return false
}

// parseDateMs mirrors the row.ts parse in degradation.ts: Date.parse on a
// string, NaN otherwise → (0, false). Ledger rows carry ISO-8601 stamps
// (new Date().toISOString()), which time.Parse(RFC3339) accepts including
// fractional seconds; the fallbacks cover the two further ES Date.parse
// forms a hand-edited row may use — zone-less date-time parses as LOCAL
// time, date-only parses as UTC midnight. Exotic non-ISO formats stay
// unparseable (TS NaN), which is the safe direction: the streak window is
// simply not reported.
func parseDateMs(v any) (float64, bool) {
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return float64(t.UnixMilli()), true
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05.999999999", s, time.Local); err == nil {
		return float64(t.UnixMilli()), true
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05.999999999", s, time.Local); err == nil {
		return float64(t.UnixMilli()), true
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return float64(t.UnixMilli()), true // already UTC
	}
	return 0, false
}

// degradationStreakRows ports degradation.ts degradationStreak(): walk the
// rows (file order, oldest first) newest-first counting the trailing run of
// degradation rows; stop at the first productive row. The first degraded
// row fixes newestMs; every further row overwrites oldestMs — the window is
// only meaningful when both streak edges parse.
func degradationStreakRows(rows []map[string]any, threshold int) degradeStreak {
	var latestTs, oldestTs string
	var newestMs, oldestMs *float64
	var roles []string
	count := 0
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		if row == nil || !isDegradationRow(row) {
			break
		}
		count++
		if ts, isStr := row["ts"].(string); isStr {
			if latestTs == "" {
				latestTs = ts
			}
			oldestTs = ts
		}
		var parsed *float64
		if ms, ok := parseDateMs(row["ts"]); ok {
			parsed = &ms
		}
		if count == 1 {
			newestMs = parsed
		}
		oldestMs = parsed
		if event, isEvent := row["event"].(string); isEvent && event == "operator-degraded" {
			if role, isStr := row["role"].(string); isStr && role != "" {
				seen := false
				for _, r := range roles {
					if r == role {
						seen = true
						break
					}
				}
				if !seen {
					roles = append(roles, role)
				}
			}
		}
	}
	var windowMs *float64
	if newestMs != nil && oldestMs != nil {
		w := *newestMs - *oldestMs
		windowMs = &w
	}
	return degradeStreak{
		Count:     count,
		Threshold: threshold,
		Breach:    count > 0 && count >= threshold,
		LatestTs:  latestTs,
		OldestTs:  oldestTs,
		WindowMs:  windowMs,
		Roles:     roles,
	}
}

// readDegradationStreak ports degradation.ts readDegradationStreak: parse
// the orchestration events.jsonl best-effort and aggregate the trailing
// degradation streak for `devagent status --providers`.
func readDegradationStreak(repoPath string, threshold int) degradeStreak {
	return degradationStreakRows(readOrchestrationEvents(repoPath), threshold)
}

// readOrchestrationEvents mirrors lessons/guard.ts readEvents(): every
// parseable JSON object row of <repo>/.devagent/runs/orchestration/events.jsonl
// in file order; blank and corrupt lines are silently skipped, absent file
// = no rows.
// TODO(FR-GO-05): move beside the lessons package port when it lands.
func readOrchestrationEvents(repoPath string) []map[string]any {
	raw, err := os.ReadFile(filepath.Join(repoPath, ".devagent", "runs", "orchestration", "events.jsonl"))
	if err != nil {
		return nil
	}
	var rows []map[string]any
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue // TS: split('\n').filter(Boolean)
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil || row == nil {
			continue // skip corrupt line; TS rejects null/non-object parses
		}
		rows = append(rows, row)
	}
	return rows
}

// Governor snapshot math ported from src/orchestrator/governor.ts defaults:
// 0.7 safety headroom, 1 GiB estimated memory per worker, 1.0 CPU factor.
const (
	govSafetyRatio     = 0.7
	govEstMemPerWorker = 1024.0 * 1024.0 * 1024.0
	giB                = 1024.0 * 1024.0 * 1024.0
)

// effectiveAuto mirrors governor.effectiveAuto() with default options:
// max(1, min(floor(freeMem * 0.7 / 1 GiB), ceil(cpus * 1.0))).
func effectiveAuto(snap osSnapshot) int {
	cpus := snap.cpus
	if cpus == 0 {
		cpus = 1 // TS: snapshot.cpus || 1
	}
	memCap := int(math.Floor(float64(snap.freeMem) * govSafetyRatio / govEstMemPerWorker))
	cpuCap := int(math.Ceil(float64(cpus) * 1.0))
	if eff := min(memCap, cpuCap); eff > 1 {
		return eff
	}
	return 1
}

// formatStatusAuto mirrors governor.formatStatus('auto', eff, snap) for a
// freshly-taken snapshot: est is the 1 GiB default ("1.0"), the sample age
// is "0.0s" (the TS snapshot was taken this instant), and calibration has
// observed zero RSS samples.
func formatStatusAuto(eff int, snap osSnapshot) string {
	freeGb := toFixed1(float64(snap.freeMem) / giB)
	totalGb := toFixed1(float64(snap.totalMem) / giB)
	return fmt.Sprintf("workers auto->%d (mem %s GB free of %s, est 1.0 GB/worker, cpus %d, sample 0.0s, cal 0)",
		eff, freeGb, totalGb, snap.cpus)
}

// toFixed1 is Number#toFixed(1) for the byte-magnitude values printed here:
// both round the true binary double to one decimal.
func toFixed1(v float64) string {
	return strconv.FormatFloat(v, 'f', 1, 64)
}
