// Package daemon is the Go port of src/server/daemon.ts (FR-GO-12, #200):
// the FR-CTRL control-plane daemon — a loopback-only JSON API over the queue,
// ledger, and board state so the TUI dashboard (FR-TUI) and remote operators
// can observe runs and dispatch work — always through the same `devagent task`
// pipeline the CLI uses (FR-CTRL-03, never a pipeline bypass).
package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/FreePeak/devagent/internal/herdr"
	"github.com/FreePeak/devagent/internal/orchestrator"
	"github.com/FreePeak/devagent/internal/platform"
)

// Capability list and tunables mirror daemon.ts:70-81 byte-for-byte.
var devagentCapabilities = []string{"approve", "dispatch", "attach", "kill-via-answer"}

const (
	// killSentinel is the exact /approve answer that routes into the
	// operator-kill path.
	killSentinel = "__kill__"
	// killMinChildAgeMs is the minimum age of a headless worker child before
	// an operator kill may reap it.
	killMinChildAgeMs = 60_000
	// maxBodyBytes is the 1 MiB request-body cap.
	maxBodyBytes = 1 << 20
	// eventsReplayLines caps the in-memory SSE replay buffer.
	eventsReplayLines = 200
	// eventsPollInterval is the run-log poll cadence.
	eventsPollInterval = 250 * time.Millisecond
	// eventsHeartbeatInterval is the SSE keepalive comment cadence.
	eventsHeartbeatInterval = 15 * time.Second
	// historyDefaultLimit is the /history default page size.
	historyDefaultLimit = 100
	// historyMaxLimit caps /history ?limit.
	historyMaxLimit = 1000
	// runLockTTLms bounds how old a run lock may be before the run is stale.
	runLockTTLms = int64(60 * 60_000)
)

// Options mirrors the TS DaemonOptions. Zero-value fields mean "default".
type Options struct {
	// Port is the TCP port (0 = ephemeral); ignored when UDSPath is set;
	// nil = 7788.
	Port *int
	// RepoPath is the repo the API reads from and dispatches into
	// (default process cwd).
	RepoPath string
	// Token is the bearer token; default DEVAGENT_DAEMON_TOKEN else a fresh
	// random one persisted 0600 to daemon-token.
	Token string
	// UDSPath is a unix-socket path; when set, listens there instead of TCP
	// (filesystem perms are the auth).
	UDSPath string
	// DispatchRunner is a test seam (defaults to DefaultDispatchRunner).
	DispatchRunner func(spec DispatchSpec) DispatchResult
	// AnswerApplier is a test seam (defaults to orchestrator.ApplyAnswerToRepo).
	AnswerApplier func(repoPath string, taskID string, answer string) orchestrator.AnswerEndpointResult
	// ReapStaleWorkers and KillProcessTree are operator-kill seams standing in
	// for the reaper (src/resilience/reaper.ts) whose Go port is FR-GO-05
	// (#190). TODO(FR-GO-05 #190): wire to the Go reaper once it lands; nil
	// seams skip the child-reaping leg of /approve __kill__.
	ReapStaleWorkers func(cwdPrefix string, idleMs int) []int
	KillProcessTree  func(pid int, reason string)
	// HerdrCli is a test seam for the herdr CLI (defaults to herdr.ExecRunner).
	HerdrCli herdr.CliRunner
}

// DispatchSpec mirrors the TS DispatchSpec: the arguments for one dispatched
// run (mirrors `devagent task` flags). TaskID is the run identity the queue
// row, the child's run lock and its dispatch log carry (issue #316).
type DispatchSpec struct {
	TaskID         string
	RepoPath       string
	Prompt         string
	Role           string
	Worker         string
	MaxLoops       *float64
	TimeoutMinutes *float64
	AutoPr         bool
}

// DispatchResult mirrors the TS `{ pid: number | null }` runner result.
type DispatchResult struct {
	PID *int
}

// Handle mirrors DaemonHandle: Stop() tears down every listener and SSE
// client.
type Handle struct {
	// Port is the TCP port when listening on TCP, else nil.
	Port *int
	// UDSPath is the unix-socket path when listening on a socket, else "".
	UDSPath string
	// Token is the effective bearer token (always present; persisted for
	// local clients on either transport).
	Token string

	srv      *http.Server
	follower *runLogFollower
}

// Stop stops the follower and then the HTTP server, unblocking SSE handlers
// via the server shutdown hook (net/http Shutdown alone does not cancel
// active handler contexts).
func (h *Handle) Stop() {
	h.follower.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = h.srv.Shutdown(ctx)
}

