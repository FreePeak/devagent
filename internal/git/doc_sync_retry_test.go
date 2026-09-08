package git

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Issue #239: a transient `git fetch` failure (VN-ISP TLS handshakes to
// github.com, ~10-15%) turned every loop iteration's phase-2 into a
// provider-degraded/no-sync row and wasted the iteration. docFetchOrigin now
// retries transient fetch failures — both shapes (non-zero exit and exit 0
// with "fatal:" on stderr) — across 3 attempts sharing ONE timeoutMs window,
// and fails immediately on non-transient errors. These tests stub the
// docGitFn/docSyncSleep seams so attempt counts, budgets, and final messages
// are deterministic; the real-git end-to-end behavior is covered by the
// SyncWorkSelectionDocs suite in doc_sync_test.go.

// stubDocGit replaces docGitFn for the duration of the test, records the
// per-attempt timeout budget docFetchOrigin passed, and returns the recorded
// call count and budgets via pointer so assertions stay in one place.
func stubDocGit(t *testing.T, fn func(args []string) (string, string, error)) (calls *int, budgets *[]int) {
	t.Helper()
	n := 0
	spent := []int{}
	orig := docGitFn
	docGitFn = func(args []string, _ string, timeoutMs int) (string, string, error) {
		n++
		spent = append(spent, timeoutMs)
		return fn(args)
	}
	t.Cleanup(func() { docGitFn = orig })
	return &n, &spent
}

// noOpDocSyncSleep removes the wall-clock pauses for the duration of the test
// while keeping the production {1s, 3s} ladder under test.
func noOpDocSyncSleep(t *testing.T) {
	t.Helper()
	orig := docSyncSleep
	docSyncSleep = func(time.Duration) {}
	t.Cleanup(func() { docSyncSleep = orig })
}

// TestDocFetchOriginRetriesTransientThenSucceeds: a transient error message
// is retried until success — one failing attempt (2 total) and a
// two-failure case (3 total, the full budget) — across both failure shapes.
func TestDocFetchOriginRetriesTransientThenSucceeds(t *testing.T) {
	noOpDocSyncSleep(t)
	cases := []struct {
		name      string
		msg       string
		exit0     bool // exit 0 with "fatal:"-shaped transient stderr
		failTimes int
	}{
		{name: "resolve-host then success", msg: "fatal: unable to access 'https://github.com/': Could not resolve host: github.com", failTimes: 1},
		{name: "tls-handshake then success", msg: "fatal: unable to access 'https://github.com/': SSL error: handshake timed out", failTimes: 1},
		{name: "early-eof then success", msg: "fatal: the remote end hung up unexpectedly (early EOF)", failTimes: 1},
		{name: "rpc-failed then success", msg: "error: RPC failed; curl 56 OpenSSL SSL_read", failTimes: 1},
		{name: "reset-by-peer twice then success", msg: "fetch failed: connection reset by peer", failTimes: 2},
		{name: "exit0-fatal tls then success", msg: "fatal: unable to access 'https://github.com/': SSL error: handshake timed out", exit0: true, failTimes: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.failTimes + 1
			failTimes := tc.failTimes
			calls, _ := stubDocGit(t, func(args []string) (string, string, error) {
				if strings.Join(args, " ") != "fetch origin main" {
					t.Fatalf("args = %v", args)
				}
				if failTimes > 0 {
					failTimes--
					if tc.exit0 {
						return "", tc.msg, nil
					}
					return "", tc.msg, errors.New("exit status 128")
				}
				return "", "", nil
			})
			stderr, err := docFetchOrigin("main", t.TempDir(), 30_000)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if stderr != "" {
				t.Fatalf("stderr = %q", stderr)
			}
			if got := *calls; got != want {
				t.Fatalf("attempts = %d, want %d", got, want)
			}
		})
	}
}

