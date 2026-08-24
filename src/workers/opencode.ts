import type { WorkerAdapter, WorkerEvent, WorkerResult, WorkerSpawnOptions } from '../types.js';
import { spawnCli } from './spawn-utils.js';
import { prepareWorkerSpawn } from './sandbox.js';

/**
 * Adapter over the OpenCode headless CLI:
 *   opencode run --format json <prompt>
 * stdout is NDJSON: one JSON object per line, possibly interleaved with garbage.
 */
export class OpenCodeAdapter implements WorkerAdapter {
  readonly name = 'opencode' as const;

  async spawn(opts: WorkerSpawnOptions): Promise<WorkerResult> {
    const start = Date.now();
    const prepared = await prepareWorkerSpawn('opencode', ['run', '--format', 'json', opts.prompt], {
      cwd: opts.cwd,
      timeoutMs: opts.timeoutMs,
      ...(opts.env ? { env: opts.env } : {}),
    });
    const { exitCode, stdout, timedOut } = await spawnCli(prepared.cmd, prepared.args, prepared.opts);

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

    const events: WorkerEvent[] = [];
    let sessionId: string | null = null;

    for (const line of stdout.split('\n')) {
      const trimmed = line.trim();
      if (!trimmed) continue;
      let obj: unknown;
      try {
        obj = JSON.parse(trimmed);
      } catch {
        continue; // skip non-JSON garbage lines
      }
      if (obj === null || typeof obj !== 'object' || Array.isArray(obj)) continue;
      const record = obj as Record<string, unknown>;
      events.push({ type: typeof record.type === 'string' ? record.type : 'event', ...record });

      const sid =
        typeof record.sessionID === 'string'
          ? record.sessionID
          : typeof record.session_id === 'string'
            ? record.session_id
            : null;
      if (sid !== null) sessionId = sid;
    }

    // resultText comes from the last event that carries a text/part string field.
    let resultText: string | null = null;
    for (const event of events) {
      for (const key of ['text', 'part']) {
        const value = event[key];
        if (typeof value === 'string') {
          resultText = value;
          break;
        }
      }
    }

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
