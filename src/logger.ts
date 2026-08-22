import { appendFileSync, mkdirSync } from 'node:fs';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import type { LogEntry, RunStage } from './types.js';

/**
 * Structured JSONL run logger (FR-OPS-01).
 * One append-only file per run under DEVAGENT_HOME/runs/.
 * Credential values must never reach `data` — callers are responsible; we redact known key names defensively.
 */
export class RunLogger {
  readonly runId = randomUUID();
  private readonly filePath: string;

  constructor(homeDir: string = process.env.DEVAGENT_HOME || join(process.env.HOME || '.', '.devagent')) {
    const runsDir = join(homeDir, 'runs');
    mkdirSync(runsDir, { recursive: true });
    this.filePath = join(runsDir, `${this.runId}.jsonl`);
  }

  get path(): string {
    return this.filePath;
  }

  log(stage: RunStage, level: LogEntry['level'], message: string, data?: Record<string, unknown>): void {
    const entry: LogEntry = {
      ts: new Date().toISOString(),
      runId: this.runId,
      stage,
      level,
      message,
      ...(data ? { data: redact(data) } : {}),
    };
    appendFileSync(this.filePath, `${JSON.stringify(entry)}\n`, 'utf8');
  }

  info(stage: RunStage, message: string, data?: Record<string, unknown>): void {
    this.log(stage, 'info', message, data);
  }

  warn(stage: RunStage, message: string, data?: Record<string, unknown>): void {
    this.log(stage, 'warn', message, data);
  }

  error(stage: RunStage, message: string, data?: Record<string, unknown>): void {
    this.log(stage, 'error', message, data);
  }
}

const SENSITIVE_KEYS = /^(.*?(api[_-]?key|token|secret|password|credential).*?)$/i;

export function redact(data: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(data)) {
    out[k] = SENSITIVE_KEYS.test(k) ? '[REDACTED]' : v;
  }
  return out;
}
