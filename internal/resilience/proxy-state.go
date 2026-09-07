// Package file mirrors src/resilience/proxy-state.ts (FR-GO-07, issue
// #194): durable provider/proxy health state (operator observability for
// the orchestrate-loop proxy gate), repo-scoped under
// `<repo>/.devagent/proxy-state.json` (like the scout heartbeat) so parallel
// loops against different repos never collide.
//
// Circuit model (classic breaker, probe = trial):
//
//	closed    — healthy; probes pass
//	open      — proxy gate failed (all 3 probes failed); work is skipped
//	half-open — first probe passed after an outage; recovery trial in flight

package resilience

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/FreePeak/devagent/internal/ledger"
)

// CircuitState is the circuit state ("closed" | "half-open" | "open").
type CircuitState string

const (
	CircuitClosed   CircuitState = "closed"
	CircuitHalfOpen CircuitState = "half-open"
	CircuitOpen     CircuitState = "open"
)

// ProxyProbeRecord is one probe outcome on the state file.
type ProxyProbeRecord struct {
	// Result of the proxy-probe gate (true = response with "result" field).
	OK bool `json:"ok"`
	// When the probe ran.
	At string `json:"at"`
	// Extra detail (attempt count, fail summary).
	Detail *string `json:"detail,omitempty"`
}

// TransientRecord is one classified transient on the state file.
type TransientRecord struct {
	// Coarse class label from src/resilience/classify.ts
	// (transientErrorClass).
	Class string `json:"class"`
	// When the transient was classified.
	At string `json:"at"`
	// Bounded excerpt of the classified error text.
	Excerpt string `json:"excerpt"`
}

// ProxyState is the durable state file shape.
type ProxyState struct {
	// Current proxy circuit state.
	Circuit CircuitState `json:"circuit"`
	// When the circuit last transitioned.
	CircuitChangedAt string            `json:"circuitChangedAt"`
	LastProbe        *ProxyProbeRecord `json:"lastProbe,omitempty"`
	LastTransient    *TransientRecord  `json:"lastTransient,omitempty"`
	UpdatedAt        string            `json:"updatedAt"`
}

// ProxyProbe is the input shape of RecordProxyProbe (TS { ok, detail? }).
type ProxyProbe struct {
	OK     bool
	Detail string
}

// ProxyStatePath returns the repo-scoped state file path.
func ProxyStatePath(repoPath string) string {
	return filepath.Join(repoPath, ".devagent", "proxy-state.json")
}

// ReadProxyState reads the state file; nil when absent or unparseable (TS
// returns null).
func ReadProxyState(repoPath string) *ProxyState {
	raw, err := os.ReadFile(ProxyStatePath(repoPath))
	if err != nil {
		return nil
	}
	var state ProxyState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil
	}
	if state.Circuit == "" {
		return nil
	}
	return &state
}

// writeState writes the state file (2-space indent + trailing newline,
// matching TS JSON.stringify(state, null, 2) + '\n').
func writeState(repoPath string, state ProxyState) error {
	if err := os.MkdirAll(filepath.Join(repoPath, ".devagent"), 0o755); err != nil {
		return err
	}
	out, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ProxyStatePath(repoPath), append(out, '\n'), 0o644)
}

// RecordProxyProbe records one proxy-probe outcome and advances the circuit:
//
//	fail       → open
//	ok + open  → half-open (first recovery probe)
//	ok         → closed (otherwise)
//
// When state is absent it is created as closed. Best-effort: never returns
// an error (observability must not break the gate).
func RecordProxyProbe(repoPath string, probe ProxyProbe) ProxyState {
	prev := ReadProxyState(repoPath)
	at := ledger.NowISO()
	var circuit CircuitState
	if probe.OK {
		if prev != nil && prev.Circuit == CircuitOpen {
			circuit = CircuitHalfOpen
		} else {
			circuit = CircuitClosed
		}
	} else {
		circuit = CircuitOpen
	}
	changedAt := at
	if prev != nil && prev.Circuit == circuit {
		changedAt = prev.CircuitChangedAt
	}
	next := ProxyState{
		Circuit:          circuit,
		CircuitChangedAt: changedAt,
		UpdatedAt:        at,
	}
	if prev != nil && prev.LastTransient != nil {
		next.LastTransient = prev.LastTransient
	}
	if probe.Detail != "" {
		d := probe.Detail
		next.LastProbe = &ProxyProbeRecord{OK: probe.OK, At: at, Detail: &d}
	} else {
		next.LastProbe = &ProxyProbeRecord{OK: probe.OK, At: at}
	}
	_ = writeState(repoPath, next)
	return next
}

// RecordTransientClass classifies an error text and, when it is a transient
// provider error, records the coarse class + bounded excerpt as
// lastTransient. Returns the record or nil when the text is not transient
// (nothing is written). Best-effort.
func RecordTransientClass(repoPath string, text string) *TransientRecord {
	cls := TransientErrorClass(&text)
	if cls == "" {
		return nil
	}
	prev := ReadProxyState(repoPath)
	at := ledger.NowISO()
	record := &TransientRecord{
		Class:   cls,
		At:      at,
		Excerpt: trunc(collapseWhitespace(text), 200),
	}
	next := ProxyState{
		Circuit:   CircuitClosed,
		UpdatedAt: at,
	}
	if prev != nil {
		if prev.Circuit != "" {
			next.Circuit = prev.Circuit
		}
		next.CircuitChangedAt = prev.CircuitChangedAt
		if prev.LastProbe != nil {
			next.LastProbe = prev.LastProbe
		}
	}
	if next.CircuitChangedAt == "" {
		next.CircuitChangedAt = at
	}
	next.LastTransient = record
	_ = writeState(repoPath, next)
	return record
}

// collapseWhitespace mirrors TS text.replace(/\s+/g, ' ').
func collapseWhitespace(s string) string {
	out := make([]byte, 0, len(s))
	inSpace := false
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '\v', '\f', 0x85, 0xA0:
			inSpace = true
		default:
			if inSpace && len(out) > 0 {
				out = append(out, ' ')
			}
			inSpace = false
			out = append(out, string(r)...)
		}
	}
	return string(out)
}
