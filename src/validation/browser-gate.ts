import { spawn } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import type { BrowserCheckConfig, GateResult } from '../types.js';
import { sanitizeTicketId } from '../git/worktree.js';

/**
 * Gate G5 (browser evidence channel): boots the repo's dev server inside the
 * worktree, drives it headlessly with Playwright, asserts every `expect`
 * clause, and writes a screenshot plus DOM excerpt into the run's evidence
 * directory so auditors can cite visual evidence. Honest-skip semantics
 * mirror G1: a missing Playwright module or browser binary never fails a leg.
 */

const CONFIG_FILENAMES = ['devagent.json', '.devagent.json'];
const DOM_EXCERPT_CHARS = 5000;
const SERVER_POLL_MS = 300;

/** Read browserCheck from devagent.json / .devagent.json; undefined when absent. */
export function loadBrowserCheck(repoPath: string): BrowserCheckConfig | undefined {
  for (const name of CONFIG_FILENAMES) {
    const p = join(repoPath, name);
    if (!existsSync(p)) continue;
    let raw: unknown;
    try {
      raw = JSON.parse(readFileSync(p, 'utf8'))?.browserCheck;
    } catch {
      // malformed config JSON: fall through to the next candidate filename
      continue;
    }
    if (raw === undefined) return undefined;
    return validateBrowserCheck(raw, p);
  }
  return undefined;
}

