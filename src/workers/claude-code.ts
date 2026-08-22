import type { WorkerAdapter, WorkerEvent, WorkerResult, WorkerSpawnOptions } from '../types.js';
import { spawnCli } from './spawn-utils.js';

/**
 * Adapter over the Claude Code headless CLI:
 *   claude -p <prompt> --output-format json [--max-turns N]
 * stdout is a single JSON object with fields like `result` and `session_id`.
 */
export class ClaudeCodeAdapter implements WorkerAdapter {
  readonly name = 'claude-code' as const;

  async spawn(opts: WorkerSpawnOptions): Promise<WorkerResult> {
    const start = Date.now();
    const args = ['-p', opts.prompt, '--output-format', 'json'];
    if (opts.maxSteps !== undefined) {
      args.push('--max-turns', String(opts.maxSteps));
    }

    const { exitCode, stdout, timedOut } = await spawnCli('claude', args, {
      cwd: opts.cwd,
      timeoutMs: opts.timeoutMs,
      ...(opts.env ? { env: opts.env } : {}),
    });

    if (timedOut) {
      return {
        exitCode,
        events: [],
        resultText: null,
        sessionId: null,
        durationMs: Date.now() - start,
        timedOut: true,
      };
    }

    if (!stdout.trim()) {
      return {
        exitCode,
        events: [],
        resultText: null,
        sessionId: null,
        durationMs: Date.now() - start,
        timedOut: false,
      };
    }

    let parsed: Record<string, unknown> | null = null;
    try {
      const candidate: unknown = JSON.parse(stdout);
      if (candidate !== null && typeof candidate === 'object' && !Array.isArray(candidate)) {
        parsed = candidate as Record<string, unknown>;
      }
    } catch {
      parsed = null;
    }

    const events: WorkerEvent[] =
      parsed !== null ? [{ type: 'result', ...parsed }] : [];
    const resultText =
      exitCode === 0 && parsed !== null && typeof parsed.result === 'string' ? parsed.result : null;
    const sessionId =
      parsed !== null && typeof parsed.session_id === 'string' ? parsed.session_id : null;

    return {
      exitCode,
      events,
      resultText,
      sessionId,
      durationMs: Date.now() - start,
      timedOut: false,
    };
  }
}
