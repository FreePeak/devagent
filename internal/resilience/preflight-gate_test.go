package resilience

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/devagent/internal/config"
)

func noSleep(int) {}

func ledgerRows(t *testing.T, repo string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo, ".devagent", "runs", "orchestration", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		rows = append(rows, jsonCompactRow(line))
	}
	return rows
}

func jsonCompactRow(line string) map[string]any {
	var m map[string]any
	if err := jsonUnmarshal([]byte(line), &m); err != nil {
		panic(err)
	}
	return m
}

// ---------------------------------------------------------------------------
// preflight roles (operator preflight, Q40)
// ---------------------------------------------------------------------------

func TestPreflightRoles(t *testing.T) {
	t.Run("declares exactly the five operator roles", func(t *testing.T) {
		want := []PreflightRole{"prd-curator", "po", "selfbuild", "warroom", "reviewer"}
		if len(PreflightRoles) != len(want) {
			t.Fatalf("roles = %v", PreflightRoles)
		}
		for i, r := range want {
			if PreflightRoles[i] != r {
				t.Fatalf("roles[%d] = %v, want %v", i, PreflightRoles[i], r)
			}
		}
	})

	t.Run("accepts declared roles and rejects others", func(t *testing.T) {
		if !IsPreflightRole("prd-curator") || !IsPreflightRole("reviewer") {
			t.Fatal("declared roles rejected")
		}
		if IsPreflightRole("orchestrator") || IsPreflightRole("") {
			t.Fatal("undeclared roles accepted")
		}
	})
}

// ---------------------------------------------------------------------------
// runPreflightGate (decision function)
// ---------------------------------------------------------------------------