function validateBrowserCheck(raw: unknown, file: string): BrowserCheckConfig {
  const o = raw as Record<string, unknown>;
  const fail = (msg: string): never => {
    throw new Error(`Invalid browserCheck in ${file}: ${msg}`);
  };
  if (typeof o !== 'object' || o === null) fail('expected an object');
  if (typeof o.start !== 'string' || o.start.trim().length === 0) fail('"start" must be a non-empty string');
  if (typeof o.url !== 'string' || !/^https?:\/\//.test(o.url)) fail('"url" must be an http(s) URL');
  if (!Array.isArray(o.expect) || o.expect.length === 0) fail('"expect" must be a non-empty array');
  for (const clause of o.expect as unknown[]) {
    if (typeof clause !== 'string' || !/^(text|selector):.+/.test(clause)) {
      fail(`expect clause ${JSON.stringify(clause)} must match "text:<substring>" or "selector:<css>"`);
    }
  }
  if (o.screenshot !== undefined && typeof o.screenshot !== 'boolean') fail('"screenshot" must be a boolean');
  return {
    start: o.start as string,
    url: o.url as string,
    expect: o.expect as string[],
    ...(typeof o.screenshot === 'boolean' ? { screenshot: o.screenshot } : {}),
  };
}

/** Deterministic evidence directory for a task's G5 artifacts under the repo root. */
export function g5EvidenceDir(repoRoot: string, taskId: string): string {
  return join(repoRoot, '.devagent', 'runs', sanitizeTicketId(taskId), 'g5');
}

/** Loaded G5 evidence markdown for the auditor prompt, when present. */
export function readG5Evidence(repoRoot: string, taskId: string): string | undefined {
  const p = join(g5EvidenceDir(repoRoot, taskId), 'g5-evidence.md');
  if (!existsSync(p)) return undefined;
  try {
    return readFileSync(p, 'utf8').slice(0, 2000);
  } catch {
    return undefined;
  }
}

/**
 * Abstraction over the headless browser so tests can drive the gate without
 * Playwright installed. A factory returning null means "dependency missing"
 * and produces an honest skip, never a failure.
 */
export interface BrowserDriver {
  goto(url: string, timeoutMs: number): Promise<{ content: string; hasSelector(selector: string): boolean | Promise<boolean> }>;
  screenshot(path: string): Promise<void>;
  close(): Promise<void>;
}
export type DriverFactory = () => Promise<BrowserDriver | null>;

export function defaultDriverFactory(): Promise<BrowserDriver | null> {
  return (async () => {
    let pw: typeof import('playwright');
    try {
      // Optional dependency: repos without it still get honest-skip, not failure.
      pw = await import('playwright' as string);
    } catch {
      return null;
    }
    try {
      const browser = await pw.chromium.launch({ headless: true });
      const page = await browser.newPage();
      return {
        async goto(url, timeoutMs) {
          await page.goto(url, { timeout: timeoutMs, waitUntil: 'domcontentloaded' });
          return { content: await page.content(), hasSelector: async (sel: string) => (await page.locator(sel).count()) > 0 };
        },
        async screenshot(path) {
          await page.screenshot({ path, fullPage: true });
        },
        async close() {
          await browser.close();
        },
      };
    } catch (err) {
      if (/Executable doesn't exist|browserType\.launch/i.test((err as Error).message)) return null;
      throw err;
    }
  })();
}

export interface ClauseResult {
  clause: string;
  ok: boolean;
  note?: string;
}

/** Evaluate every expect clause against loaded page content. Pure and testable. */
export async function evaluateExpectations(
  expect: string[],
  page: { content: string; hasSelector(selector: string): boolean | Promise<boolean> },
): Promise<ClauseResult[]> {
  const results: ClauseResult[] = [];
  for (const clause of expect) {
    if (clause.startsWith('text:')) {
      const needle = clause.slice('text:'.length);
      const ok = page.content.includes(needle);
      results.push({ clause, ok, ...(ok ? {} : { note: `"${needle}" not found in DOM` }) });
      continue;
    }
    const selector = clause.slice('selector:'.length);
    try {
      const ok = await page.hasSelector(selector);
      results.push({ clause, ok, ...(ok ? {} : { note: `no element matches "${selector}"` }) });
    } catch (err) {
      results.push({ clause, ok: false, note: `invalid selector "${selector}": ${(err as Error).message}` });
    }
  }
  return results;
}

interface ServerProcess {
  child: ReturnType<typeof spawn>;
  stop(): void;
  outputTail(): string;
}

function startServer(cmd: string, cwd: string): ServerProcess {
  const child = spawn(cmd, { cwd, shell: true, stdio: ['ignore', 'pipe', 'pipe'], detached: true });
  const out: string[] = [];
  child.stdout?.on('data', (d: Buffer) => out.push(d.toString()));
  child.stderr?.on('data', (d: Buffer) => out.push(d.toString()));
  return {
    child,
    stop() {
      try {
        if (child.pid !== undefined) process.kill(-child.pid, 'SIGTERM');
      } catch {}
      setTimeout(() => {
        try {
          if (child.pid !== undefined && child.killed === false) process.kill(-child.pid, 'SIGKILL');
        } catch {}
      }, 1500).unref();
    },
    outputTail: () => out.join('').split('\n').slice(-15).join('\n'),
  };
}

async function waitForServer(url: string, deadline: number, server: ServerProcess): Promise<boolean> {
  while (Date.now() < deadline) {
    const up = await new Promise<boolean>((resolve) => {
      const req = fetch(url, { signal: AbortSignal.timeout(2000) })
        .then(() => resolve(true))
        .catch(() => resolve(false));
      void req;
    });
    if (up) return true;
    if (server.child.exitCode !== null && server.child.exitCode !== undefined) return false;
    await new Promise((r) => setTimeout(r, SERVER_POLL_MS));
  }
  return false;
}

export async function runBrowserGate(args: {
  worktreePath: string;
  evidenceDir: string;
  timeoutMs: number;
  driverFactory?: DriverFactory;
  /** Server readiness probe; overridable for tests that inject a fake driver. */
  serverReady?: (url: string, deadline: number) => Promise<boolean>;
}): Promise<GateResult | null> {
  const cfg = loadBrowserCheck(args.worktreePath);
  if (!cfg) return null; // no browserCheck -> gate not run at all

  const factory = args.driverFactory ?? defaultDriverFactory;
  const driver = await factory().catch(() => null);
  if (!driver) {
    return {
      gate: 'G5-browser',
      passed: true,
      skipped: true,
      findings: [],
      detail: 'skipped: Playwright or a chromium browser binary is unavailable (npm i -D playwright && npx playwright install chromium to enable)',
    };
  }

  const server = startServer(cfg.start, args.worktreePath);
  const ready = args.serverReady ?? ((url: string, deadline: number) => waitForServer(url, deadline, server));
  try {
    const bootDeadline = Date.now() + Math.max(args.timeoutMs * 0.6, 10_000);
    if (!(await ready(cfg.url, bootDeadline))) {
      const why = server.child.exitCode !== null && server.child.exitCode !== undefined
        ? `start command exited with code ${server.child.exitCode}`
        : `server did not respond on ${cfg.url} within budget`;
      return {
        gate: 'G5-browser',
        passed: false,
        findings: [],
        detail: `browser check failed: ${why}\nserver output:\n${server.outputTail()}`,
      };
    }

    let page: { content: string; hasSelector(selector: string): boolean | Promise<boolean> };
    try {
      page = await driver.goto(cfg.url, Math.max(args.timeoutMs * 0.3, 5000));
    } catch (err) {
      return {
        gate: 'G5-browser',
        passed: false,
        findings: [],
        detail: `browser check failed: could not load ${cfg.url}: ${(err as Error).message}`,
      };
    }

    const clauses = await evaluateExpectations(cfg.expect, page);
    const failedClauses = clauses.filter((c) => !c.ok);

    // Evidence artifacts land outside the worktree so they are auditor-visible
    // without being committed into the PR branch.
    mkdirSync(args.evidenceDir, { recursive: true });
    writeFileSync(join(args.evidenceDir, 'g5-dom.html'), page.content.slice(0, DOM_EXCERPT_CHARS));
    const artifactPaths = [join(args.evidenceDir, 'g5-dom.html')];
    if (cfg.screenshot !== false) {
      const shot = join(args.evidenceDir, 'g5-screenshot.png');
      try {
        await driver.screenshot(shot);
        artifactPaths.push(shot);
      } catch {
        // screenshot is best-effort; clause results decide the verdict
      }
    }

    const detailLines = [
      ...clauses.map((c) => `${c.ok ? 'PASS' : 'FAIL'} ${c.clause}${c.note ? ` (${c.note})` : ''}`),
      `artifacts: ${artifactPaths.join(', ')}`,
    ];
    const result: GateResult = {
      gate: 'G5-browser',
      passed: failedClauses.length === 0,
      findings: [],
      detail: detailLines.join('\n'),
    };

    writeFileSync(
      join(args.evidenceDir, 'g5-evidence.md'),
      [
        '# G5 browser evidence',
        '',
        `- url: ${cfg.url}`,
        `- verdict: ${result.passed ? 'pass' : 'fail'}`,
        '',
        ...detailLines.map((l) => `- ${l}`),
        '',
        '## DOM excerpt',
        '',
        '```html',
        page.content.slice(0, DOM_EXCERPT_CHARS),
        '```',
        '',
      ].join('\n'),
    );
    return result;
  } finally {
    server.stop();
    await driver.close().catch(() => {});
  }
}