// routeContext mirrors RouteContext: everything an endpoint needs.
type routeContext struct {
	repoPath       string
	token          string
	startedAt      time.Time
	follower       *runLogFollower
	dispatchRunner func(spec DispatchSpec) DispatchResult
	answerApplier  func(repoPath string, taskID string, answer string) orchestrator.AnswerEndpointResult
}

// daemon implements http.Handler. It carries the Options-derived seams not
// part of the per-request context (the reaper seams and herdr CLI runner —
// TS reaches the herdr CLI through module singletons).
type daemon struct {
	ctx              routeContext
	reapStaleWorkers func(cwdPrefix string, idleMs int) []int
	killProcessTree  func(pid int, reason string)
	herdrCli         herdr.CliRunner
}

func (d *daemon) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// Mirror route().catch: any endpoint error becomes a 500 with the error
	// message; sendJSON refuses to double-write a spent response.
	res := &response{w: w}
	if err := d.route(res, req); err != nil {
		res.sendJSON(http.StatusInternalServerError, errorBody{OK: false, Note: err.Error()})
	}
}

// Start mirrors startDaemon: bind the listener (TCP 127.0.0.1 by default, or
// a unix socket), start the run-log follower, return the handle.
func Start(opts Options) (*Handle, error) {
	repoPath := opts.RepoPath
	if repoPath == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		repoPath = cwd
	}

	herdrCli := opts.HerdrCli
	if herdrCli == nil {
		herdrCli = herdr.ExecRunner{}
	}
	dispatchRunner := opts.DispatchRunner
	if dispatchRunner == nil {
		dispatchRunner = DefaultDispatchRunner
	}
	answerApplier := opts.AnswerApplier
	if answerApplier == nil {
		answerApplier = orchestrator.ApplyAnswerToRepo
	}

	d := &daemon{
		ctx: routeContext{
			repoPath:  repoPath,
			token:     resolveToken(opts.Token),
			startedAt: time.Now(),
			// SSE sources: the per-run run-log AND the repo orchestration
			// stream the selfbuild loop writes phase/result rows to (without
			// the latter the stream never sees loop progress — the two trees
			// were disjoint).
			follower:       newRunLogFollower(devagentHome(), []string{filepath.Join(repoPath, ".devagent", "runs", "orchestration", "events.jsonl")}),
			dispatchRunner: dispatchRunner,
			answerApplier:  answerApplier,
		},
		reapStaleWorkers: opts.ReapStaleWorkers,
		killProcessTree:  opts.KillProcessTree,
		herdrCli:         herdrCli,
	}

	srv := &http.Server{Handler: d}
	// Shutdown() does not cancel active handler contexts; hooking cancel into
	// RegisterOnShutdown unblocks SSE handlers (blocked on req.Context().Done())
	// so Stop() tears down every listener and SSE client like the TS close().
	baseCtx, cancelBase := context.WithCancel(context.Background())
	srv.BaseContext = func(net.Listener) context.Context { return baseCtx }
	srv.RegisterOnShutdown(cancelBase)

	var ln net.Listener
	if opts.UDSPath != "" {
		var err error
		ln, err = platform.Listen(opts.UDSPath)
		if err != nil {
			return nil, err
		}
	} else {
		port := 7788
		if opts.Port != nil {
			port = *opts.Port
		}
		var err error
		ln, err = net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return nil, err
		}
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	select {
	case err := <-serveErr:
		// Serve returns immediately on listener error.
		_ = ln.Close()
		return nil, err
	default:
	}

	d.ctx.follower.Start()

	handle := &Handle{srv: srv, follower: d.ctx.follower, Token: d.ctx.token}
	if opts.UDSPath == "" {
		if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
			handle.Port = &tcp.Port
		}
	} else {
		handle.UDSPath = opts.UDSPath
	}
	return handle, nil
}

// response wraps the handler writer with the sendJSON writableEnded guard.
type response struct {
	w     http.ResponseWriter
	ended bool
}

// sendJSON mirrors sendJson: serialize the body, stamp status/headers, and
// refuse to double-write a spent response.
func (r *response) sendJSON(status int, body any) {
	if r.ended {
		return
	}
	r.ended = true
	payload, err := json.Marshal(body)
	if err != nil {
		payload = []byte(`{"ok":false,"note":"encoding error"}`)
	}
	h := r.w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(payload)))
	r.w.WriteHeader(status)
	_, _ = r.w.Write(payload)
}

// errorBody is the `{ok:false,note}` JSON error shape.
type errorBody struct {
	OK   bool   `json:"ok"`
	Note string `json:"note"`
}

// okBody is the `{ok:true}` JSON shape (healthz).
type okBody struct {
	OK bool `json:"ok"`
}

