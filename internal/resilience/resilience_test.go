package resilience

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func strPtr(s string) *string { return &s }
func intPtr2(i int) *int      { return &i }

// ---------------------------------------------------------------------------
// isTransientProviderError / transientErrorClass
// ---------------------------------------------------------------------------

func TestIsTransientProviderError(t *testing.T) {
	transient := []string{
		"endpoint is unavailable",
		"upstream request failed",
		"error from provider",
		"connection lost before init",
		"connection refused",
		"ETIMEDOUT",
		"ECONNRESET",
		"ECONNREFUSED 127.0.0.1:20128",
		"ENOTFOUND api.example.com",
		"fetch failed",
		"network error while streaming",
		"overloaded",
		"rate limit exceeded",
		"too many requests",
		"tokens per minute (TPM) limit exceeded",
		"tokens_per_minute budget exhausted",
		"requests per second (RPS) limit",
		"Server error (503) from https://api.x.ai",
		"429 too many requests",
		"529 overloaded",
		"timeout while waiting",
		"request timed out",
		"unavailable",
		"service unavailable",
		"bad gateway",
		"gateway timeout on proxy",
		"socket hang up",
		"unrecognized_model error from omniroute proxy",
		"[claude-code:unrecognized_model] {\"model\":\"cmd/minimax\"}",
		"Empty stream at flush",
		"empty response (no content block)",
	}
	for _, s := range transient {
		in := s
		if !IsTransientProviderError(&in) {
			t.Errorf("IsTransientProviderError(%q) = false, want true", s)
		}
	}
	notTransient := []string{
		"",
		"Result: I implemented the feature as requested",
		"test passed: 1/1",
		"invalid api key — fetch failed", // non-retryable gates the transient check
		"billing: credit balance is low — error from provider",
		"authentication failed",
		"unauthorized",
		"model not found: grok-99",
	}
	for _, s := range notTransient {
		in := s
		if IsTransientProviderError(&in) {
			t.Errorf("IsTransientProviderError(%q) = true, want false", s)
		}
	}
	if IsTransientProviderError(nil) {
		t.Error("IsTransientProviderError(nil) = true, want false")
	}
}

func TestTransientErrorClass(t *testing.T) {
	cases := []struct {
		in   *string
		want string
	}{
		{strPtr(`[claude-code:unrecognized_model] {"model":"cmd/minimax/minimax-m3-free"}`), "unrecognized-model"},
		{strPtr("unrecognized_model error from omniroute proxy"), "unrecognized-model"},
		{strPtr("Empty stream at flush"), "empty-stream"},
		{strPtr("Claude returned an empty response (no content block)"), "empty-stream"},
		{strPtr("429 too many requests"), "rate-limit"},
		{strPtr("rate limit exceeded, retry after 60s"), "rate-limit"},
		{strPtr("too many requests on upstream"), "rate-limit"},
		// xAI 429 split (FR-GROK-06): TPM vs RPS.
		{strPtr("429 rate_limit_error: exceeded tokens per minute (TPM) limit"), "rate-limit-tpm"},
		{strPtr("tokens_per_minute budget exhausted on grok-4.6"), "rate-limit-tpm"},
		{strPtr("429 requests per second (RPS) limit exceeded"), "rate-limit-rps"},
		{strPtr("rps limit hit on proxy"), "rate-limit-rps"},
		// Numeric 5xx without masking named classes.
		{strPtr("Server error (503) from https://api.x.ai/v1/chat/completions"), "server-error"},
		{strPtr("500 internal server error"), "server-error"},
		{strPtr("bad gateway 502"), "bad-gateway"},
		{strPtr("overloaded 529"), "rate-limit"},
		{strPtr("connect ECONNREFUSED 127.0.0.1:503"), "network"},
		{strPtr("overloaded"), "overloaded"},
		{strPtr("Service overloaded, try again"), "overloaded"},
		{strPtr("ETIMEDOUT"), "timeout"},
		{strPtr("gateway timeout on proxy"), "bad-gateway"},
		{strPtr("connection lost before init"), "network"},
		{strPtr("ECONNREFUSED 127.0.0.1:9000"), "network"},
		{strPtr("fetch failed"), "network"},
		{strPtr("socket hang up"), "network"},
		{strPtr("endpoint is unavailable"), "unavailable"},
		{strPtr("upstream request failed"), "upstream"},
		{strPtr("error from provider (code 503)"), "upstream"},
		{strPtr("service unavailable"), "unavailable"},
		// Not transient.
		{strPtr("Result: I implemented the feature as requested"), ""},
		{strPtr("test passed: 1/1"), ""},
		{nil, ""},
		{strPtr(""), ""},
		// Non-retryable words gate the transient check even mid-sentence.
		{strPtr("invalid api key — fetch failed"), ""},
		{strPtr("billing: credit balance is low — error from provider"), ""},
	}
	for _, c := range cases {
		if got := TransientErrorClass(c.in); got != c.want {
			name := "<nil>"
			if c.in != nil {
				name = *c.in
			}
			t.Errorf("TransientErrorClass(%q) = %q, want %q", name, got, c.want)
		}
	}
}