// TestDocFetchOriginFailsImmediatelyOnNonTransient: auth/not-found failures
// are not retried — attempt 1 fails the call, in both failure shapes.
func TestDocFetchOriginFailsImmediatelyOnNonTransient(t *testing.T) {
	noOpDocSyncSleep(t)
	cases := []struct {
		name  string
		msg   string
		exit0 bool
	}{
		{name: "auth", msg: "fatal: Authentication failed for 'https://github.com/'"},
		{name: "missing-ref", msg: "fatal: couldn't find remote ref refs/heads/nope"},
		{name: "not-found", msg: "fatal: repository 'https://github.com/x/y/' not found"},
		{name: "http-403", msg: "fatal: unable to access 'https://github.com/': The requested URL returned error: 403"},
		{name: "exit0-fatal auth", msg: "fatal: Authentication failed for 'https://github.com/'", exit0: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls, _ := stubDocGit(t, func(args []string) (string, string, error) {
				if tc.exit0 {
					return "", tc.msg, nil
				}
				return "", tc.msg, errors.New("exit status 128")
			})
			stderr, err := docFetchOrigin("main", t.TempDir(), 30_000)
			// Failure contract: non-zero exit (err) OR the exit-0 "fatal:"
			// stderr shape — never a clean (nil, non-fatal-stderr) result.
			if err == nil && !strings.Contains(stderr, "fatal:") {
				t.Fatal("expected failure")
			}
			if got := *calls; got != 1 {
				t.Fatalf("attempts = %d, want 1 (no retry)", got)
			}
		})
	}
}

// TestDocFetchOriginExhaustsRetriesWithSameFinalMessage: a persistent
// transient error burns all 3 attempts and surfaces the same final message.
func TestDocFetchOriginExhaustsRetriesWithSameFinalMessage(t *testing.T) {
	noOpDocSyncSleep(t)
	msg := "fatal: unable to access 'https://github.com/': SSL error: handshake timed out"
	calls, _ := stubDocGit(t, func(args []string) (string, string, error) {
		return "", msg, errors.New("exit status 128")
	})
	stderr, err := docFetchOrigin("main", t.TempDir(), 30_000)
	if err == nil {
		t.Fatal("expected persistent transient failure to fail")
	}
	if got := *calls; got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
	if got := docErrMessage(stderr, err); got != msg {
		t.Fatalf("final message = %q, want %q", got, msg)
	}
}

// TestDocFetchOriginSharedWindow pins the ONE-window budget semantics: each
// attempt gets the remaining budget (so per-attempt budgets shrink), and the
// ladder stops once the window is spent — never arming docGit's 30s default
// via a non-positive timeout.
func TestDocFetchOriginSharedWindow(t *testing.T) {
	cases := []struct {
		name        string
		timeoutMs   int
		ladder      []time.Duration
		sleepPerTry time.Duration // stub-side fetch duration
		wantCalls   int
		wantErr     error // nil = any error accepted
	}{
		{
			// Each fetch consumes its whole remaining budget, so attempt 2
			// finds the window spent: exactly 1 call, budget ≈ 60ms.
			name: "budget exhausted stops the ladder", timeoutMs: 60, ladder: nil,
			sleepPerTry: 60 * time.Millisecond, wantCalls: 1,
		},
		{
			// A 0 budget must never reach docGit (it resets <=0 to 30s): the
			// ladder exits without a call and reports a timeout-shaped error.
			name: "pre-spent window makes no call", timeoutMs: 0, ladder: nil,
			wantCalls: 0, wantErr: context.DeadlineExceeded,
		},
		{
			// The backoff must fit inside the remaining window: with 50ms
			// left and a 1s backoff, attempt 2 is skipped despite budget.
			name: "backoff larger than remaining window skips retry", timeoutMs: 50,
			ladder: []time.Duration{time.Second, 3 * time.Second}, wantCalls: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			noOpDocSyncSleep(t)
			origLadder := docFetchBackoffs
			docFetchBackoffs = tc.ladder
			t.Cleanup(func() { docFetchBackoffs = origLadder })

			calls, budgets := stubDocGit(t, func(args []string) (string, string, error) {
				if tc.sleepPerTry > 0 {
					time.Sleep(tc.sleepPerTry)
				}
				return "", "fatal: unable to access 'https://github.com/': Could not resolve host: github.com", errors.New("exit status 128")
			})
			_, err := docFetchOrigin("main", t.TempDir(), tc.timeoutMs)
			if err == nil {
				t.Fatal("expected failure")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got := *calls; got != tc.wantCalls {
				t.Fatalf("attempts = %d, want %d", got, tc.wantCalls)
			}
			for i, b := range *budgets {
				if b > tc.timeoutMs {
					t.Fatalf("attempt %d budget %dms exceeds window %dms", i+1, b, tc.timeoutMs)
				}
			}
			if len(*budgets) >= 2 && (*budgets)[1] >= (*budgets)[0] {
				t.Fatalf("budgets not shrinking: %v", *budgets)
			}
		})
	}
}

