import { afterEach, describe, expect, it } from 'vitest';
import { mkdtempSync, rmSync, utimesSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  inspectTranscript,
  latestTranscript,
  projectSlug,
} from '../src/sessionguard/transcript.js';

let dir: string;
afterEach(() => {
  if (dir) rmSync(dir, { recursive: true, force: true });
});

describe('inspectTranscript', () => {
  it('flags a session whose last assistant turn is a synthetic API error', () => {
    dir = mkdtempSync(join(tmpdir(), 'cc-t-'));
    const file = join(dir, 's1.jsonl');
    writeFileSync(
      file,
      [
        JSON.stringify({ type: 'user', sessionId: 's1', message: { role: 'user', content: 'go' } }),
        JSON.stringify({ type: 'assistant', sessionId: 's1', timestamp: '2026-08-23T10:00:00Z', message: { role: 'assistant', content: [{ type: 'text', text: 'working...' }] } }),
        JSON.stringify({ type: 'assistant', sessionId: 's1', timestamp: '2026-08-23T10:00:05Z', isApiErrorMessage: true, message: { model: '<synthetic>', content: [{ type: 'text', text: 'API Error: Connection lost mid-response.' }] } }),
      ].join('\n'),
    );
    const status = inspectTranscript(file);
    expect(status.interrupted).toBe(true);
    expect(status.sessionId).toBe('s1');
    expect(status.lastErrorText).toContain('Connection lost');
  });

  it('clears interruption once a real assistant message follows the error', () => {
    dir = mkdtempSync(join(tmpdir(), 'cc-t-'));
    const file = join(dir, 's2.jsonl');
    writeFileSync(
      file,
      [
        JSON.stringify({ type: 'assistant', sessionId: 's2', isApiErrorMessage: true, message: { model: '<synthetic>', content: 'API Error' } }),
        JSON.stringify({ type: 'assistant', sessionId: 's2', message: { role: 'assistant', content: 'recovered' } }),
      ].join('\n'),
    );
    const status = inspectTranscript(file);
    expect(status.interrupted).toBe(false);
    expect(status.lastErrorText).toBeUndefined();
  });

  it('reports healthy sessions as not interrupted and skips malformed lines', () => {
    dir = mkdtempSync(join(tmpdir(), 'cc-t-'));
    const file = join(dir, 's3.jsonl');
    writeFileSync(file, ['not json', '{"type":"user","sessionId":"s3"}'].join('\n'));
    expect(inspectTranscript(file)).toMatchObject({
      interrupted: false,
      sessionId: 's3',
    });
  });
});

describe('latestTranscript + projectSlug', () => {
  it('picks the most recently modified transcript', () => {
    dir = mkdtempSync(join(tmpdir(), 'cc-p-'));
    const old = join(dir, 'a.jsonl');
    const newer = join(dir, 'b.jsonl');
    const note = join(dir, 'notes.txt');
    writeFileSync(old, '{}');
    writeFileSync(newer, '{}');
    writeFileSync(note, 'x');
    const past = Date.now() / 1000 - 3600;
    utimesSync(old, past, past);
    expect(latestTranscript(dir)).toBe(newer);
  });

  it('derives the Claude Code project slug including the leading dash', () => {
    expect(projectSlug('/Users/linh.doan/work/repo')).toBe(
      '-Users-linh-doan-work-repo',
    );
  });
});