// route mirrors route(): path normalization, method dispatch, guards, then
// the endpoint table in daemon.ts order.
func (d *daemon) route(res *response, req *http.Request) error {
	path := strings.TrimRight(req.URL.EscapedPath(), "/")
	if path == "" {
		path = "/"
	}
	method := strings.ToUpper(req.Method)

	// healthz is the unauthenticated liveness probe and sits before every guard.
	if path == "/healthz" && method == "GET" {
		res.sendJSON(http.StatusOK, okBody{OK: true})
		return nil
	}

	if !hostAllowed(req.Host) {
		res.sendJSON(http.StatusForbidden, errorBody{OK: false, Note: "forbidden host"})
		return nil
	}
	if !originAllowed(req.Header.Get("Origin")) {
		res.sendJSON(http.StatusForbidden, errorBody{OK: false, Note: "forbidden origin"})
		return nil
	}
	if !d.auth(req) {
		res.w.Header().Set("WWW-Authenticate", `Bearer realm="devagent-daemon"`)
		res.sendJSON(http.StatusUnauthorized, errorBody{OK: false, Note: "unauthorized"})
		return nil
	}

	switch {
	case path == "/status" && method == "GET":
		return d.statusEndpoint(res)
	case path == "/agents" && method == "GET":
		return d.agentsEndpoint(res)
	case strings.HasPrefix(path, "/agents/") && method == "GET":
		id, err := decodeURIComponent(strings.TrimPrefix(path, "/agents/"))
		if err != nil {
			return err
		}
		return d.agentEndpoint(res, id)
	case path == "/dispatch" && method == "POST":
		return d.dispatchEndpoint(res, req)
	case path == "/approve" && method == "POST":
		return d.approveEndpoint(res, req)
	case path == "/events" && method == "GET":
		return d.eventsEndpoint(res, req)
	case path == "/history" && method == "GET":
		return d.historyEndpoint(res, req)
	case path == "/sessions" && method == "GET":
		return d.sessionsEndpoint(res)
	case strings.HasPrefix(path, "/attach/") && method == "POST":
		id, err := decodeURIComponent(strings.TrimPrefix(path, "/attach/"))
		if err != nil {
			return err
		}
		return d.attachEndpoint(res, id)
	}

	// Known paths with the wrong method are 405; unknown paths are 404.
	// /healthz is NOT in the known set (TS mirrors this: POST /healthz → 404).
	known := path == "/status" || path == "/agents" || path == "/dispatch" ||
		path == "/approve" || path == "/events" || path == "/history" ||
		path == "/sessions" || strings.HasPrefix(path, "/agents/") ||
		strings.HasPrefix(path, "/attach/")
	if known {
		res.sendJSON(http.StatusMethodNotAllowed, errorBody{OK: false, Note: "method not allowed"})
		return nil
	}
	res.sendJSON(http.StatusNotFound, errorBody{OK: false, Note: "not found"})
	return nil
}

// hostAllowed mirrors hostAllowed: loopback hostnames only. The Host header
// is raw (may carry a port); a bracketed IPv6 host is cut at "]" inclusive.
func hostAllowed(host string) bool {
	if host == "" {
		return false
	}
	var bare string
	if strings.HasPrefix(host, "[") {
		end := strings.Index(host, "]")
		if end < 0 {
			return false
		}
		bare = host[:end+1]
	} else {
		bare = strings.SplitN(host, ":", 2)[0]
	}
	switch bare {
	case "127.0.0.1", "localhost", "[::1]":
		return true
	}
	return false
}

// originAllowed mirrors originAllowed: absent Origin is same-origin (allow);
// a present Origin must parse and carry a loopback hostname. WHATWG URL
// keeps IPv6 brackets and lowercases, hence the explicit bracket handling.
func originAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	h := u.Host
	if strings.HasPrefix(h, "[") {
		end := strings.Index(h, "]")
		if end < 0 {
			return false
		}
		h = strings.ToLower(h[:end+1])
	} else {
		h = strings.ToLower(u.Hostname())
	}
	switch h {
	case "127.0.0.1", "localhost", "[::1]":
		return true
	}
	return false
}

// auth mirrors the bearer check with the TS timing-safe comparison.
func (d *daemon) auth(req *http.Request) bool {
	header := req.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	return timingSafeEq(strings.TrimPrefix(header, "Bearer "), d.ctx.token)
}