// TestIsTransientFetchError pins the issue's case-insensitive transient
// pattern list against network/TLS messages and auth/not-found messages.
func TestIsTransientFetchError(t *testing.T) {
	cases := []struct {
		msg       string
		transient bool
	}{
		{"fatal: unable to access 'https://github.com/': Could not resolve host: github.com", true},
		{"ssh: connect to host github.com port 22: Connection refused", true},
		{"error: RPC failed; curl 56 OpenSSL SSL_read: Connection was reset, errno 0", true},
		{"fatal: early EOF", true},
		{"fatal: unable to access 'https://github.com/': Failed to connect to github.com port 443: Operation timed out", true},
		{"TLS handshake failed", true},
		{"ssl routines:ssl3_get_record:packet length too large", true},
		{"connection reset by peer", true},
		{"fatal: Authentication failed for 'https://github.com/'", false},
		{"fatal: couldn't find remote ref refs/heads/nope", false},
		{"fatal: repository 'https://github.com/x/y/' not found", false},
		{"error: pathspec 'x' did not match any file(s) known to git", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isTransientFetchError(tc.msg); got != tc.transient {
			t.Fatalf("isTransientFetchError(%q) = %v, want %v", tc.msg, got, tc.transient)
		}
	}
}

// TestSyncWorkSelectionDocsFetchFailureDetailParity: the exhausted-retry path
// keeps the byte-parity failure format ("git fetch failed: <message>") and no
// command runs after the failed fetch.
func TestSyncWorkSelectionDocsFetchFailureDetailParity(t *testing.T) {
	noOpDocSyncSleep(t)
	msg := "fatal: unable to access 'https://github.com/': Could not resolve host: github.com"
	stubDocGit(t, func(args []string) (string, string, error) {
		if len(args) > 0 && args[0] == "fetch" {
			return "", msg, errors.New("exit status 128")
		}
		// Reaching a non-fetch command means the sync wrongly continued past
		// a failed fetch.
		t.Fatalf("unexpected git call after fetch failure: %v", args)
		return "", "", nil
	})
	r := SyncWorkSelectionDocs(t.TempDir(), nil)
	if r.OK {
		t.Fatalf("r = %+v", r)
	}
	if r.Detail != "git fetch failed: "+msg {
		t.Fatalf("detail = %q, want exact byte-parity %q", r.Detail, "git fetch failed: "+msg)
	}
}

// TestDocFetchRetryContract pins the #239 retry contract constants: 3
// attempts, the {1s, 3s} backoff ladder (one entry per retry), and the seam
// wiring — without stubbing anything.
func TestDocFetchRetryContract(t *testing.T) {
	if fetchRetryAttempts != 3 {
		t.Fatalf("fetchRetryAttempts = %d, want 3", fetchRetryAttempts)
	}
	if len(docFetchBackoffs) != fetchRetryAttempts-1 {
		t.Fatalf("backoff ladder = %v, want %d entries", docFetchBackoffs, fetchRetryAttempts-1)
	}
	if docFetchBackoffs[0] != time.Second || docFetchBackoffs[1] != 3*time.Second {
		t.Fatalf("backoff ladder = %v, want [1s 3s]", docFetchBackoffs)
	}
	if docGitFn == nil || docSyncSleep == nil {
		t.Fatal("seams must not be nil")
	}
}
