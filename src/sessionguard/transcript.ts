/**
 * Transcript inspection: detect sessions whose last assistant turn died on
 * an API error. Interrupted turns persist as synthetic assistant messages
 * (`isApiErrorMessage: true`) with no successful assistant message after
 * them. Resume such a session with `claude --resume <sessionId>`.
 */

import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';

export interface TranscriptStatus {
  file: string;
  sessionId?: string;
  interrupted: boolean;
  lastErrorText?: string;
  lastTimestamp?: string;
}

interface TranscriptLine {
  type?: string;
  sessionId?: string;
  session_id?: string;
  timestamp?: string;
  isApiErrorMessage?: boolean;
  message?: { model?: string; content?: unknown };
}

export function inspectTranscript(path: string): TranscriptStatus {
  const raw = readFileSync(path, 'utf8');
  const status: TranscriptStatus = { file: path, interrupted: false };

  for (const line of raw.split('\n')) {
    const trimmed = line.trim();
    if (!trimmed.startsWith('{')) continue;
    let entry: TranscriptLine;
    try {
      entry = JSON.parse(trimmed) as TranscriptLine;
    } catch {
      continue;
    }
    if (!status.sessionId) {
      status.sessionId = entry.sessionId ?? entry.session_id;
    }
    if (entry.timestamp) status.lastTimestamp = entry.timestamp;
    if (entry.type === 'assistant') {
      if (entry.isApiErrorMessage === true || entry.message?.model === '<synthetic>') {
        status.interrupted = true;
        status.lastErrorText = extractText(entry.message?.content);
      } else {
        // A real assistant response after the failure means the session
        // already continued past it.
        status.interrupted = false;
        status.lastErrorText = undefined;
      }
    }
  }
  return status;
}

function extractText(content: unknown): string | undefined {
  if (typeof content === 'string') return content;
  if (Array.isArray(content)) {
    const text = content
      .map((block) =>
        typeof block === 'object' && block !== null && 'text' in block
          ? String((block as { text: unknown }).text)
          : '',
      )
      .filter(Boolean)
      .join(' ');
    return text || undefined;
  }
  return undefined;
}

/** Latest transcript for a project slug dir (e.g. ~/.claude/projects/<slug>). */
export function latestTranscript(projectDir: string): string | undefined {
  let newest: { file: string; mtime: number } | undefined;
  for (const name of readdirSync(projectDir)) {
    if (!name.endsWith('.jsonl')) continue;
    const file = join(projectDir, name);
    const mtime = statSync(file).mtimeMs;
    if (!newest || mtime > newest.mtime) newest = { file, mtime };
  }
  return newest?.file;
}

/** Default projects directory honoring CLAUDE_CONFIG_DIR like Claude Code does. */
export function claudeProjectsDir(home: string): string {
  const configDir = process.env.CLAUDE_CONFIG_DIR ?? join(home, '.claude');
  return join(configDir, 'projects');
}

/** Encode a working directory into a project slug the way Claude Code does. */
export function projectSlug(cwd: string): string {
  return cwd.replace(/[/.]/g, '-');
}