// timingSafeEq mirrors timingSafeEq: length mismatch rejects first, then a
// constant-time content comparison.
func timingSafeEq(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func inf() float64 {
	return math.Inf(1)
}

// readBody mirrors readBody: accumulate up to MAX_BODY_BYTES; a larger body
// is an error (TS destroys the socket, making the failure a transport error;
// here the route catch surfaces a 500 "body too large").
func readBody(req *http.Request) (string, error) {
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(req.Body, maxBodyBytes+1))
	if err != nil {
		return "", err
	}
	if n > maxBodyBytes {
		return "", errors.New("body too large")
	}
	return buf.String(), nil
}

// resolveToken mirrors resolveToken: explicit option, env, else generate +
// persist (0600) under DEVAGENT_HOME/daemon-token so local TUIs can pick it
// up. An empty-string option/env falls through to generation like the TS
// `if (existing)` truthiness check.
func resolveToken(token string) string {
	if token == "" {
		token = os.Getenv("DEVAGENT_DAEMON_TOKEN")
	}
	if token != "" {
		return token
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand never fails on supported platforms; fall back to a
		// time-derived token rather than panicking the daemon.
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	home := devagentHome()
	if err := os.MkdirAll(home, 0o755); err == nil {
		if err := os.WriteFile(filepath.Join(home, "daemon-token"), []byte(token+"\n"), 0o600); err != nil {
			_ = err // persistence is best-effort; the returned token still authenticates this run
		}
	}
	return token
}

// devagentHome mirrors devagentHome: resolve DEVAGENT_HOME the same way the
// CLI does.
func devagentHome() string {
	if home := os.Getenv("DEVAGENT_HOME"); home != "" {
		return home
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = "."
	}
	return filepath.Join(home, ".devagent")
}

// decodeURIComponent mirrors JS decodeURIComponent via url.PathUnescape. The
// error text differs from the V8 URIError ("URI malformed") — documented
// divergence, visible only as the 500 note for a malformed escape.
func decodeURIComponent(s string) (string, error) {
	out, err := url.PathUnescape(s)
	if err != nil {
		return "", fmt.Errorf("URI malformed: %s", s)
	}
	return out, nil
}

// runBestEffort mirrors a TS `try { ... } catch {}` block around best-effort
// work: a panic aborts the block exactly like a JS throw lands in the empty
// catch.
func runBestEffort(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// jsNumber mirrors JS Number(value) for the JSON types the daemon accepts:
// it returns the numeric value and whether Number.isFinite(value) holds.
// null/undefined → 0 (finite), booleans → 1/0, numbers → themselves,
// strings → JS string parsing (trim, "" → 0, 0x hex, ±Infinity → not
// finite), objects/arrays → NaN (not finite).
func jsNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case nil:
		return 0, true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case float64:
		return n, !isJSNonFinite(n)
	case string:
		return parseJSNumber(n)
	default:
		return 0, false
	}
}

// isJSNonFinite reports whether a float is NaN or ±Infinity.
func isJSNonFinite(f float64) bool {
	return f != f || f > 1.7976931348623157e308 || f < -1.7976931348623157e308
}

// parseJSNumber mirrors JS Number(string): trim the JS whitespace class,
// "" → 0, optional sign, 0x hex literals, decimal floats, and the special
// names; anything else is NaN.
func parseJSNumber(s string) (float64, bool) {
	t := trimJSSpace(s)
	if t == "" {
		return 0, true
	}
	sign := 1.0
	if strings.HasPrefix(t, "+") {
		t = t[1:]
	} else if strings.HasPrefix(t, "-") {
		sign = -1
		t = t[1:]
	}
	if t == "Infinity" {
		return sign * inf(), false
	}
	if len(t) > 2 && t[0] == '0' && (t[1] == 'x' || t[1] == 'X') {
		v, err := strconv.ParseUint(t[2:], 16, 64)
		if err != nil {
			// Hex literals larger than uint64 lose precision in JS but stay
			// finite; the daemon only parses operator-supplied budget knobs,
			// so the uint64 ceiling is an accepted divergence.
			return 0, false
		}
		return sign * float64(v), true
	}
	if strings.ContainsAny(t, "_") {
		return 0, false // JS does not accept numeric separators via Number()
	}
	v, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, false
	}
	if isJSNonFinite(v) {
		return v, false
	}
	return sign * v, true
}

// trimJSSpace mirrors String#trim's whitespace class (Unicode Zs, BOM, and
// the ASCII controls).
func trimJSSpace(s string) string {
	start := 0
	for start < len(s) {
		r, size := utf8.DecodeRuneInString(s[start:])
		if !isJSSpace(r) {
			break
		}
		start += size
	}
	end := len(s)
	for end > start {
		r, size := utf8.DecodeLastRuneInString(s[:end])
		if !isJSSpace(r) {
			break
		}
		end -= size
	}
	return s[start:end]
}

func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x85, 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}
