// actions_serve.go ports the `serve` command (src/cli.ts, FR-TICKET-04): the
// webhook receiver HTTP server with two routes — POST /webhooks/linear
// (signature-verified Linear/GitHub dispatch) and POST /api/answer
// (human-in-the-loop resume, token-gated).
//
// Routing is a single HandlerFunc, NOT http.ServeMux: the TS checks
// `req.url?.startsWith(...)` on the raw URL, and ServeMux would normalize
// paths and redirect — a behavioral divergence.

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/daemon"
	"github.com/FreePeak/devagent/internal/integrations"
	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/spf13/cobra"
)

// answerBodyLimit mirrors the TS body bound (1_000_000 chars; raw chunks are
// Buffers so the effective bound is bytes).
const answerBodyLimit = 1_000_000

// serveCommand builds the `serve` cobra command. The coordinator wires it
// into root.go (replacing the stub at that site).
func serveCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the webhook receiver HTTP server (FR-TICKET-04)",
		RunE: func(cmd *cobra.Command, args []string) error {
			port, _ := cmd.Flags().GetInt("port")
			repo, _ := cmd.Flags().GetString("repo")
			if repo == "" {
				repo, _ = os.Getwd() // TS default: process.cwd()
			}
			creds := config.LoadCredentials()
			if os.Getenv("LINEAR_WEBHOOK_SECRET") == "" {
				fmt.Fprintln(os.Stderr, "LINEAR_WEBHOOK_SECRET is not set.")
				os.Exit(1)
			}
			if creds.LinearAPIKey == "" {
				fmt.Fprintln(os.Stderr, "LINEAR_API_KEY is not set.")
				os.Exit(1)
			}
			// Load-and-validate the effective config exactly like TS
			// loadConfig(opts.repo); the dispatch pipeline that would consume
			// it is not yet ported (see serveOnEvent).
			if _, err := config.Load(repo); err != nil {
				return err
			}
			dedup := integrations.NewDeliveryDedup(10000) // TS DeliveryDedup default

			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Human-in-the-loop resume endpoint (LangGraph
				// interrupt/resume pattern: observation via devagent_board,
				// decisions POSTed here). Token-gated.
				uri := r.URL.RequestURI() // req.url includes the query string
				if r.Method == http.MethodPost && strings.HasPrefix(uri, "/api/answer") {
					serveAnswerRoute(w, r)
					return
				}
				if r.Method != http.MethodPost || !strings.HasPrefix(uri, "/webhooks/linear") {
					w.WriteHeader(http.StatusNotFound) // res.end(): empty body
					return
				}
				integrations.HandleWebhook(w, r, integrations.WebhookOptions{
					SigningSecret: os.Getenv("LINEAR_WEBHOOK_SECRET"),
					Dedup:         dedup,
					OnEvent:       serveOnEvent,
				})
			})

			// server.listen(port, cb): the banner prints only after the
			// socket is bound, so bind first.
			addr := fmt.Sprintf(":%d", port)
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return err // TS: unhandled 'error' event exits nonzero
			}
			fmt.Printf("Listening on :%d — POST /webhooks/linear, POST /api/answer (Bearer DEVAGENT_ANSWER_TOKEN)\n", port)
			return http.Serve(ln, handler)
		},
	}
	cmd.Flags().Int("port", 8080, "listen port")
	cmd.Flags().String("repo", "", "target repository for dispatched runs")
	return cmd
}