func TestTransientClassConsistentWithTransient(t *testing.T) {
	probeSamples := []*string{
		strPtr("429 too many requests"),
		strPtr("overloaded"),
		strPtr("Empty stream at flush"),
		strPtr(`[claude-code:unrecognized_model] {"model":"cmd/minimax/minimax-m3-free"}`),
		strPtr("connect ECONNREFUSED 127.0.0.1:20128"),
		strPtr("ETIMEDOUT"),
		strPtr("fetch failed"),
		strPtr("gateway timeout on proxy"),
		strPtr("Result: I implemented the feature"),
		nil,
	}
	for _, raw := range probeSamples {
		expected := IsTransientProviderError(raw)
		label := TransientErrorClass(raw)
		if (label != "") != expected {
			t.Fatalf("class/Transient mismatch for %v: class=%q transient=%v", raw, label, expected)
		}
	}
}

func TestIsRetryableWithoutSession(t *testing.T) {
	cases := []struct {
		name string
		opts RetryableWithoutSessionOpts
		want bool
	}{
		{"timedOut is transient by default", RetryableWithoutSessionOpts{TimedOut: true}, true},
		{"empty text is not retryable", RetryableWithoutSessionOpts{}, false},
		{"rate limit retries without a session", RetryableWithoutSessionOpts{ErrorText: strPtr("429 rate limit exceeded")}, true},
		{"overload retries without a session", RetryableWithoutSessionOpts{Stderr: strPtr("upstream overloaded")}, true},
		{"auth failure does not retry", RetryableWithoutSessionOpts{ErrorText: strPtr("invalid api key")}, false},
		{"plain output does not retry", RetryableWithoutSessionOpts{ErrorText: strPtr("done with the task")}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsRetryableWithoutSession(c.opts); got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// proxy-state file
// ---------------------------------------------------------------------------

func TestProxyStateFile(t *testing.T) {
	repoPath := t.TempDir()

	t.Run("readProxyState returns null for an absent file", func(t *testing.T) {
		if ReadProxyState(repoPath) != nil {
			t.Fatal("want nil")
		}
	})

	t.Run("readProxyState returns null for a corrupt file", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(repoPath, ".devagent"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ProxyStatePath(repoPath), []byte("not-json {"), 0o644); err != nil {
			t.Fatal(err)
		}
		if ReadProxyState(repoPath) != nil {
			t.Fatal("want nil")
		}
	})

	t.Run("recordProxyProbe ok → closed; fail → open; ok after open → half-open; ok after half-open → closed", func(t *testing.T) {
		state := RecordProxyProbe(repoPath, ProxyProbe{OK: true})
		if state.Circuit != CircuitClosed || state.LastProbe == nil || !state.LastProbe.OK {
			t.Fatalf("state = %+v", state)
		}
		state = RecordProxyProbe(repoPath, ProxyProbe{OK: false})
		if state.Circuit != CircuitOpen || state.LastProbe.OK {
			t.Fatalf("state = %+v", state)
		}
		state = RecordProxyProbe(repoPath, ProxyProbe{OK: true})
		if state.Circuit != CircuitHalfOpen {
			t.Fatalf("circuit = %v, want half-open", state.Circuit)
		}
		state = RecordProxyProbe(repoPath, ProxyProbe{OK: true})
		if state.Circuit != CircuitClosed {
			t.Fatalf("circuit = %v, want closed", state.Circuit)
		}
		payload, err := os.ReadFile(ProxyStatePath(repoPath))
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := unmarshalStrict(payload, &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["circuit"].(string); !ok {
			t.Fatalf("payload.circuit = %#v", raw["circuit"])
		}
		if _, ok := raw["updatedAt"].(string); !ok {
			t.Fatalf("payload.updatedAt = %#v", raw["updatedAt"])
		}
	})

	t.Run("recordProxyProbe stores detail and circuitChangedAt", func(t *testing.T) {
		RecordProxyProbe(repoPath, ProxyProbe{OK: true, Detail: "attempt 1/3"})
		state := ReadProxyState(repoPath)
		if state == nil || state.LastProbe == nil || state.LastProbe.Detail == nil || *state.LastProbe.Detail != "attempt 1/3" {
			t.Fatalf("state = %+v", state)
		}
		if state.CircuitChangedAt == "" {
			t.Fatal("circuitChangedAt missing")
		}
	})

	t.Run("recordTransientClass writes class + bounded excerpt for transient text", func(t *testing.T) {
		r := RecordTransientClass(repoPath, "429 too many requests because the proxy is rate limiting upstream")
		if r == nil || r.Class != "rate-limit" {
			t.Fatalf("record = %+v", r)
		}
		state := ReadProxyState(repoPath)
		if state == nil || state.LastTransient == nil || state.LastTransient.Class != "rate-limit" {
			t.Fatalf("state = %+v", state)
		}
		if !strings.Contains(state.LastTransient.Excerpt, "429") {
			t.Fatalf("excerpt = %q", state.LastTransient.Excerpt)
		}
		if len(state.LastTransient.Excerpt) > 200 {
			t.Fatalf("excerpt length = %d, want <= 200", len(state.LastTransient.Excerpt))
		}
	})

	t.Run("recordTransientClass returns null and leaves state unchanged for non-transient text", func(t *testing.T) {
		before, err := os.ReadFile(ProxyStatePath(repoPath))
		if err != nil {
			t.Fatal(err)
		}
		r := RecordTransientClass(repoPath, "Result: I implemented the feature as requested")
		if r != nil {
			t.Fatalf("record = %+v, want nil", r)
		}
		after, err := os.ReadFile(ProxyStatePath(repoPath))
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Fatal("state file changed on non-transient text")
		}
	})

	t.Run("recordTransientClass preserves an open circuit when recording", func(t *testing.T) {
		RecordProxyProbe(repoPath, ProxyProbe{OK: false}) // open
		if ReadProxyState(repoPath).Circuit != CircuitOpen {
			t.Fatal("expected open")
		}
		RecordTransientClass(repoPath, "service unavailable — upstream stalled")
		if ReadProxyState(repoPath).Circuit != CircuitOpen {
			t.Fatal("circuit must stay open")
		}
		if ReadProxyState(repoPath).LastTransient.Class != "unavailable" {
			t.Fatalf("class = %v", ReadProxyState(repoPath).LastTransient.Class)
		}
	})

	t.Run("decouples probe and transient attributes (each writer preserves the other)", func(t *testing.T) {
		RecordTransientClass(repoPath, "Empty stream at flush")
		afterTransient := ReadProxyState(repoPath)
		RecordProxyProbe(repoPath, ProxyProbe{OK: true, Detail: "attempt 1/3"})
		afterProbe := ReadProxyState(repoPath)
		if afterProbe.LastTransient == nil || afterTransient.LastTransient == nil ||
			afterProbe.LastTransient.Class != afterTransient.LastTransient.Class {
			t.Fatalf("lastTransient lost: %+v vs %+v", afterProbe.LastTransient, afterTransient.LastTransient)
		}
	})

	t.Run("circuit: closed → open → half-open → closed round-trip with ok/failed sequence", func(t *testing.T) {
		RecordProxyProbe(repoPath, ProxyProbe{OK: true}) // closed
		if ReadProxyState(repoPath).Circuit != CircuitClosed {
			t.Fatal("want closed")
		}
		RecordProxyProbe(repoPath, ProxyProbe{OK: false}) // open
		if ReadProxyState(repoPath).Circuit != CircuitOpen {
			t.Fatal("want open")
		}
		RecordProxyProbe(repoPath, ProxyProbe{OK: true}) // half-open
		if ReadProxyState(repoPath).Circuit != CircuitHalfOpen {
			t.Fatal("want half-open")
		}
		RecordProxyProbe(repoPath, ProxyProbe{OK: true}) // closed
		if ReadProxyState(repoPath).Circuit != CircuitClosed {
			t.Fatal("want closed")
		}
		RecordProxyProbe(repoPath, ProxyProbe{OK: false}) // open again
		if ReadProxyState(repoPath).Circuit != CircuitOpen {
			t.Fatal("want open")
		}
	})

	t.Run("state file is 2-space indented JSON with trailing newline", func(t *testing.T) {
		payload, err := os.ReadFile(ProxyStatePath(repoPath))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(string(payload), "\n") {
			t.Fatal("missing trailing newline")
		}
		if !strings.Contains(string(payload), "\n  \"circuit\":") {
			t.Fatalf("not 2-space indented: %q", payload[:min(80, len(payload))])
		}
	})
}

func unmarshalStrict(b []byte, v any) error {
	return jsonUnmarshal(b, v)
}

// ---------------------------------------------------------------------------
// degradationStreak
// ---------------------------------------------------------------------------

func degradationLoopResult(loop int, status string, ts string) map[string]any {
	return map[string]any{
		"ts": ts, "kind": "event", "event": "loop-result", "loop": float64(loop), "status": status,
		"goal": "Goal: surface consecutive cross-role provider degradation (loop " + itoa(loop) + ")",
	}
}

func degradationOperatorDegraded(role string, ts string, ok ...bool) map[string]any {
	row := map[string]any{
		"ts": ts, "kind": "event", "event": "operator-degraded", "taskId": "operator-preflight",
		"role": role, "worker": "omp", "model": "omniroute/dev", "ok": false, "attempts": float64(3),
		"detail": "unrecognized_model: probe 403",
	}
	if len(ok) > 0 && ok[0] {
		row["ok"] = true
		delete(row, "detail")
	}
	return row
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := ""
	for i > 0 {
		digits = string(rune('0'+i%10)) + digits
		i /= 10
	}
	return digits
}

func TestIsDegradationRow(t *testing.T) {
	t.Run("counts operator-degraded rows from every preflight role", func(t *testing.T) {
		for _, role := range []string{"prd-curator", "po", "selfbuild", "warroom", "reviewer"} {
			if !IsDegradationRow(degradationOperatorDegraded(role, "2026-09-05T12:00:00Z")) {
				t.Errorf("role %q not counted", role)
			}
		}
	})

	t.Run("treats an ok:true operator-degraded row (passed probe) as productive", func(t *testing.T) {
		if IsDegradationRow(degradationOperatorDegraded("selfbuild", "2026-09-05T12:00:00Z", true)) {
			t.Fatal("ok:true row counted as degradation")
		}
	})

	t.Run("counts loop-result provider-degraded and operator-diverged rows", func(t *testing.T) {
		if !IsDegradationRow(degradationLoopResult(106, "provider-degraded", "2026-09-05T12:00:00Z")) {
			t.Fatal("provider-degraded not counted")
		}
		if !IsDegradationRow(degradationLoopResult(107, "operator-diverged", "2026-09-05T12:00:00Z")) {
			t.Fatal("operator-diverged not counted")
		}
	})

	t.Run("treats every other loop-result status as productive (the provider answered)", func(t *testing.T) {
		for _, status := range []string{"ok", "failed", "failed-tests", "invalid", "skipped", "push-failed", "operator-degraded"} {
			if IsDegradationRow(degradationLoopResult(108, status, "2026-09-05T12:00:00Z")) {
				t.Errorf("status %q counted", status)
			}
		}
	})

	t.Run("treats unrelated ledger rows as productive", func(t *testing.T) {
		if IsDegradationRow(map[string]any{"ts": "x", "kind": "audit", "taskId": "T", "attempt": float64(1), "verdict": "pass"}) {
			t.Error("audit row counted")
		}
		if IsDegradationRow(map[string]any{"ts": "x", "kind": "event", "event": "lessons-eval", "loop": float64(108)}) {
			t.Error("lessons-eval row counted")
		}
		if IsDegradationRow(map[string]any{"ts": "x", "kind": "event", "event": "loop-phase", "loop": float64(108), "phase": "task"}) {
			t.Error("loop-phase row counted")
		}
	})
}

func TestDegradationStreak(t *testing.T) {
	t.Run("flags the loops 106-108 shape: three provider-degraded rows in 75s", func(t *testing.T) {
		rows := []map[string]any{
			degradationLoopResult(104, "ok", "2026-09-05T11:50:00Z"),
			degradationLoopResult(105, "ok", "2026-09-05T11:58:00Z"),
			degradationLoopResult(106, "provider-degraded", "2026-09-05T12:00:00Z"),
			degradationLoopResult(107, "provider-degraded", "2026-09-05T12:00:30Z"),
			degradationLoopResult(108, "provider-degraded", "2026-09-05T12:01:15Z"),
		}
		s := DegradationStreak(rows, DegradeStreakThreshold)
		if s.Count != 3 || !s.Breach || s.Threshold != DegradeStreakThreshold {
			t.Fatalf("s = %+v", s)
		}
		if s.OldestTs == nil || *s.OldestTs != "2026-09-05T12:00:00Z" || s.LatestTs == nil || *s.LatestTs != "2026-09-05T12:01:15Z" {
			t.Fatalf("edges = %v / %v", s.OldestTs, s.LatestTs)
		}
		if s.WindowMs == nil || *s.WindowMs != 75_000 {
			t.Fatalf("windowMs = %v", s.WindowMs)
		}
		if len(s.Roles) != 0 {
			t.Fatalf("roles = %v", s.Roles)
		}
	})

	t.Run("empty ledger yields a zero streak without breach", func(t *testing.T) {
		s := DegradationStreak(nil, DegradeStreakThreshold)
		if s.Count != 0 || s.Breach || s.LatestTs != nil || s.OldestTs != nil || s.WindowMs != nil {
			t.Fatalf("s = %+v", s)
		}
	})

	t.Run("a productive newest row yields count 0", func(t *testing.T) {
		rows := []map[string]any{
			degradationLoopResult(106, "provider-degraded", "2026-09-05T12:00:00Z"),
			degradationLoopResult(107, "provider-degraded", "2026-09-05T12:00:30Z"),
			degradationLoopResult(108, "ok", "2026-09-05T12:01:15Z"),
		}
		if DegradationStreak(rows, DegradeStreakThreshold).Count != 0 {
			t.Fatal("want count 0")
		}
	})

	t.Run("stops at the first productive row — only the trailing run counts", func(t *testing.T) {
		rows := []map[string]any{
			degradationLoopResult(100, "provider-degraded", "2026-09-05T10:00:00Z"),
			degradationLoopResult(101, "provider-degraded", "2026-09-05T10:01:00Z"),
			degradationLoopResult(102, "ok", "2026-09-05T10:02:00Z"),
			degradationOperatorDegraded("po", "2026-09-05T10:03:00Z"),
			degradationLoopResult(103, "provider-degraded", "2026-09-05T10:04:00Z"),
		}
		s := DegradationStreak(rows, DegradeStreakThreshold)
		if s.Count != 2 || s.Breach {
			t.Fatalf("s = %+v", s)
		}
		if s.OldestTs == nil || *s.OldestTs != "2026-09-05T10:03:00Z" {
			t.Fatalf("oldestTs = %v", s.OldestTs)
		}
	})

	t.Run("aggregates cross-role: operator-degraded rows from any role join loop-result rows in one streak", func(t *testing.T) {
		rows := []map[string]any{
			degradationLoopResult(106, "ok", "2026-09-05T12:00:00Z"),
			degradationOperatorDegraded("selfbuild", "2026-09-05T12:01:00Z"),
			degradationLoopResult(107, "provider-degraded", "2026-09-05T12:02:00Z"),
			degradationOperatorDegraded("warroom", "2026-09-05T12:03:00Z"),
			degradationOperatorDegraded("selfbuild", "2026-09-05T12:04:00Z"),
		}
		s := DegradationStreak(rows, DegradeStreakThreshold)
		if s.Count != 4 || !s.Breach {
			t.Fatalf("s = %+v", s)
		}
		// Newest-first, deduplicated.
		if len(s.Roles) != 2 || s.Roles[0] != "selfbuild" || s.Roles[1] != "warroom" {
			t.Fatalf("roles = %v", s.Roles)
		}
	})

	t.Run("honors a threshold override in both directions", func(t *testing.T) {
		var rows []map[string]any
		for n := 1; n <= 4; n++ {
			rows = append(rows, degradationLoopResult(n, "provider-degraded", "2026-09-05T12:0"+itoa(n)+":00Z"))
		}
		if DegradationStreak(rows, 5).Breach {
			t.Fatal("threshold 5 must not breach at count 4")
		}
		if DegradationStreak(rows, 5).Threshold != 5 {
			t.Fatal("threshold not honored")
		}
		if !DegradationStreak(rows, 2).Breach {
			t.Fatal("threshold 2 must breach at count 4")
		}
	})

	t.Run("never breaches at zero rows, even with a zero threshold", func(t *testing.T) {
		if DegradationStreak(nil, 0).Breach {
			t.Fatal("breach at zero rows")
		}
	})

	t.Run("keeps counting when timestamps are unparseable; windowMs is then null", func(t *testing.T) {
		rows := []map[string]any{
			degradationLoopResult(1, "provider-degraded", "not-a-date"),
			degradationLoopResult(2, "provider-degraded", "2026-09-05T12:00:00Z"),
			degradationLoopResult(3, "provider-degraded", "also-broken"),
		}
		s := DegradationStreak(rows, DegradeStreakThreshold)
		if s.Count != 3 || !s.Breach {
			t.Fatalf("s = %+v", s)
		}
		if s.LatestTs == nil || *s.LatestTs != "also-broken" {
			t.Fatalf("latestTs = %v", s.LatestTs)
		}
		if s.WindowMs != nil {
			t.Fatalf("windowMs = %v, want nil", s.WindowMs)
		}
	})

	t.Run("computes the window from the streak edges even when a middle ts is unparseable", func(t *testing.T) {
		rows := []map[string]any{
			degradationLoopResult(1, "provider-degraded", "2026-09-05T12:00:00Z"),
			degradationLoopResult(2, "provider-degraded", "not-a-date"),
			degradationLoopResult(3, "provider-degraded", "2026-09-05T12:02:00Z"),
		}
		s := DegradationStreak(rows, DegradeStreakThreshold)
		if s.Count != 3 {
			t.Fatalf("count = %d", s.Count)
		}
		if s.WindowMs == nil || *s.WindowMs != 120_000 {
			t.Fatalf("windowMs = %v, want 120000", s.WindowMs)
		}
	})
}

func TestReadDegradationStreak(t *testing.T) {
	writeLedger := func(t *testing.T, repo string, lines []string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(repo, ".devagent", "runs", "orchestration"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, EventsFilePath), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("returns a zero streak when events.jsonl is absent", func(t *testing.T) {
		repo := t.TempDir()
		s := ReadDegradationStreak(repo, DegradeStreakThreshold)
		if s.Count != 0 || s.Breach {
			t.Fatalf("s = %+v", s)
		}
	})

	t.Run("walks the trailing run of a fixture ledger, skipping corrupt lines", func(t *testing.T) {
		repo := t.TempDir()
		writeLedger(t, repo, []string{
			jsonCompact(degradationLoopResult(105, "ok", "2026-09-05T11:58:00Z")),
			"{not json at all",
			jsonCompact(degradationLoopResult(106, "provider-degraded", "2026-09-05T12:00:00Z")),
			jsonCompact(degradationLoopResult(107, "operator-diverged", "2026-09-05T12:00:30Z")),
			jsonCompact(degradationOperatorDegraded("reviewer", "2026-09-05T12:01:15Z")),
		})
		s := ReadDegradationStreak(repo, DegradeStreakThreshold)
		if s.Count != 3 || !s.Breach {
			t.Fatalf("s = %+v", s)
		}
		if s.WindowMs == nil || *s.WindowMs != 75_000 {
			t.Fatalf("windowMs = %v", s.WindowMs)
		}
		if len(s.Roles) != 1 || s.Roles[0] != "reviewer" {
			t.Fatalf("roles = %v", s.Roles)
		}
	})

	t.Run("reports no breach once a productive row lands after the outage", func(t *testing.T) {
		repo := t.TempDir()
		writeLedger(t, repo, []string{
			jsonCompact(degradationOperatorDegraded("po", "2026-09-05T12:00:00Z")),
			jsonCompact(degradationOperatorDegraded("po", "2026-09-05T12:00:30Z")),
			jsonCompact(degradationOperatorDegraded("po", "2026-09-05T12:01:00Z")),
			jsonCompact(degradationOperatorDegraded("po", "2026-09-05T12:01:30Z", true)),
			jsonCompact(degradationLoopResult(109, "ok", "2026-09-05T12:05:00Z")),
		})
		if ReadDegradationStreak(repo, DegradeStreakThreshold).Count != 0 {
			t.Fatal("want count 0")
		}
	})
}

// ---------------------------------------------------------------------------
// pageDegradeBreach (shared Q41 pager, doc-sync source)
// ---------------------------------------------------------------------------

func pagerLoopResult(loop int, status string, ts string) map[string]any {
	return map[string]any{
		"ts": ts, "kind": "event", "event": "loop-result", "loop": float64(loop), "status": status,
		"goal": "Goal: close Q41 paging gap (loop " + itoa(loop) + ")",
	}
}

func pagerAppendRow(t *testing.T, repo string, row map[string]any) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(repo, EventsFilePath), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(jsonCompact(row) + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

// tsAt: ts 30s apart, starting 2026-09-06T00:00:00Z (the live 145–154
// cadence).
func pagerTsAt(i int) string {
	return formatIsoMs(parseUnixMs(1788652800000 + int64(i)*30_000))
}

func TestPageDegradeBreach(t *testing.T) {
	tempRepo := func(t *testing.T, webhookURL string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".devagent", "runs", "orchestration"), 0o755); err != nil {
			t.Fatal(err)
		}
		if webhookURL != "" {
			if err := os.WriteFile(filepath.Join(dir, "devagent.json"), []byte(`{"resilience": {"degradeWebhookUrl": "`+webhookURL+`"}}`), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	docSyncCycle := func(repo string, i int, notify DegradeBreachNotifier, status string) bool {
		pagerAppendRow(t, repo, pagerLoopResult(145+i, status, pagerTsAt(i)))
		return PageDegradeBreach(PageDegradeBreachArgs{
			RepoPath: repo,
			Source:   DegradeBreachSourceDocSync,
			Role:     "selfbuild",
			Worker:   "omp",
			Model:    "omniroute/dev",
			Detail:   "doc-sync rc=3: fatal: the current branch " + status,
			Notify:   notify,
		})
	}

	t.Run("pages exactly once when the doc-sync streak reaches the threshold, then stays silent", func(t *testing.T) {
		repo := tempRepo(t, "https://pager.invalid/hook")
		type call struct {
			url   string
			alert DegradeBreachAlert
		}
		var calls []call
		notify := func(url string, alert DegradeBreachAlert) error {
			calls = append(calls, call{url, alert})
			return nil
		}
		var results []bool
		for i := 0; i <= DegradeStreakThreshold; i++ {
			results = append(results, docSyncCycle(repo, i, notify, "operator-diverged"))
		}
		// One POST for the whole episode: the breach cycle pages, every later
		// cycle of the same outage stays silent (Q41 once-per-episode rule).
		if len(calls) != 1 {
			t.Fatalf("calls = %d, want 1", len(calls))
		}
		for i, r := range results {
			want := i == DegradeStreakThreshold-1
			if r != want {
				t.Fatalf("results[%d] = %v, want %v", i, r, want)
			}
		}
		if calls[0].url != "https://pager.invalid/hook" {
			t.Fatalf("url = %q", calls[0].url)
		}
		a := calls[0].alert
		if a.Event != "provider-degraded-breach" || a.Source != DegradeBreachSourceDocSync || a.Repo != repo ||
			a.Role != "selfbuild" || a.Worker != "omp" || a.Model != "omniroute/dev" ||
			a.Count != DegradeStreakThreshold || a.Threshold != DegradeStreakThreshold ||
			a.Detail == nil || *a.Detail != "doc-sync rc=3: fatal: the current branch operator-diverged" {
			t.Fatalf("alert = %+v", a)
		}
		// Outage window carried off the streak rows: 3 rows, 30s apart.
		if a.OldestTs == nil || *a.OldestTs != pagerTsAt(0) {
			t.Fatalf("oldestTs = %v, want %v", a.OldestTs, pagerTsAt(0))
		}
		if a.LatestTs == nil || *a.LatestTs != pagerTsAt(DegradeStreakThreshold-1) {
			t.Fatalf("latestTs = %v", a.LatestTs)
		}
		if a.WindowMs == nil || *a.WindowMs != float64((DegradeStreakThreshold-1)*30_000) {
			t.Fatalf("windowMs = %v", a.WindowMs)
		}
		if a.TS == "" {
			t.Fatal("ts missing")
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
			if docSyncCycle(repo, i, notify, "operator-diverged") {
				t.Fatalf("paged below threshold at i=%d", i)
			}
		}
		if calls != 0 {
			t.Fatalf("calls = %d", calls)
		}
	})

	t.Run("treats a dirty-PRD refusal as productive, so a paused streak never pages", func(t *testing.T) {
		repo := tempRepo(t, "https://pager.invalid/hook")
		calls := 0
		notify := func(url string, alert DegradeBreachAlert) error {
			calls++
			return nil
		}
		// rc=2 records status `operator-degraded`, which is NOT a degraded
		// loop-result status: it stops the streak walk.
		docSyncCycle(repo, 0, notify, "operator-diverged")
		docSyncCycle(repo, 1, notify, "operator-diverged")
		if docSyncCycle(repo, 2, notify, "operator-degraded") {
			t.Fatal("paged on operator-degraded row")
		}
		if calls != 0 {
			t.Fatalf("calls = %d", calls)
		}
	})

	t.Run("never pages when the config file is broken", func(t *testing.T) {
		repo := tempRepo(t, "")
		if err := os.WriteFile(filepath.Join(repo, "devagent.json"), []byte("{ not json at all"), 0o644); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < DegradeStreakThreshold; i++ {
			pagerAppendRow(t, repo, pagerLoopResult(145+i, "provider-degraded", pagerTsAt(i)))
		}
		calls := 0
		notify := func(url string, alert DegradeBreachAlert) error {
			calls++
			return nil
		}
		// config.Load errors on the malformed file; the pager swallows it.
		if PageDegradeBreach(PageDegradeBreachArgs{RepoPath: repo, Source: DegradeBreachSourceDocSync, Notify: notify}) {
			t.Fatal("paged on broken config")
		}
		if calls != 0 {
			t.Fatalf("calls = %d", calls)
		}
	})

	t.Run("never pages when the paging transport fails", func(t *testing.T) {
		repo := tempRepo(t, "https://pager.invalid/hook")
		for i := 0; i < DegradeStreakThreshold; i++ {
			pagerAppendRow(t, repo, pagerLoopResult(145+i, "operator-diverged", pagerTsAt(i)))
		}
		notify := func(url string, alert DegradeBreachAlert) error {
			return errors.New("connect ECONNREFUSED 127.0.0.1:443")
		}
		if PageDegradeBreach(PageDegradeBreachArgs{RepoPath: repo, Source: DegradeBreachSourceDocSync, Notify: notify}) {
			t.Fatal("paged despite transport failure")
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
			if docSyncCycle(repo, i, notify, "operator-diverged") {
				t.Fatal("paged without webhook url")
			}
		}
		if calls != 0 {
			t.Fatalf("calls = %d", calls)
		}
	})

	t.Run("declares exactly the two streak surfaces", func(t *testing.T) {
		if len(DegradeBreachSources) != 2 || DegradeBreachSources[0] != DegradeBreachSourcePreflight || DegradeBreachSources[1] != DegradeBreachSourceDocSync {
			t.Fatalf("sources = %v", DegradeBreachSources)
		}
		if !IsDegradeBreachSource("doc-sync") || !IsDegradeBreachSource("preflight") {
			t.Fatal("declared sources rejected")
		}
		if IsDegradeBreachSource("board-archived") {
			t.Fatal("unknown source accepted")
		}
	})

	t.Run("injectable clock sets the alert ts", func(t *testing.T) {
		repo := tempRepo(t, "https://pager.invalid/hook")
		for i := 0; i < DegradeStreakThreshold; i++ {
			pagerAppendRow(t, repo, pagerLoopResult(145+i, "operator-diverged", pagerTsAt(i)))
		}
		var gotTS string
		notify := func(url string, alert DegradeBreachAlert) error {
			gotTS = alert.TS
			return nil
		}
		PageDegradeBreach(PageDegradeBreachArgs{
			RepoPath: repo,
			Source:   DegradeBreachSourceDocSync,
			Notify:   notify,
			Now:      unixMsFunc(1788782400000),
		})
		if gotTS != "2026-09-07T12:00:00.000Z" {
			t.Fatalf("ts = %q", gotTS)
		}
	})
}
