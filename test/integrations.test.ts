import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('node:child_process', async (importOriginal) => {
  const actual =
    await importOriginal<typeof import('node:child_process')>();
  return {
    ...actual,
    execFile: vi.fn(),
  };
});

import { execFile } from 'node:child_process';
import {
  LINEAR_ISSUE_QUERY,
  extractAcceptanceCriteria,
  fetchTicket,
  parseLinearIssue,
} from '../src/integrations/linear.js';
import { branchExists, createPr, pushBranch } from '../src/integrations/github.js';
import {
  createWorktree,
  removeWorktree,
  sanitizeTicketId,
} from '../src/git/worktree.js';

type ExecCall = [file: string, args: string[], opts: { cwd?: string }];

function mockedExec() {
  const m = vi.mocked(execFile);
  // promisify(execFile) expects a callback-style function; vitest mock satisfies it.
  m.mockImplementation(((...cbArgs: unknown[]) => {
    const cb = cbArgs[cbArgs.length - 1] as (
      err: Error | null,
      stdout: string,
      stderr: string,
    ) => void;
    cb(null, '', '');
    return undefined;
  }) as never);
  return m;
}

function graphqlResponse(issue: unknown) {
  return {
    ok: true,
    status: 200,
    json: async () => ({ data: { issue } }),
  };
}

// ---------- Linear ----------

describe('linear', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('exports a query selecting expected fields by identifier', () => {
    expect(LINEAR_ISSUE_QUERY).toContain('issue(id: $id)');
    expect(LINEAR_ISSUE_QUERY).toContain('title');
    expect(LINEAR_ISSUE_QUERY).toContain('description');
    expect(LINEAR_ISSUE_QUERY).toContain('url');
    expect(LINEAR_ISSUE_QUERY).toContain('labels');
    expect(LINEAR_ISSUE_QUERY).toContain('name');
  });

  it('parseLinearIssue maps labels, description and url', () => {
    const ticket = parseLinearIssue({
      data: {
        issue: {
          title: 'Add endpoint',
          description: 'Do the thing',
          url: 'https://linear.app/proj/issue/ENG-1',
          labels: { nodes: [{ name: 'backend' }, { name: 'P1' }] },
        },
      },
    });
    expect(ticket.title).toBe('Add endpoint');
    expect(ticket.description).toBe('Do the thing');
    expect(ticket.labels).toEqual(['backend', 'P1']);
    expect(ticket.url).toBe('https://linear.app/proj/issue/ENG-1');
  });

  it('extractAcceptanceCriteria prefers checklist under Acceptance heading', () => {
    const md = [
      '## Context',
      '- [ ] not an AC',
      '',
      '## Acceptance Criteria',
      '- [ ] first item',
      '- [x] second item',
      '- [X] third ITEM',
      '',
      '## Notes',
      '- [ ] also not an AC',
    ].join('\n');
    expect(extractAcceptanceCriteria(md)).toEqual([
      'first item',
      'second item',
      'third ITEM',
    ]);
  });

  it('extractAcceptanceCriteria falls back to all checklist items without heading', () => {
    const md = ['Intro text', '', '- [ ] alpha', '- [X] beta'].join('\n');
    expect(extractAcceptanceCriteria(md)).toEqual(['alpha', 'beta']);
  });

  it('parseLinearIssue returns empty acceptanceCriteria when no checklists', () => {
    const ticket = parseLinearIssue({
      data: { issue: { title: 'T', description: 'plain text only' } },
    });
    expect(ticket.acceptanceCriteria).toEqual([]);
  });

  it('fetchTicket posts with raw Authorization header (no Bearer)', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      graphqlResponse({
        title: 'Ship it',
        description: '- [ ] works',
        url: 'https://linear.app/x/y/ENG-9',
        labels: { nodes: [{ name: 'api' }] },
      }),
    );
    vi.stubGlobal('fetch', fetchMock);

    const ticket = await fetchTicket('ENG-9', 'lin_api_key_123');

    expect(ticket.id).toBe('ENG-9');
    expect(ticket.acceptanceCriteria).toEqual(['works']);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe('https://api.linear.app/graphql');
    const headers = init.headers as Record<string, string>;
    expect(headers.Authorization).toBe('lin_api_key_123');
    const body = JSON.parse(String(init.body)) as {
      variables: { id: string };
    };
    expect(body.variables.id).toBe('ENG-9');
  });

  it('fetchTicket throws on non-200 responses', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({ ok: false, status: 401 }),
    );
    await expect(fetchTicket('ENG-1', 'key')).rejects.toThrow(
      /Linear API request failed: HTTP 401/,
    );
  });

  it('fetchTicket throws when issue missing from response', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(graphqlResponse(null)),
    );
    await expect(fetchTicket('NOPE-404', 'key')).rejects.toThrow(
      /issue not found/i,
    );
  });
});