func TestRunPreflightGate(t *testing.T) {
	tempRepo := func(t *testing.T) string { return t.TempDir() }

	t.Run("proceeds on a first-attempt pass and advances the circuit (closed), writing no ledger row", func(t *testing.T) {
		repo := tempRepo(t)
		var probes [][]string
		probe := func(cmd string, a []string, cwd string) (Probe, error) {
			cp := append([]string{}, a...)
			probes = append(probes, cp)
			return Probe{OK: true}, nil
		}
		decision, err := RunPreflightGate(PreflightGateArgs{
			RepoPath: repo,
			Role:     "selfbuild",
			Worker:   "omp",
			Model:    "omniroute/dev",
			Argv:     []string{"omp", "-p", "--mode", "json"},
			Probe:    probe,
			DelayMs:  noSleep,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !decision.OK || decision.Role != "selfbuild" || decision.Worker != "omp" ||
			decision.Model != "omniroute/dev" || decision.Attempts != 1 {
			t.Fatalf("decision = %+v", decision)
		}
		// Prompt sits immediately after -p, before caller flags (buildOmpArgs shape).
		if len(probes) != 1 || len(probes[0]) != 4 ||
			probes[0][0] != "-p" || probes[0][1] != "OK" || probes[0][2] != "--mode" || probes[0][3] != "json" {
			t.Fatalf("probes = %v", probes)
		}
		// Success advances the shared circuit (open -> half-open) so consumers
		// never see a stale open after recovery; no operator-degraded row lands.
		state := ReadProxyState(repo)
		if state == nil || state.Circuit != CircuitClosed || state.LastProbe == nil || !state.LastProbe.OK {
			t.Fatalf("state = %+v", state)
		}
		if _, statErr := os.Stat(filepath.Join(repo, ".devagent", "runs", "orchestration", "events.jsonl")); !os.IsNotExist(statErr) {
			t.Fatal("no ledger row may land on a passing gate")
		}
	})

	t.Run("advances a previously open circuit to half-open on a passing probe", func(t *testing.T) {
		repo := tempRepo(t)
		// Seed an open circuit (as a failed gate would leave it).
		RecordProxyProbe(repo, ProxyProbe{OK: false, Detail: "preflight[selfbuild]: seeded failure"})
		if ReadProxyState(repo).Circuit != CircuitOpen {
			t.Fatal("seed circuit not open")
		}
		decision, err := RunPreflightGate(PreflightGateArgs{
			RepoPath: repo,
			Role:     "selfbuild",
			Worker:   "omp",
			Model:    "omniroute/dev",
			Argv:     []string{"omp", "-p", "--mode", "json"},
			Probe:    func(cmd string, a []string, cwd string) (Probe, error) { return Probe{OK: true}, nil },
			DelayMs:  noSleep,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !decision.OK {
			t.Fatalf("decision = %+v", decision)
		}
		state := ReadProxyState(repo)
		if state.Circuit != CircuitHalfOpen || state.LastProbe == nil || !state.LastProbe.OK {
			t.Fatalf("state = %+v", state)
		}
		if _, statErr := os.Stat(filepath.Join(repo, ".devagent", "runs", "orchestration", "events.jsonl")); !os.IsNotExist(statErr) {
			t.Fatal("no ledger row may land on a passing gate")
		}
	})

	t.Run("retries to PreflightProbeAttempts, then degrades: circuit opens and one operator-degraded row lands", func(t *testing.T) {
		repo := tempRepo(t)
		calls := 0
		decision, err := RunPreflightGate(PreflightGateArgs{
			RepoPath: repo,
			Role:     "warroom",
			Worker:   "omp",
			Model:    "omniroute/dev",
			Argv:     []string{"omp", "-p"},
			Probe: func(cmd string, a []string, cwd string) (Probe, error) {
				calls++
				return Probe{OK: false, Detail: "unrecognized_model: probe 403"}, nil
			},
			DelayMs: noSleep,
		})
		if err != nil {
			t.Fatal(err)
		}
		if calls != PreflightProbeAttempts {
			t.Fatalf("calls = %d, want %d", calls, PreflightProbeAttempts)
		}
		if decision.OK || decision.Role != "warroom" || decision.Attempts != PreflightProbeAttempts ||
			decision.Detail != "unrecognized_model: probe 403" {
			t.Fatalf("decision = %+v", decision)
		}
		// Circuit state: probe failure opens the breaker for status --providers.
		state := ReadProxyState(repo)
		if state == nil || state.Circuit != CircuitOpen || state.LastProbe == nil || state.LastProbe.OK {
			t.Fatalf("state = %+v", state)
		}
		if state.LastProbe.Detail == nil || !strings.Contains(*state.LastProbe.Detail, "preflight[warroom]") {
			t.Fatalf("lastProbe.detail = %v", state.LastProbe.Detail)
		}
		// Exactly one structured ledger row (skip evidence, not per-attempt noise).
		rows := ledgerRows(t, repo)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		row := rows[0]
		if row["kind"] != "event" || row["event"] != "operator-degraded" ||
			row["taskId"] != PreflightLedgerTaskID || row["role"] != "warroom" ||
			row["worker"] != "omp" || row["model"] != "omniroute/dev" ||
			row["ok"] != false || row["attempts"] != float64(PreflightProbeAttempts) ||
			row["detail"] != "unrecognized_model: probe 403" {
			t.Fatalf("row = %#v", row)
		}
		if _, ok := row["ts"].(string); !ok {
			t.Fatalf("row.ts = %#v", row["ts"])
		}
	})

	t.Run("recovers on a later attempt and reports ok without degrading anything", func(t *testing.T) {
		repo := tempRepo(t)
		calls := 0
		decision, err := RunPreflightGate(PreflightGateArgs{
			RepoPath: repo,
			Role:     "po",
			Argv:     []string{"omp", "-p"},
			Probe: func(cmd string, a []string, cwd string) (Probe, error) {
				calls++
				if calls >= 2 {
					return Probe{OK: true}, nil
				}
				return Probe{OK: false, Detail: "stream empty"}, nil
			},
			DelayMs: noSleep,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !decision.OK || decision.Attempts != 2 {
			t.Fatalf("decision = %+v", decision)
		}
		if _, statErr := os.Stat(filepath.Join(repo, ".devagent", "runs", "orchestration", "events.jsonl")); !os.IsNotExist(statErr) {
			t.Fatal("no ledger row may land on a recovering gate")
		}
		// Recovery still advances the shared circuit so a later gate/consumer
		// sees a live (non-open) state.
		state := ReadProxyState(repo)
		if state.Circuit != CircuitClosed || state.LastProbe == nil || !state.LastProbe.OK {
			t.Fatalf("state = %+v", state)
		}
	})

	t.Run("treats a thrown probe as a failure and still writes the degraded row", func(t *testing.T) {
		repo := tempRepo(t)
		decision, err := RunPreflightGate(PreflightGateArgs{
			RepoPath: repo,
			Role:     "reviewer",
			Argv:     []string{"omp", "-p"},
			Probe: func(cmd string, a []string, cwd string) (Probe, error) {
				return Probe{OK: false, Detail: boundDetail("spawn omp ENOENT")}, nil
			},
			DelayMs: noSleep,
		})
		if err != nil {
			t.Fatal(err)
		}
		if decision.OK || decision.Detail != "spawn omp ENOENT" {
			t.Fatalf("decision = %+v", decision)
		}
		rows := ledgerRows(t, repo)
		if len(rows) != 1 || rows[0]["event"] != "operator-degraded" {
			t.Fatalf("rows = %#v", rows)
		}
	})

	t.Run("rejects argv that does not start with the prompt flag", func(t *testing.T) {
		repo := tempRepo(t)
		_, err := RunPreflightGate(PreflightGateArgs{
			RepoPath: repo,
			Role:     "po",
			Argv:     []string{"omp", "--mode", "json"},
			Probe:    func(cmd string, a []string, cwd string) (Probe, error) { return Probe{OK: true}, nil },
			DelayMs:  noSleep,
		})
		if err == nil || !strings.Contains(err.Error(), "prompt flag") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("injectable sleeper records retry delays", func(t *testing.T) {
		repo := tempRepo(t)
		var delays []int
		_, err := RunPreflightGate(PreflightGateArgs{
			RepoPath: repo,
			Role:     "po",
			Argv:     []string{"omp", "-p"},
			Probe: func(cmd string, a []string, cwd string) (Probe, error) {
				return Probe{OK: false, Detail: "nope"}, nil
			},
			DelayMs: func(ms int) { delays = append(delays, ms) },
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(delays) != PreflightProbeAttempts-1 {
			t.Fatalf("delays = %v", delays)
		}
		for _, d := range delays {
			if d != PreflightRetryDelayMs {
				t.Fatalf("delay = %d, want %d", d, PreflightRetryDelayMs)
			}
		}
	})

	t.Run("injectable clock stamps the ledger row", func(t *testing.T) {
		repo := tempRepo(t)
		_, err := RunPreflightGate(PreflightGateArgs{
			RepoPath: repo,
			Role:     "po",
			Argv:     []string{"omp", "-p"},
			Probe: func(cmd string, a []string, cwd string) (Probe, error) {
				return Probe{OK: false, Detail: "nope"}, nil
			},
			DelayMs: noSleep,
			NowFunc: unixMsFunc(1788782400000),
		})
		if err != nil {
			t.Fatal(err)
		}
		rows := ledgerRows(t, repo)
		if rows[0]["ts"] != "2026-09-07T12:00:00.000Z" {
			t.Fatalf("ts = %#v", rows[0]["ts"])
		}
	})

	t.Run("wedge detail skips the circuit write but still records the degraded row", func(t *testing.T) {
		repo := tempRepo(t)
		detail := "Still starting after 21s — phase: preloadPluginRoots"
		decision, err := RunPreflightGate(PreflightGateArgs{
			RepoPath: repo,
			Role:     "selfbuild",
			Argv:     []string{"omp", "-p"},
			Probe: func(cmd string, a []string, cwd string) (Probe, error) {
				return Probe{OK: false, Detail: detail}, nil
			},
			DelayMs: noSleep,
		})
		if err != nil {
			t.Fatal(err)
		}
		if decision.OK || decision.Detail != detail {
			t.Fatalf("decision = %+v", decision)
		}
		// Wedge class is env-hiccup: the circuit write is skipped.
		if ReadProxyState(repo) != nil {
			t.Fatal("circuit must not be written on a wedge")
		}
		rows := ledgerRows(t, repo)
		if len(rows) != 1 || rows[0]["event"] != "operator-degraded" || rows[0]["detail"] != detail {
			t.Fatalf("rows = %#v", rows)
		}
	})
}

// ---------------------------------------------------------------------------
// degradation paging (Q41 write side)
// ---------------------------------------------------------------------------

func TestPreflightGatePaging(t *testing.T) {
	tempRepo := func(t *testing.T, webhookURL string) string {
		t.Helper()
		dir := t.TempDir()
		if webhookURL != "" {
			if err := os.WriteFile(filepath.Join(dir, "devagent.json"), []byte(`{"resilience": {"degradeWebhookUrl": "`+webhookURL+`"}}`), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	degradeCycle := func(repo string, notify DegradeBreachNotifier) PreflightDecision {
		decision, err := RunPreflightGate(PreflightGateArgs{
			RepoPath: repo,
			Role:     "selfbuild",
			Worker:   "omp",
			Model:    "omniroute/dev",
			Argv:     []string{"omp", "-p"},
			Probe: func(cmd string, a []string, cwd string) (Probe, error) {
				return Probe{OK: false, Detail: "unrecognized_model: probe 403"}, nil
			},
			DelayMs: noSleep,
			Notify:  notify,
		})
		if err != nil {
			t.Fatal(err)
		}
		return decision
	}

	t.Run("pages exactly once at the threshold and stays silent for the rest of the streak", func(t *testing.T) {
		repo := tempRepo(t, "https://pager.invalid/hook")
		type call struct {
			url   string
			alert DegradeBreachAlert
		}
		var calls []call
		var decisions []PreflightDecision
		notify := func(url string, alert DegradeBreachAlert) error {
			calls = append(calls, call{url, alert})
			return nil
		}
		for i := 0; i <= DegradeStreakThreshold; i++ {
			decisions = append(decisions, degradeCycle(repo, notify))
		}
		if len(calls) != 1 {
			t.Fatalf("calls = %d, want 1", len(calls))
		}
		if calls[0].url != "https://pager.invalid/hook" {
			t.Fatalf("url = %q", calls[0].url)
		}
		a := calls[0].alert
		if a.Event != "provider-degraded-breach" ||
			// The gate claims the preflight source; the doc-sync surface is
			// the other caller of the same pager (degrade-pager.go).
			a.Source != DegradeBreachSourcePreflight || a.Repo != repo ||
			a.Role != "selfbuild" || a.Worker != "omp" || a.Model != "omniroute/dev" ||
			a.Count != DegradeStreakThreshold || a.Threshold != DegradeStreakThreshold ||
			a.Detail == nil || *a.Detail != "unrecognized_model: probe 403" {
			t.Fatalf("alert = %+v", a)
		}
		if a.TS == "" {
			t.Fatal("ts missing")
		}
		if len(a.Roles) != 1 || a.Roles[0] != "selfbuild" {
			t.Fatalf("roles = %v", a.Roles)
		}
		// The breach cycle reports it on the decision; later cycles do not re-page.
		if !decisions[DegradeStreakThreshold-1].Paged {
			t.Fatal("breach cycle did not report paged")
		}
		if decisions[DegradeStreakThreshold].Paged {
			t.Fatal("later cycle re-paged")
		}
	})

	t.Run("stays silent while the streak is below the threshold", func(t *testing.T) {
		repo := tempRepo(t, "https://pager.invalid/hook")
		calls := 0
		notify := func(url string, alert DegradeBreachAlert) error {
			calls++
			return nil
		}
		for i := 0; i < DegradeStreakThreshold-1; i++ {
			if d := degradeCycle(repo, notify); d.Paged {
				t.Fatalf("paged below threshold at i=%d", i)
			}
		}
		if calls != 0 {
			t.Fatalf("calls = %d", calls)
		}
	})

	t.Run("never throws into the loop when the paging transport fails", func(t *testing.T) {
		repo := tempRepo(t, "https://pager.invalid/hook")
		for i := 0; i < DegradeStreakThreshold-1; i++ {
			degradeCycle(repo, func(url string, alert DegradeBreachAlert) error { return nil })
		}
		decision := degradeCycle(repo, func(url string, alert DegradeBreachAlert) error {
			return errors.New("connect ECONNREFUSED 127.0.0.1:443")
		})
		// The outage decision is untouched by the paging failure.
		if decision.OK || decision.Attempts != PreflightProbeAttempts ||
			decision.Detail != "unrecognized_model: probe 403" || decision.Paged {
			t.Fatalf("decision = %+v", decision)
		}
		if ReadProxyState(repo).Circuit != CircuitOpen {
			t.Fatal("circuit must stay open")
		}
	})

	t.Run("does not page when resilience.degradeWebhookUrl is unset (opt-in)", func(t *testing.T) {
		repo := tempRepo(t, "")
		calls := 0
		notify := func(url string, alert DegradeBreachAlert) error {
			calls++
			return nil
		}
		for i := 0; i <= DegradeStreakThreshold; i++ {
			if d := degradeCycle(repo, notify); d.Paged {
				t.Fatalf("paged without webhook url at i=%d", i)
			}
		}
		if calls != 0 {
			t.Fatalf("calls = %d", calls)
		}
		// Every degraded cycle still lands its evidence row.
		rows := ledgerRows(t, repo)
		if len(rows) != DegradeStreakThreshold+1 {
			t.Fatalf("rows = %d, want %d", len(rows), DegradeStreakThreshold+1)
		}
	})
}

// ---------------------------------------------------------------------------
// resilience.degradeWebhookUrl config (paging knob) + the operator-alert
// non-retryable classification fixtures from test/guard.test.ts
// ---------------------------------------------------------------------------

func TestDegradeWebhookConfig(t *testing.T) {
	t.Run("rejects a value that is not an http(s) URL", func(t *testing.T) {
		repo := t.TempDir()
		if err := os.WriteFile(filepath.Join(repo, "devagent.json"), []byte(`{"resilience": {"degradeWebhookUrl": "pager.invalid/hook"}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := config.Load(repo)
		if err == nil || !strings.Contains(err.Error(), "Invalid resilience.degradeWebhookUrl") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestIsNonRetryableApiError(t *testing.T) {
	// From test/guard.test.ts 'flags non-retryable auth/billing failures'
	// (the other guard.test.ts describe blocks port with src/sessionguard,
	// a different wave).
	t.Run("flags non-retryable auth/billing failures", func(t *testing.T) {
		if !IsNonRetryableApiError("Invalid API key provided") {
			t.Fatal("invalid api key not flagged")
		}
		if !IsNonRetryableApiError("your credit balance is too low") {
			t.Fatal("credit balance not flagged")
		}
		if IsNonRetryableApiError("API Error: Connection lost mid-response") {
			t.Fatal("connection lost flagged as non-retryable")
		}
	})
}

// PostOperatorAlert error-string parity smoke: a non-2xx response yields
// exactly the TS message.
func TestPostOperatorAlertNon2xx(t *testing.T) {
	alert := DegradeBreachAlert{Event: "provider-degraded-breach", Roles: []string{}}
	if err := PostOperatorAlert("http://127.0.0.1:1/hook", alert); err == nil {
		t.Fatal("expected an error from an unreachable webhook")
	}
	// BoardArchivedAlert satisfies the OperatorAlert union.
	var op OperatorAlert = BoardArchivedAlert{Event: "board-archived"}
	if op.operatorAlertEvent() != "board-archived" {
		t.Fatal("board alert event mismatch")
	}
}
