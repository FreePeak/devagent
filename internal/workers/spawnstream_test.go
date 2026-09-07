// Go port of test/workers/capabilities.test.ts (PRD Q30): the watchdog
// budget precedence the spawn path must keep.
package workers

import (
	"os"
	"testing"
)

const tenMinutesMs = 10 * 60 * 1000

func armedCaps() *WorkerCapabilities {
	return &WorkerCapabilities{DefaultNoProgressTimeoutMs: tenMinutesMs}
}
func offCaps() *WorkerCapabilities {
	return &WorkerCapabilities{DefaultNoProgressTimeoutMs: 0}
}

func intPtr(n int) *int { return &n }

func TestResolveNoProgressTimeoutMs_DeclaredDefault(t *testing.T) {
	t.Setenv("DEVAGENT_NO_PROGRESS_TIMEOUT_MS", "")
	os.Unsetenv("DEVAGENT_NO_PROGRESS_TIMEOUT_MS")
	if got := ResolveNoProgressTimeoutMs(nil, armedCaps()); got != tenMinutesMs {
		t.Fatalf("armed default = %d, want %d", got, tenMinutesMs)
	}
	if got := ResolveNoProgressTimeoutMs(nil, offCaps()); got != 0 {
		t.Fatalf("off default = %d, want 0", got)
	}
	if got := ResolveNoProgressTimeoutMs(nil, nil); got != 0 {
		t.Fatalf("no capabilities = %d, want 0", got)
	}
}

func TestResolveNoProgressTimeoutMs_EnvDefaultOverridesDeclaration(t *testing.T) {
	t.Setenv("DEVAGENT_NO_PROGRESS_TIMEOUT_MS", "120000")
	if got := ResolveNoProgressTimeoutMs(nil, armedCaps()); got != 120_000 {
		t.Fatalf("armed with env = %d, want 120000", got)
	}
	if got := ResolveNoProgressTimeoutMs(nil, offCaps()); got != 120_000 {
		t.Fatalf("off with env = %d, want 120000", got)
	}
}

func TestResolveNoProgressTimeoutMs_IgnoresBadEnv(t *testing.T) {
	for _, raw := range []string{"", "0", "-5", "not-a-number", "Infinity"} {
		t.Setenv("DEVAGENT_NO_PROGRESS_TIMEOUT_MS", raw)
		if got := ResolveNoProgressTimeoutMs(nil, armedCaps()); got != tenMinutesMs {
			t.Fatalf("armed with env %q = %d, want %d", raw, got, tenMinutesMs)
		}
		if got := ResolveNoProgressTimeoutMs(nil, offCaps()); got != 0 {
			t.Fatalf("off with env %q = %d, want 0", raw, got)
		}
	}
}

func TestResolveNoProgressTimeoutMs_ExplicitOverride(t *testing.T) {
	t.Setenv("DEVAGENT_NO_PROGRESS_TIMEOUT_MS", "120000")
	// A nonzero declaration is a floor for adapters that must stay watched.
	os.Unsetenv("DEVAGENT_NO_PROGRESS_TIMEOUT_MS")
	if got := ResolveNoProgressTimeoutMs(intPtr(0), armedCaps()); got != tenMinutesMs {
		t.Fatalf("explicit 0 vs armed = %d, want %d", got, tenMinutesMs)
	}
	// 0 = watchdog off for an adapter that declares 0.
	if got := ResolveNoProgressTimeoutMs(intPtr(0), offCaps()); got != 0 {
		t.Fatalf("explicit 0 vs off = %d, want 0", got)
	}
	// A nonzero declaration is a floor for adapters that must stay watched.
	if got := ResolveNoProgressTimeoutMs(intPtr(0), armedCaps()); got != tenMinutesMs {
		t.Fatalf("explicit 0 vs armed = %d, want %d", got, tenMinutesMs)
	}
	t.Setenv("DEVAGENT_NO_PROGRESS_TIMEOUT_MS", "120000")
	if got := ResolveNoProgressTimeoutMs(intPtr(0), armedCaps()); got != 120_000 {
		t.Fatalf("explicit 0 vs armed+env = %d, want 120000", got)
	}
}

// Adapter capability declarations pinned to the values each adapter
// resolved pre-Q30 (test/workers/capabilities.test.ts registry block).
func TestAdapterCapabilityDeclarations(t *testing.T) {
	adapter, _ := GetWorker("claude-code")
	if caps := adapter.Capabilities().DefaultNoProgressTimeoutMs; caps != 0 {
		t.Fatalf("claude-code default = %d, want 0", caps)
	}
	adapter, _ = GetWorker("opencode")
	if caps := adapter.Capabilities().DefaultNoProgressTimeoutMs; caps != 0 {
		t.Fatalf("opencode default = %d, want 0", caps)
	}
	for _, name := range []string{"omp", "pi", "grok"} {
		adapter, err := GetWorker(name)
		if err != nil {
			t.Fatalf("GetWorker(%q): %v", name, err)
		}
		if caps := adapter.Capabilities().DefaultNoProgressTimeoutMs; caps != tenMinutesMs {
			t.Fatalf("%s default = %d, want %d", name, caps, tenMinutesMs)
		}
	}
}
