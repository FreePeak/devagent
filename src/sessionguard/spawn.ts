/**
 * Node child_process implementation of the guard's AttemptRunner.
 * Streams claude's stdout/stderr through untouched while parsing
 * stream-json events for session id, retry state, and terminal errors.
 */

import { spawn } from 'node:child_process';
import { createInterface } from 'node:readline';
import type { AttemptResult, LineHandler, SpawnOpts } from './guard.js';
import { parseStreamLine } from './events.js';

const WATCHDOG_POLL_MS = 1_000;

export function spawnClaude(
  argv: string[],
  handler: LineHandler,
  opts: SpawnOpts,
): Promise<AttemptResult> {
  const [file, ...args] = argv;
  if (!file) return Promise.reject(new Error('empty argv'));

  const child = spawn(file, args, {
    stdio: ['inherit', 'pipe', 'pipe'],
    env: opts.env,
  });

  const result: AttemptResult = {
    exitCode: null,
    timedOut: false,
    resultIsError: false,
    sawResult: false,
  };

  let lastProgressAt = Date.now();
  const touch = () => {
    lastProgressAt = Date.now();
  };

  const observe = (event: ReturnType<typeof parseStreamLine>) => {
    if (event.kind === 'init') result.sessionId = event.sessionId;
    else if (event.kind === 'result') {
      result.sawResult = true;
      result.resultIsError = event.isError;
      if (event.sessionId && !result.sessionId) result.sessionId = event.sessionId;
    } else if (event.kind === 'synthetic_error') {
      result.syntheticErrorText = event.text;
    }
  };

  return new Promise((resolve, reject) => {
    const stdoutRl = createInterface({ input: child.stdout! });
    stdoutRl.on('line', (line) => {
      handler.onLine(line, 'stdout');
      touch();
      observe(parseStreamLine(line));
    });

    const stderrChunks: string[] = [];
    createInterface({ input: child.stderr! }).on('line', (line) => {
      handler.onLine(line, 'stderr');
      stderrChunks.push(line);
      touch();
    });

    let watchdog: NodeJS.Timeout | null = null;
    if (opts.noProgressTimeoutMs > 0) {
      watchdog = setInterval(() => {
        if (Date.now() - lastProgressAt >= opts.noProgressTimeoutMs) {
          result.timedOut = true;
          child.kill('SIGKILL');
        }
      }, WATCHDOG_POLL_MS);
      watchdog.unref();
    }

    child.on('error', (err) => {
      if (watchdog) clearInterval(watchdog);
      reject(err);
    });
    child.on('close', (code) => {
      if (watchdog) clearInterval(watchdog);
      result.exitCode = code;
      if (
        !result.syntheticErrorText &&
        code !== 0 &&
        result.timedOut === false &&
        stderrChunks.length > 0 &&
        !result.sessionId
      ) {
        // Launch-level failure before any stream output; surface stderr tail.
        result.syntheticErrorText = stderrChunks.slice(-5).join('\n');
      }
      resolve(result);
    });
  });
}
