// Package spawn is the Go port of the spawn helpers in
// src/workers/spawn-utils.ts — the single place child processes get their
// environment. The nested-env blocklist, the PATH fallback, and the PWD
// sync carry live-incident lessons; FR-GO-05 will extend this package with
// the streaming watchdog variants.
package spawn

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Options mirrors SpawnCliOptions. TimeoutMs is a hard wall clock (SIGKILL).
type Options struct {
	Dir       string
	TimeoutMs int
	// Env is merged over the base environment unless ReplaceEnv.
	Env map[string]string
	// ReplaceEnv makes Env the base environment (worker sandboxing: a
	// scrubbed env cannot unset inherited secrets by merging).
	ReplaceEnv bool
}

// Result mirrors SpawnCliResult. ExitCode -1 means timeout or spawn failure;
// TimedOut distinguishes the two for callers.
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
	TimedOut bool
}

// NESTED_ENV_BLOCKLIST: env vars injected by agent harnesses / CI that
// corrupt nested CLI dispatches (live-smoke lesson: a parent's ANTHROPIC_MODEL
// makes child `claude -p` fail model resolution and emit nothing on stdout).
var NESTED_ENV_BLOCKLIST = []string{"ANTHROPIC_MODEL", "ANTHROPIC_SMALL_FAST_MODEL", "CLAUDE_CODE_ENTRYPOINT", "CLAUDECODE"}

// FALLBACK_PATH_SEGMENTS: fallback PATH segments for children spawned from
// minimal-env contexts (launchd plists without EnvironmentVariables, scrubbed
// worker sandboxes). Without them `git`/`gh`/homebrew tools ENOENT and
// publish stages die with "spawn git ENOENT" (live-smoke lesson 2026-08-25).
var FALLBACK_PATH_SEGMENTS = []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"}

func ensureUsablePath(env map[string]string) {
	p := env["PATH"]
	if strings.Contains(p, "/usr/bin") && strings.Contains(p, "/bin") {
		return
	}
	segments := strings.Split(p, ":")
	var missing []string
	for _, seg := range FALLBACK_PATH_SEGMENTS {
		found := false
		for _, s := range segments {
			if s == seg {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, seg)
		}
	}
	if len(missing) > 0 {
		if p != "" {
			env["PATH"] = p + ":" + strings.Join(missing, ":")
		} else {
			env["PATH"] = strings.Join(missing, ":")
		}
	}
}

// BuildEnv mirrors buildEnv(): blocklist scrub, env merge (or replace), PATH
// fallback, and PWD/OLDPWD sync with the spawn dir (CLIs that trust $PWD over
// getcwd otherwise silently run against the parent's directory — live-smoke
// lesson 2026-08-26).
func BuildEnv(opts Options) map[string]string {
	env := map[string]string{}
	if opts.ReplaceEnv {
		for k, v := range opts.Env {
			env[k] = v
		}
	} else {
		for _, kv := range os.Environ() {
			if i := strings.IndexByte(kv, '='); i > 0 {
				env[kv[:i]] = kv[i+1:]
			}
		}
		for k, v := range opts.Env {
			env[k] = v
		}
	}
	for _, k := range NESTED_ENV_BLOCKLIST {
		delete(env, k)
	}
	ensureUsablePath(env)
	if opts.Dir != "" {
		env["PWD"] = opts.Dir
		delete(env, "OLDPWD")
	}
	return env
}

// RunCli runs a CLI to completion with a hard wall-clock timeout. On timeout
// the child is killed and TimedOut=true is returned (ExitCode -1) instead of
// an error, so callers can map it to their own result shapes. Stdin is closed
// immediately: headless prompts come via argv, and an open stdin makes
// `omp -p` sit in readPipedInput until the pipe closes (2026-09-03).
func RunCli(name string, args []string, opts Options) Result {
	if opts.TimeoutMs <= 0 {
		opts.TimeoutMs = 60_000
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(opts.TimeoutMs)*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = opts.Dir
	cmd.Env = envSlice(BuildEnv(opts))
	// Stdin stays an open pipe that we close immediately (not /dev/null):
	// stdio 'ignore' makes `claude -p` emit empty stdout (live-smoke lesson).
	stdin, err := cmd.StdinPipe()
	if err == nil {
		_ = stdin.Close()
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	timedOut := ctx.Err() == context.DeadlineExceeded
	if timedOut {
		return Result{ExitCode: -1, Stdout: stdout.String(), Stderr: stderr.String(), TimedOut: true}
	}
	exitCode := 0
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1 // spawn failure (ENOENT etc.): never read as success
		}
	}
	return Result{ExitCode: exitCode, Stdout: stdout.String(), Stderr: stderr.String()}
}

func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
