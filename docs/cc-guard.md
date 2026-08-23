# cc-guard: auto-resume for headless Claude Code sessions

`devagent guard` wraps a headless (`-p`) Claude Code invocation and restarts
the same persisted session when an API failure kills the turn.

## Why it exists

Claude Code retries transient failures before a response starts streaming,
but never retries a response that dies **mid-stream**
("API Error: Connection lost mid-response") — partial output is already
committed to the transcript, so the harness keeps it and ends the turn.
The documented recovery is starting a new turn in the same session via
`claude --resume <session-id>`. cc-guard automates exactly that, using the
structured `--output-format stream-json` events (`system/init`,
`system/api_retry`, `result.is_error`) rather than brittle stdout string
matching.

## Usage

```sh
# basic
npx tsx src/cli.ts guard -- claude -p "refactor module X"

# tuned: up to 8 launches, 5s first backoff, kill if silent for 10 minutes
npx tsx src/cli.ts guard \
  --max-attempts 8 \
  --base-delay-ms 5000 \
  --no-progress-timeout-ms 600000 \
  -- claude -p "long running task"
```

Everything after `--` is the claude invocation. Guard flags must come before
the `--`. On failure of attempt N, guard re-launches:

```
claude <original flags without the prompt> --resume <sessionId> -p "<resume-prompt>"
```

so the model continues from the persisted transcript instead of repeating
work. Output streams through untouched; guard diagnostics go to stderr.

## Options

| Flag | Default | Meaning |
| --- | --- | --- |
| `--resume-prompt <text>` | `Continue` | prompt sent when resuming |
| `--max-attempts <n>` | `5` | total launches including the first |
| `--base-delay-ms <n>` | `2000` | first backoff delay (exponential, +/-25% jitter) |
| `--max-delay-ms <n>` | `60000` | backoff ceiling |
| `--no-progress-timeout-ms <n>` | `0` (off) | kill + retry when the child streams nothing this long |

## Checking interactive sessions

`devagent guard-status` inspects the latest transcript for the current
project and reports whether its last assistant turn died on an API error
(persisted as a synthetic `<synthetic>` message with `isApiErrorMessage`):

```sh
npx tsx src/cli.ts guard-status
# INTERRUPTED session <id> (...)
# resume with: claude --resume <id>

# detect AND recover in one step (runs devagent guard against the session)
npx tsx src/cli.ts guard-status --resume
```

Exit code 1 means interrupted (and still interrupted after `--resume`
fails), so it can gate scripts or shell prompts.

## Non-retryable errors

Auth and billing failures never succeed on retry. If the synthetic error
text matches `invalid api key`, `authentication`, `unauthorized`,
`credit balance`, `billing`, or `model not found`, cc-guard gives up
immediately with exit code 1.

## Related Claude Code settings (verified against official docs)

| Variable | Fact |
| --- | --- |
| `CLAUDE_CODE_MAX_RETRIES` | default 10, **hard-capped at 15** — values like `10000` silently clamp |
| `CLAUDE_CODE_RETRY_WATCHDOG=1` | unattended mode: indefinite 429/529 retries, ~300 transient retries (~3h budget) |
| `API_TIMEOUT_MS` | per-request timeout, default 600000 |

Mid-stream drops are terminal regardless of these settings; cc-guard is the
recovery layer on top.
