import { describe, expect, it } from 'vitest';
import { createServer } from 'node:http';
import type { Server } from 'node:http';
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  evaluateExpectations,
  g5EvidenceDir,
  loadBrowserCheck,
  runBrowserGate,
  type DriverFactory,
} from '../src/validation/browser-gate.js';

function tempRepo(files: Record<string, string> = {}): string {
  const dir = mkdtempSync(join(tmpdir(), 'da-g5-'));
  for (const [name, content] of Object.entries(files)) {
    writeFileSync(join(dir, name), content);
  }
  return dir;
}

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const s: Server = createServer();
    s.listen(0, '127.0.0.1', () => {
      const port = (s.address() as { port: number }).port;
      s.close(() => resolve(port));
    });
    s.on('error', reject);
  });
}

interface FakePage {
  content: string;
  selectors?: string[];
}

function fakeDriverFactory(page: FakePage, seenUrls: string[] = []): DriverFactory {
  return async () => ({
    async goto(url) {
      seenUrls.push(url);
      return { content: page.content, hasSelector: (sel) => (page.selectors ?? []).includes(sel) };
    },
    async screenshot(path) {
      writeFileSync(path, 'png-bytes');
    },
    async close() {},
  });
}

describe('loadBrowserCheck', () => {
  it('returns undefined with no config file or no browserCheck key', () => {
    expect(loadBrowserCheck(tempRepo())).toBeUndefined();
    const cfg = tempRepo({ 'devagent.json': JSON.stringify({ testCommand: 'npm test' }) });
    try {
      expect(loadBrowserCheck(cfg)).toBeUndefined();
    } finally {
      rmSync(cfg, { recursive: true, force: true });
    }
  });

  it('parses a valid browserCheck block', () => {
    const dir = tempRepo({
      'devagent.json': JSON.stringify({
        browserCheck: { start: 'npm run dev', url: 'http://localhost:3000/', expect: ['text:hi', 'selector:#app'] },
      }),
    });
    try {
      expect(loadBrowserCheck(dir)).toEqual({
        start: 'npm run dev',
        url: 'http://localhost:3000/',
        expect: ['text:hi', 'selector:#app'],
      });
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('throws on malformed blocks instead of silently skipping', () => {
    const cases: unknown[] = [
      { start: '', url: 'http://x/', expect: ['text:a'] },
      { start: 'x', url: 'ftp://nope/', expect: ['text:a'] },
      { start: 'x', url: 'http://x/', expect: [] },
      { start: 'x', url: 'http://x/', expect: ['click:button'] },
      { start: 'x', url: 'http://x/', expect: ['text:a'], screenshot: 'yes' },
    ];
    for (const browserCheck of cases) {
      const dir = tempRepo({ '.devagent.json': JSON.stringify({ browserCheck }) });
      try {
        expect(() => loadBrowserCheck(dir)).toThrow(/Invalid browserCheck/);
      } finally {
        rmSync(dir, { recursive: true, force: true });
      }
    }
  });
});

describe('evaluateExpectations', () => {
  const page = { content: '<html>hello world <div id="app"></div></html>', hasSelector: async (s: string) => s === '#app' };

  it('passes text and selector hits', async () => {
    const r = await evaluateExpectations(['text:hello world', 'selector:#app'], page);
    expect(r.every((c) => c.ok)).toBe(true);
  });

  it('fails misses with a note naming the clause', async () => {
    // Third predicate throws like Playwright does on an invalid selector.
    const strict = { ...page, hasSelector: async (s: string) => { if (s === '[[[') throw new Error('bad selector'); return s === '#app'; } };
    const r = await evaluateExpectations(['text:absent-string', 'selector:.missing', 'selector:[[[' ], strict);
    expect(r.map((c) => c.ok)).toEqual([false, false, false]);
    expect(r[0]!.note).toContain('absent-string');
    expect(r[1]!.note).toContain('.missing');
    expect(r[2]!.note).toContain('invalid selector');
  });
});

describe('runBrowserGate', () => {
  it('returns null without browserCheck so behavior stays byte-identical', async () => {
    const dir = tempRepo();
    try {
      expect(await runBrowserGate({ worktreePath: dir, evidenceDir: join(dir, 'ev'), timeoutMs: 1000 })).toBeNull();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('honestly skips when Playwright or a browser binary is unavailable', async () => {
    const missingModule: DriverFactory = async () => null;
    const throwingFactory: DriverFactory = async () => {
      throw new Error('Executable does not exist at /fake/chromium');
    };
    for (const factory of [missingModule, throwingFactory]) {
      const dir = tempRepo({
        'devagent.json': JSON.stringify({
          browserCheck: { start: 'true', url: 'http://127.0.0.1:1/', expect: ['text:x'] },
        }),
      });
      try {
        const r = await runBrowserGate({ worktreePath: dir, evidenceDir: join(dir, 'ev'), timeoutMs: 1000, driverFactory: factory });
        expect(r).not.toBeNull();
        expect(r!.passed).toBe(true);
        expect(r!.skipped).toBe(true);
        expect(r!.detail).toMatch(/skipped/i);
      } finally {
        rmSync(dir, { recursive: true, force: true });
      }
    }
  });

  it('boots the server, evaluates all clauses, and writes evidence artifacts', async () => {
    const port = await freePort();
    const serverScript = `require('http').createServer((q,s)=>{s.end('welcome to the app')}).listen(${port},'127.0.0.1')`;
    const dir = tempRepo({
      'devagent.json': JSON.stringify({
        browserCheck: {
          start: `node -e ${JSON.stringify(serverScript)}`,
          url: `http://127.0.0.1:${port}/`,
          expect: ['text:welcome to the app', 'selector:#app'],
        },
      }),
    });
    const evidenceDir = join(dir, '..', 'g5-evidence-' + Math.floor(Math.random() * 1e9));
    const seenUrls: string[] = [];
    try {
      const r = await runBrowserGate({
        worktreePath: dir,
        evidenceDir,
        timeoutMs: 15_000,
        driverFactory: fakeDriverFactory({ content: '<html>welcome to the app <div id="app"></div></html>', selectors: ['#app'] }, seenUrls),
        serverReady: async () => true,
      });
      expect(r).not.toBeNull();
      expect(r!.passed).toBe(true);
      expect(seenUrls).toEqual([`http://127.0.0.1:${port}/`]);
      expect(existsSync(join(evidenceDir, 'g5-dom.html'))).toBe(true);
      expect(existsSync(join(evidenceDir, 'g5-screenshot.png'))).toBe(true);
      const md = readFileSync(join(evidenceDir, 'g5-evidence.md'), 'utf8');
      expect(md).toContain('PASS text:welcome to the app');
      expect(md).toContain('PASS selector:#app');
    } finally {
      rmSync(dir, { recursive: true, force: true });
      rmSync(evidenceDir, { recursive: true, force: true });
    }
  }, 30_000);

  it('fails the gate when an expect clause does not hold', async () => {
    const dir = tempRepo({
      'devagent.json': JSON.stringify({
        browserCheck: { start: 'true', url: 'http://127.0.0.1:45658/', expect: ['text:not-there'] },
      }),
    });
    try {
      const r = await runBrowserGate({
        worktreePath: dir,
        evidenceDir: join(dir, 'ev'),
        timeoutMs: 5000,
        driverFactory: fakeDriverFactory({ content: '<html>actual page</html>' }),
        serverReady: async () => true,
      });
      expect(r!.passed).toBe(false);
      expect(r!.detail).toContain('FAIL text:not-there');
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('fails when the dev server never comes up', async () => {
    const port = await freePort(); // reserved then closed: nothing listens
    const dir = tempRepo({
      'devagent.json': JSON.stringify({
        browserCheck: {
          start: 'node -e "setTimeout(()=>{},30000)"',
          url: `http://127.0.0.1:${port}/`,
          expect: ['text:x'],
        },
      }),
    });
    try {
      const r = await runBrowserGate({
        worktreePath: dir,
        evidenceDir: join(dir, 'ev'),
        timeoutMs: 1500,
        // Injected driver isolates server-boot failure from Playwright absence
        driverFactory: fakeDriverFactory({ content: '<html></html>' }),
      });
      expect(r!.passed).toBe(false);
      expect(r!.detail).toContain('browser check failed');
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  }, 20_000);

  it('builds the deterministic evidence dir from repo root and task id', () => {
    expect(g5EvidenceDir('/repo', 'ENG-12/a')).toBe(join('/repo', '.devagent', 'runs', 'ENG-12a', 'g5'));
  });
});