// ---------- GitHub ----------

describe('github', () => {
  let m: ReturnType<typeof mockedExec>;

  beforeEach(() => {
    m = mockedExec();
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('createPr shells out to gh pr create with correct args and returns URL', async () => {
    m.mockImplementation(((
      _file: string,
      _args: string[],
      _opts: unknown,
      cb: (err: null, stdout: string, stderr: string) => void,
    ) => {
      cb(null, 'Creating PR...\nhttps://github.com/o/r/pull/7\n', '');
      return undefined;
    }) as never);

    const url = await createPr({
      repoPath: '/repo',
      branch: 'devagent/ENG-9',
      title: 'PR title',
      body: 'PR body',
    });

    expect(url).toBe('https://github.com/o/r/pull/7');
    expect(m).toHaveBeenCalledTimes(1);
    const [file, args, opts] = m.mock.calls[0] as unknown as ExecCall;
    expect(file).toBe('gh');
    expect(opts.cwd).toBe('/repo');
    expect(args.slice(0, 2)).toEqual(['pr', 'create']);
    expect(args[args.indexOf('-t') + 1]).toBe('PR title');
    expect(args[args.indexOf('-b') + 1]).toBe('PR body');
    expect(args[args.indexOf('-H') + 1]).toBe('devagent/ENG-9');
    expect(args).not.toContain('-B');
  });

  it('createPr includes -B baseBranch when provided', async () => {
    m.mockImplementation(((
      _file: string,
      _args: string[],
      _opts: unknown,
      cb: (err: null, stdout: string, stderr: string) => void,
    ) => {
      cb(null, 'https://github.com/o/r/pull/8\n', '');
      return undefined;
    }) as never);

    await createPr({
      repoPath: '/repo',
      branch: 'feature',
      title: 't',
      body: 'b',
      baseBranch: 'develop',
    });

    const [, args] = m.mock.calls[0] as unknown as ExecCall;
    expect(args[args.indexOf('-B') + 1]).toBe('develop');
  });

  it('createPr throws descriptive error including stderr on failure', async () => {
    m.mockImplementation(((
      _file: string,
      _args: string[],
      _opts: unknown,
      cb: (err: Error | null, stdout: string, stderr: string) => void,
    ) => {
      cb(Object.assign(new Error('exit code 1'), { stderr: 'no remote configured' }), '', '');
      return undefined;
    }) as never);

    await expect(
      createPr({ repoPath: '/repo', branch: 'b', title: 't', body: 'b' }),
    ).rejects.toThrow(/gh pr create failed.*no remote configured/s);
  });

  it('branchExists resolves false when rev-parse fails', async () => {
    m.mockImplementation(((
      _file: string,
      args: string[],
      _opts: unknown,
      cb: (err: Error | null) => void,
    ) => {
      expect(args[0]).toBe('rev-parse');
      cb(new Error('fatal: Needed a single revision'));
      return undefined;
    }) as never);

    await expect(branchExists('/repo', 'missing')).resolves.toBe(false);
  });

  it('branchExists resolves true when rev-parse succeeds', async () => {
    await expect(branchExists('/repo', 'main')).resolves.toBe(true);
  });
});

// ---------- Worktree ----------

describe('worktree', () => {
  let m: ReturnType<typeof mockedExec>;

  beforeEach(() => {
    m = mockedExec();
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('sanitizeTicketId keeps only [A-Za-z0-9-_]', () => {
    expect(sanitizeTicketId('ENG-204')).toBe('ENG-204');
    expect(sanitizeTicketId('eng/204 x!')).toBe('eng204x');
  });

  it('createWorktree builds correct git args and returns path and branch', async () => {
    const info = await createWorktree('/repo', 'ENG-204');

    expect(info.branch).toBe('devagent/ENG-204');
    expect(info.worktreePath).toBe('/repo/.devagent-worktrees/ENG-204');
    expect(m).toHaveBeenCalledTimes(1);
    const [file, args, opts] = m.mock.calls[0] as unknown as ExecCall;
    expect(file).toBe('git');
    expect(args).toEqual([
      'worktree',
      'add',
      '-b',
      'devagent/ENG-204',
      '/repo/.devagent-worktrees/ENG-204',
    ]);
    expect(opts.cwd).toBe('/repo');
  });

  it('createWorktree throws descriptive error when branch already exists', async () => {
    m.mockImplementation(((
      _file: string,
      _args: string[],
      _opts: unknown,
      cb: (err: Error | null, stdout: string, stderr: string) => void,
    ) => {
      cb(
        Object.assign(new Error('exit code 128'), {
          stderr:
            "fatal: a branch named 'devagent/ENG-1' already exists",
        }),
        '',
        '',
      );
      return undefined;
    }) as never);

    await expect(createWorktree('/repo', 'ENG-1')).rejects.toThrow(
      /branch "devagent\/ENG-1" already exists/,
    );
  });

  it('removeWorktree runs git worktree remove --force and swallows failures', async () => {
    await removeWorktree('/repo', 'ENG-204');
    const [file, args] = m.mock.calls[0] as unknown as ExecCall;
    expect(file).toBe('git');
    expect(args).toEqual([
      'worktree',
      'remove',
      '--force',
      '/repo/.devagent-worktrees/ENG-204',
    ]);

    m.mockImplementation(((
      _file: string,
      _args: string[],
      _opts: unknown,
      cb: (err: Error | null) => void,
    ) => {
      cb(new Error('not a working tree'));
      return undefined;
    }) as never);
    await expect(removeWorktree('/repo', 'GONE-1')).resolves.toBeUndefined();
  });
});

describe('pushBranch', () => {
  let m: ReturnType<typeof mockedExec>;
  beforeEach(() => {
    m = mockedExec();
  });
  afterEach(() => {
    vi.restoreAllMocks();
  });
  it('pushes explicit refspec to origin', async () => {
    m.mockImplementation(((file: string, args: string[], _o: unknown, cb: (err: null) => void) => {
      expect(file).toBe('git');
      expect(args).toEqual(['push', '-u', 'origin', 'devagent/ENG-9:devagent/ENG-9']);
      cb(null);
      return undefined;
    }) as never);
    await pushBranch('/repo', 'devagent/ENG-9');
  });

  it('rejects with stderr detail on failure', async () => {
    m.mockImplementation(((
      _f: string,
      _a: string[],
      _o: unknown,
      cb: (err: Error, stdout: string, stderr: string) => void,
    ) => {
      const e = new Error('git failed') as Error & { stderr?: string };
      e.stderr = '! [remote rejected] x (permission denied)';
      cb(e, '', e.stderr);
      return undefined;
    }) as never);
    await expect(pushBranch('/repo', 'x')).rejects.toThrow(/permission denied/);
  });
});
