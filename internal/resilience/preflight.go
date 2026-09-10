// Package resilience seeds the Go port of src/resilience/preflight.ts
// (Q40). Only the single probe function lands here with FR-GO-02 — the
// typed gate, ledger rows, circuit, and pager arrive with the orchestrator
// port (FR-GO-07, issue #194).
package resilience

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/FreePeak/devagent/internal/spawn"
)

// One probe outcome.
type Probe struct {
	OK     bool
	Detail string
}

// boundDetail bounds an error excerpt for ledger/log output.
func boundDetail(text string) string {
	return trunc(strings.Join(strings.Fields(text), " "), 200)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// OMPStartupWedgePattern: omp's own startup-watchdog signature
// ("Still starting after <n>s — phase: ...").
var OMPStartupWedgePattern = regexp.MustCompile(`Still starting after \d+s`)

// probeSuccessMarkers are the two answer shapes (mirrors the
// orchestrate-loop probe): `--mode json` (omp) emits the reply inside an
// event stream containing `"text":"OK"`; grok's `--output-format
// streaming-json` emits it as a text chunk — `{"type":"text","data":"OK"}`
// (captured 2026-09-06). Either marker in the streamed stdout completes the
// probe; a probe that ends without either is degraded.
var probeSuccessMarkers = []string{`"text":"OK"`, `"type":"text","data":"OK"`}

func probeStreamCompleted(s string) bool {
	for _, m := range probeSuccessMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// ProbeAPIRetryCapEnv overrides the per-invocation in-process API retry
// cap the probe hands the worker CLI (#306 requirement 3). Unset/invalid =
// probeDefaultAPIRetryCap.
const ProbeAPIRetryCapEnv = "DEVAGENT_PROBE_API_MAX_RETRIES"

// probeDefaultAPIRetryCap bounds the worker's own retries within one probe
// launch at 2 (three requests total): transient failures still get a
// second and third shot inside the wall cap, but a stalled request cannot
// retry itself forever — the user's `~/.omp/agent/config.yml` may carry
// `retry.maxRetries: 999999`, and that file MUST NOT be edited (it drives
// interactive agents). The overlay below applies per probe launch only.
const probeDefaultAPIRetryCap = 2

// writeProbeRetryCapOverlay writes a run-scoped omp config overlay and
// returns its path. omp loads `PI_CONFIG_FILES` with the same
// config.yml-style precedence as its --config flag, so the cap applies to
// this child only. Callers pass it via probeOverlayEnv and remove the file
// when the probe finishes; a write failure only drops the cap (the probe's
// job is measuring the provider, not file IO).
func writeProbeRetryCapOverlay() (string, error) {
	capN := probeDefaultAPIRetryCap
	if v, err := strconv.Atoi(os.Getenv(ProbeAPIRetryCapEnv)); err == nil && v >= 0 {
		capN = v
	}
	f, err := os.CreateTemp("", "devagent-probe-retry-overlay-*.yml")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f,
		"# devagent preflight probe overlay (#306): per-invocation API retry cap.\nretry:\n  maxRetries: %d\n",
		capN); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// probeOverlayEnv merges the overlay into PI_CONFIG_FILES ("" overlay or
// no overlay path = no env). An existing operator value is preserved and
// the overlay is appended — later files win, so the cap lands on top.
func probeOverlayEnv(overlay string) map[string]string {
	if overlay == "" {
		return nil
	}
	v := overlay
	if prev := os.Getenv("PI_CONFIG_FILES"); prev != "" {
		v = prev + string(filepath.ListSeparator) + overlay
	}
	return map[string]string{"PI_CONFIG_FILES": v}
}

// RunPreflightProbe asks the worker CLI to reply to "OK" and require an
// answer, decided on the STREAMED stdout, not process exit (#306).
// Measured 2026-09-11: the answer arrives well before omp finishes
// session/advisor/teardown — at least one sample carried `"text":"OK"` in
// the captured output while the wall cap still fired. So the probe reads
// stdout as it streams, and the moment a success marker appears it kills
// the child's process group and returns ok. The wall cap stays as a
// backstop for the never-answers mode (PreflightProbeTimeoutMs records why
// it must NOT be raised). Each launch also carries the per-invocation
// retry-cap overlay so the CLI's own retry budget cannot loop inside one
// probe. Env hardening comes from internal/spawn (BuildEnv), the same
// spawn path as the worker adapters.
func RunPreflightProbe(cmd string, args []string, dir string, timeoutMs int) Probe {
	if timeoutMs <= 0 {
		timeoutMs = 60_000
	}
	var env map[string]string
	if overlay, err := writeProbeRetryCapOverlay(); err == nil {
		defer os.Remove(overlay)
		env = probeOverlayEnv(overlay)
	}
	spawnOpts := spawn.Options{Dir: dir, TimeoutMs: timeoutMs, Env: env}

	ex := exec.Command(cmd, args...)
	ex.Dir = dir
	ex.Env = spawn.EnvSlice(spawn.BuildEnv(spawnOpts))
	setProbeProcessGroup(ex)
	var stderr bytes.Buffer
	ex.Stderr = &stderr
	// Stdin stays an open pipe that we close immediately (not /dev/null):
	// stdio 'ignore' makes `claude -p` emit empty stdout, and an open stdin
	// makes `omp -p` sit in readPipedInput (spawn.RunCli lessons).
	if stdin, err := ex.StdinPipe(); err == nil {
		_ = stdin.Close()
	}
	stdout, err := ex.StdoutPipe()
	if err != nil {
		// Pipe setup failed before launch: fall back to the buffered path,
		// which waits for exit but still bounds at the cap.
		return probeFromBuffered(spawn.RunCli(cmd, args, spawnOpts))
	}
	if err := ex.Start(); err != nil {
		// Spawn failure (ENOENT etc.): the buffered path's shape was
		// ExitCode -1 with no output — keep the detail identical.
		return Probe{OK: false, Detail: "exit -1"}
	}
	// Wall backstop: kill the whole group when the cap fires; the read
	// below EOFs and Wait returns.
	stopCap := time.AfterFunc(time.Duration(timeoutMs)*time.Millisecond, func() {
		killProbeProcessGroup(ex)
	})
	defer stopCap.Stop()

	var stdoutBuf strings.Builder
	buf := make([]byte, 64*1024)
	answered := false
	for {
		n, rerr := stdout.Read(buf)
		if n > 0 {
			stdoutBuf.Write(buf[:n])
			if probeStreamCompleted(stdoutBuf.String()) {
				answered = true
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	if answered {
		killProbeProcessGroup(ex)
	}
	exitCode := -1
	if werr := ex.Wait(); werr == nil {
		exitCode = 0
	} else if exitErr, ok := werr.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	}
	if answered {
		return Probe{OK: true}
	}
	detail := boundDetail(stderr.String() + "\n" + stdoutBuf.String())
	if detail == "" {
		detail = "exit " + strconv.Itoa(exitCode)
	}
	return Probe{OK: false, Detail: detail}
}

// probeFromBuffered maps a buffered spawn.RunCli result to a Probe — the
// pre-#306 shape (exit 0 + marker in stdout), used only on the pipe-setup
// fallback.
func probeFromBuffered(run spawn.Result) Probe {
	ok := run.ExitCode == 0 && probeStreamCompleted(run.Stdout)
	if ok {
		return Probe{OK: true}
	}
	detail := boundDetail(run.Stderr + "\n" + run.Stdout)
	if detail == "" {
		detail = "exit " + strconv.Itoa(run.ExitCode)
	}
	return Probe{OK: false, Detail: detail}
}
