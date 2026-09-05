import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { enqueueTask, taskCount, listTasks } from '../src/queue.js';
import { renderQueueCard } from '../src/commands/human-card.js';

/** Strip ANSI escapes so chip/box contiguity can be asserted on visible text. */
function plain(s: string): string {
  return s.replace(/\x1b\[[0-9;]*m/g, '');
}

let repoPath: string;

beforeEach(() => {
  repoPath = mkdtempSync(join(tmpdir(), 'devagent-queue-human-'));
});

afterEach(() => {
  rmSync(repoPath, { recursive: true, force: true });
});

describe('queue list human card (#144 R3)', () => {
  it('empty queue card shows counts and one next action pointing at status', () => {
    const card = plain(renderQueueCard([], taskCount(repoPath)).join('\n'));
    expect(card).toContain('╭─ Queue');
    expect(card).toContain('pending 0');
    expect(card).toContain('next:');
    expect(card).toContain('devagent status');
    expect(card).toContain('╰');
    expect(card.split('\n').filter((l) => l.includes('next:')).length).toBe(1);
  });

  it('pending tasks suggest consume --auto-pr as the next action', () => {
    enqueueTask(repoPath, { id: 'T1', title: 'research', goal: 'do the thing' });
    enqueueTask(repoPath, { id: 'T2', title: 'implement', goal: 'build it' });
    const tasks = listTasks(repoPath);
    const card = plain(renderQueueCard(tasks, taskCount(repoPath)).join('\n'));
    expect(card).toContain('pending 2');
    expect(card).toContain('T1');
    expect(card).toContain('●');
    expect(card).toContain('devagent consume --auto-pr');
    expect(card.split('\n').filter((l) => l.includes('next:')).length).toBe(1);
  });

  it('--json contract remains a task array (documented via listTasks shape)', () => {
    enqueueTask(repoPath, { id: 'T1', title: 'research', goal: 'do the thing' });
    const tasks = listTasks(repoPath);
    const json = JSON.stringify(tasks, null, 2);
    const parsed = JSON.parse(json) as unknown;
    expect(Array.isArray(parsed)).toBe(true);
    expect((parsed as { id: string }[])[0]?.id).toBe('T1');
  });
});