// serveAnswerRoute ports the POST /api/answer branch: Bearer-token gate, 1 MiB
// body bound, strict {repoPath, taskId, answer} string check, then
// applyAnswerToRepo (answer_glue.go).
func serveAnswerRoute(w http.ResponseWriter, r *http.Request) {
	token := os.Getenv("DEVAGENT_ANSWER_TOKEN")
	if token == "" || r.Header.Get("Authorization") != "Bearer "+token {
		// 503 when unconfigured: the service refuses to run approvals open
		code, note := http.StatusUnauthorized, "unauthorized"
		if token == "" {
			code, note = http.StatusServiceUnavailable, "DEVAGENT_ANSWER_TOKEN not configured"
		}
		writeAnswerJSON(w, code, answerBody{OK: false, Note: note})
		return
	}
	// Bound the body like the TS `raw.length > 1_000_000 -> req.destroy()`:
	// read one extra byte to detect overflow, then tear the connection down
	// without a response (the TS client sees the socket reset).
	raw, err := io.ReadAll(io.LimitReader(r.Body, int64(answerBodyLimit)+1))
	if err != nil {
		writeAnswerJSON(w, http.StatusBadRequest, answerBody{OK: false, Note: "invalid JSON body"})
		return
	}
	if len(raw) > answerBodyLimit {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, hjErr := hj.Hijack(); hjErr == nil {
				_ = conn.Close()
				return
			}
		}
		// Hijack unavailable (e.g. wrapped writer): closest fallback to
		// req.destroy() is an empty 413.
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}

	// Decode, then type-check each field separately: TS distinguishes
	// JSON.parse failure ("invalid JSON body") from a typeof failure
	// ("expected ... strings"), and json.Unmarshal into typed string fields
	// would conflate the two (wrong-type numbers error the Unmarshal).
	var parsed struct {
		RepoPath json.RawMessage `json:"repoPath"`
		TaskID   json.RawMessage `json:"taskId"`
		Answer   json.RawMessage `json:"answer"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		// TS sends no content-type on these 400 branches; net/http sniffs a
		// text/plain Content-Type for us — accepted divergence.
		writeAnswerRaw(w, http.StatusBadRequest, marshalCompact(answerBody{OK: false, Note: "invalid JSON body"}))
		return
	}
	if !isJSONString(parsed.RepoPath) || !isJSONString(parsed.TaskID) || !isJSONString(parsed.Answer) {
		writeAnswerRaw(w, http.StatusBadRequest, marshalCompact(answerBody{OK: false, Note: "expected {repoPath, taskId, answer} strings"}))
		return
	}
	var repoPath, taskID, answer string
	// Validated as JSON strings above; these cannot fail.
	_ = json.Unmarshal(parsed.RepoPath, &repoPath)
	_ = json.Unmarshal(parsed.TaskID, &taskID)
	_ = json.Unmarshal(parsed.Answer, &answer)

	status, body := applyAnswerToRepo(repoPath, taskID, answer)
	writeAnswerJSON(w, status, body)
}

// isJSONString reports whether raw is a JSON string value (missing keys are
// nil RawMessage -> false, matching TS `typeof undefined !== 'string'`).
func isJSONString(raw json.RawMessage) bool {
	return len(raw) > 0 && raw[0] == '"'
}

// writeAnswerJSON writes a {ok,note} body with the explicit application/json
// header the TS sets on the token-fail and final-answer branches.
func writeAnswerJSON(w http.ResponseWriter, status int, body answerBody) {
	w.Header().Set("Content-Type", "application/json")
	writeAnswerRaw(w, status, marshalCompact(body))
}

// writeAnswerRaw writes a pre-serialized compact body (JSON.stringify has no
// trailing newline).
func writeAnswerRaw(w http.ResponseWriter, status int, body []byte) {
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// serveOnEvent ports the serve onEvent handler plus the observable prefix of
// dispatchRun (src/cli.ts): parse the dispatch, acquire the latest-wins run
// lock, and open the run log.
//
// REQUIRED DEVIATION (FR-GO-07 #194): TS dispatchRun then fires runPipeline —
// not yet ported to Go. Everything up to that call is ported faithfully
// (ticket resolution, lock acquisition, duplicate-trigger message, run
// logger); the pipeline call itself is a loud stderr stub so an operator
// never mistakes acceptance for dispatch.
func serveOnEvent(v integrations.VerifiedWebhook) {
	linearDispatch := integrations.ParseAgentSessionEvent(v.Payload)
	var githubDispatch *integrations.GithubIssueDispatch
	if linearDispatch == nil {
		githubDispatch = integrations.ParseGithubIssueEvent(v.GithubEvent, v.Payload)
	}
	ticketID := ""
	if linearDispatch != nil {
		ticketID = linearDispatch.IssueIdentifier
	} else if githubDispatch != nil {
		ticketID = githubDispatch.IssueIdentifier
	}
	if ticketID == "" {
		return // not an event we act on
	}

	// Latest-wins dedup: skip if a run for this ticket is already active.
	home := devagentHome() // TS `DEVAGENT_HOME || join(HOME||'.', '.devagent')`
	lock := ledger.TryAcquireRun(home, ticketID, 0)
	if lock == nil {
		fmt.Printf("Run for %s already active; skipping duplicate trigger\n", ticketID)
		return
	}
	logger, err := ledger.NewRunLogger(home)
	if err != nil {
		return // TS: dispatchRun failure is swallowed by `.catch(() => {})`
	}
	logger.Info(ledger.StageFetch, fmt.Sprintf("Webhook-dispatched run %s starting", logger.RunID()), []ledger.KV{{Key: "ticket", Value: ticketID}})
	fmt.Fprintln(os.Stderr, "[serve] run pipeline not yet ported to Go (FR-GO-07 #194) — webhook accepted, no run dispatched")
}

// newDaemonCmd wires the FR-CTRL control-plane daemon (FR-GO-12, #200) to
// the CLI: HTTP+SSE on 127.0.0.1 (UDS with --uds-path), the FR-GO-15
// cutover entrypoint for LaunchAgents/launchers. Mirrors the Node
// `daemon` command flags (src/cli.ts:1749-1767, frozen surface).
func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the FR-CTRL control-plane daemon (HTTP+SSE on 127.0.0.1; UDS with --uds-path)",
		RunE: func(cmd *cobra.Command, args []string) error {
			portFlag, _ := cmd.Flags().GetInt("port")
			repoFlag, _ := cmd.Flags().GetString("repo")
			udsFlag, _ := cmd.Flags().GetString("uds-path")
			tokenFlag, _ := cmd.Flags().GetString("token")
			if repoFlag == "" {
				repoFlag, _ = os.Getwd()
			}
			opts := daemon.Options{RepoPath: repoFlag}
			port := portFlag
			opts.Port = &port
			if udsFlag != "" {
				opts.UDSPath = udsFlag
			}
			if tokenFlag != "" {
				opts.Token = tokenFlag
			}
			handle, err := daemon.Start(opts)
			if err != nil {
				return err
			}
			where := handle.UDSPath
			if where == "" {
				where = fmt.Sprintf("http://127.0.0.1:%d", *handle.Port)
			}
			fmt.Printf("devagent daemon listening on %s (token: %s)\n", where, handle.Token)
			// Foreground service: block until signalled, then shut down
			// cleanly (SSE clients unblocked via RegisterOnShutdown).
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
			<-sig
			handle.Stop()
			return nil
		},
	}
	cmd.Flags().Int("port", 7788, "TCP port (0 = ephemeral)")
	cmd.Flags().String("repo", "", "repo the API reads from and dispatches into")
	cmd.Flags().String("uds-path", "", "listen on a Unix-domain socket instead of TCP")
	cmd.Flags().String("token", "", "bearer token (default DEVAGENT_DAEMON_TOKEN or a fresh one persisted to daemon-token)")
	return cmd
}
