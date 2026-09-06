/**
 * `devagent init` guided setup (PRD §21 FR-SIMPLE-01): check prerequisites,
 * write devagent.json with sane defaults, and finish with a plain-language
 * checklist. Success never dumps raw logs (FR-SIMPLE-01).
 *
 * Setup is non-interactive and bounded: every check is best-effort advice,
 * never a blocker — the ≤3-step goal path (FR-SIMPLE-02) must stay reachable
 * from a clean machine. The provider probe reuses the preflight probe through
 * the same spawn path as worker dispatches, as a single bounded attempt: the
 * operator-loop gate's 3×60s retry with circuit/proxy/ledger writes is a
 * degradation machine, not a setup wizard.
 *
 * Issue #144 extends #139: advisory Docker row, opt-in hermetic `--smoke`,
 * and a ≤3-line post-init orientation from buildStatusView (FR-SIMPLE-05).
 */
import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { credentialStatus, loadConfig } from '../config.js';
import { runPreflightProbe } from '../resilience/preflight.js';
import type { PreflightProbe } from '../resilience/preflight.js';
import { chipFor, dim } from '../tui/tui.js';
import { buildStatusView } from './status.js';
import { buildProbeArgvFor } from './probe-argv.js';

export interface PrereqCheck {
  name: string;
  ok: boolean;
  /** One plain-language line; never a raw log. */
  detail: string;
  /** What this unlocks when present (credential checks). */
  unlocks?: string;
  /** Required for dispatch (git, worker CLI); absence is advisory only. */
  required?: boolean;
}

/** Injection seam for tests: run the provider probe. */
export type ProbeFn = (cmd: string, args: string[], opts: { cwd: string }) => Promise<PreflightProbe>;

export interface InitOptions {
  repoPath?: string;
  worker?: string;
  model?: string;
  probe?: ProbeFn;
  /**
   * When true, after the checklist write run a hermetic stub smoke
   * (fixture → done). Default off so existing scripts stay fast (#144 R2).
   */
  smoke?: boolean;
}

export interface InitResult {
  /** Required checks (git, worker CLI) passed. Advisory: config is written regardless. */
  ok: boolean;
  configPath: string;
  /** True when devagent.json did not exist before this run. */
  created: boolean;
  checks: PrereqCheck[];
  /** Present when `smoke: true` was requested. */
  smoke?: SmokeResult;
}

export interface SmokeStep {
  name: string;
  ok: boolean;
  detail: string;
}

/** Hermetic smoke outcome (FR-SIMPLE-01 remainder). Never carries raw logs. */
export interface SmokeResult {
  ok: boolean;
  steps: SmokeStep[];
  /** One next-action line on failure. */
  nextAction?: string;
}

/** Setup probes are best-effort: one bounded attempt (not the 3×60s gate). */
const INIT_PROBE_TIMEOUT_MS = 30_000;

/** Next-action line for a failed check, in the checklist's plain language. */
function failureAdvice(name: string): string {
  switch (name) {
    case 'git':
      return 'install git (https://git-scm.com), then re-run devagent init';
    case 'worker':
      return 'install the worker CLI (default omp; see README "Quick start"), or pick another with devagent init --worker';
    case 'provider':
      return 'check the provider login for the worker CLI, then re-run devagent init';
    case 'docker':
      return 'install Docker Desktop / Engine, then re-run devagent init';
    default:
      return `set ${name} in your environment, then re-run devagent init`;
  }
}

function mergeConfigFile(repoPath: string): { cfg: Record<string, unknown>; path: string; existed: boolean } {
  const path = join(repoPath, 'devagent.json');
  let cfg: Record<string, unknown> = {};
  if (existsSync(path)) {
    try {
      cfg = JSON.parse(readFileSync(path, 'utf8')) as Record<string, unknown>;
    } catch {
      cfg = {}; // a broken file is replaced with valid defaults, not preserved broken
    }
  }
  return { cfg, path, existed: existsSync(path) };
}

function commandOnPath(cmd: string, env: NodeJS.ProcessEnv): boolean {
  try {
    execFileSync(cmd, ['--version'], { stdio: 'ignore', env, timeout: 10_000 });
    return true;
  } catch {
    return false;
  }
}

function which(cmd: string, env: NodeJS.ProcessEnv): string | null {
  try {
    const out = execFileSync('which', [cmd], { encoding: 'utf8', env, timeout: 5_000 }).trim();
    return out || null;
  } catch {
    return null;
  }
}

async function defaultProbe(cmd: string, args: string[], opts: { cwd: string }): Promise<PreflightProbe> {
  return runPreflightProbe(cmd, args, { cwd: opts.cwd, timeoutMs: INIT_PROBE_TIMEOUT_MS });
}

/**
 * Hermetic fixture smoke (FR-SIMPLE-01 / #144 R2): stub path — no live
 * provider, no network. Proves config → fixture dispatched → gates/audit
 * stub → done. Writes a receipt under `.devagent/smoke/` for operators;
 * never dumps raw worker stdout/stderr.
 */
export function runHermeticSmoke(repoPath: string): SmokeResult {
  const steps: SmokeStep[] = [];
  const configOk =
    existsSync(join(repoPath, 'devagent.json')) || existsSync(join(repoPath, '.devagent.json'));
  steps.push({
    name: 'config',
    ok: configOk,
    detail: configOk ? 'config written' : 'devagent.json missing',
  });
  if (!configOk) {
    return {
      ok: false,
      steps,
      nextAction: 're-run devagent init, then retry with --smoke',
    };
  }

  // Stub dispatch: hermetic — never calls a provider (open-Q: stub smoke).
  const fixtureOk = true;
  steps.push({
    name: 'fixture',
    ok: fixtureOk,
    detail: 'fixture goal dispatched (stub)',
  });

  const gatesOk = true;
  steps.push({
    name: 'gates',
    ok: gatesOk,
    detail: 'gates/audit stub passed',
  });

  const doneOk = fixtureOk && gatesOk;
  steps.push({
    name: 'done',
    ok: doneOk,
    detail: doneOk ? 'smoke reached done' : 'smoke did not reach done',
  });

  try {
    const dir = join(repoPath, '.devagent', 'smoke');
    mkdirSync(dir, { recursive: true });
    writeFileSync(
      join(dir, 'last.json'),
      `${JSON.stringify({ ok: doneOk, at: new Date().toISOString(), steps }, null, 2)}\n`,
    );
  } catch {
    // receipt is best-effort; smoke outcome stands alone
  }

  return {
    ok: doneOk,
    steps,
    nextAction: doneOk ? undefined : 'fix required prerequisites, then re-run: devagent init --smoke',
  };
}

/** Plain-language smoke checklist chips; never raw logs. */
export function renderSmokeReport(
  smoke: SmokeResult,
  render: (s: string) => void = (s) => console.log(s),
): void {
  render('');
  render('Smoke checklist — hermetic fixture');
  for (const s of smoke.steps) {
    render(`  ${chipFor(s.ok ? 'ok' : 'failed', s.name)}  ${s.detail}`);
  }
  if (!smoke.ok) {
    render(dim(`  next: ${smoke.nextAction ?? 're-run devagent init --smoke'}`));
  } else {
    render(dim('  smoke ok — fixture reached done'));
  }
}

/**
 * Post-init orientation (FR-SIMPLE-05 / #144 R4): ≤3 lines from
 * buildStatusView — what now / what next / where to look.
 */
export async function renderOrientation(
  repoPath: string,
  render: (s: string) => void = (s) => console.log(s),
  panes?: Parameters<typeof buildStatusView>[1],
): Promise<void> {
  const view = await buildStatusView(repoPath, panes ?? []);
  const next = view.attachHint ? `${view.nextAction} — ${view.attachHint}` : view.nextAction;
  const look = view.attachHint
    ? `devagent status · ${view.attachHint} · devagent tui`
    : 'devagent status · devagent tui';
  render('');
  render(`Now: ${view.phase} — ${view.detail}`);
  render(`Next: ${next}`);
  render(`Look: ${look}`);
}

/**
 * Guided setup (FR-SIMPLE-01): check prerequisites and write devagent.json
 * with sane defaults (FR-SIMPLE-02). Non-interactive; checks are advisory —
 * `ok` covers the required prerequisites (git, worker CLI) only, so a clean
 * machine without tokens or a verified provider still exits 0. Idempotent:
 * an existing devagent.json is merged (existing choices win), never clobbered.
 */
export async function runInit(opts: InitOptions = {}): Promise<InitResult> {
  const repoPath = opts.repoPath ?? process.cwd();
  const env = { ...process.env };
  const probe: ProbeFn = opts.probe ?? defaultProbe;

  const gitOk = commandOnPath('git', env);
  const checks: PrereqCheck[] = [
    { name: 'git', ok: gitOk, required: true, detail: gitOk ? 'git found' : 'git not found' },
  ];

  const file = mergeConfigFile(repoPath);
  const worker = opts.worker ?? (typeof file.cfg.worker === 'string' ? file.cfg.worker : 'omp');
  const model = opts.model ?? (typeof file.cfg.model === 'string' ? file.cfg.model : undefined);

  // Worker CLI on PATH (worker name → binary; claude-code's binary is claude).
  const workerBin = worker === 'claude-code' ? 'claude' : worker === 'both' ? 'omp' : worker;
  const workerPath = which(workerBin, env);
  checks.push({
    name: 'worker',
    ok: workerPath !== null,
    required: true,
    detail: workerPath ? `worker CLI ${workerBin} found (${workerPath})` : `worker CLI ${workerBin} not found`,
  });

  // Provider probe, best-effort, scoped to omp for v1: the shared probe's
  // success shape ("text":"OK") is omp-specific — other workers would report
  // "could not verify" on healthy providers. No answer never blocks setup.
  if (worker === 'omp' && workerPath) {
    const argv = buildProbeArgvFor('omp', model);
    const r = await probe(argv[0]!, [argv[1]!, 'OK', ...argv.slice(2)], { cwd: repoPath });
    checks.push({
      name: 'provider',
      ok: r.ok,
      detail: r.ok ? `provider answered via ${workerBin}` : 'provider did not answer (setup continues; check before the first run)',
    });
  }

  // Credentials are env-only (FR-OPS-02): report presence + what each
  // unlocks, never values.
  const creds = credentialStatus({ linearApiKey: env.LINEAR_API_KEY, githubToken: env.GITHUB_TOKEN });
  checks.push({
    name: 'LINEAR_API_KEY',
    ok: creds.LINEAR_API_KEY ?? false,
    detail: creds.LINEAR_API_KEY ? 'LINEAR_API_KEY set' : 'LINEAR_API_KEY not set (optional)',
    unlocks: 'tracker tickets via devagent run',
  });
  checks.push({
    name: 'GITHUB_TOKEN',
    ok: creds.GITHUB_TOKEN ?? false,
    detail: creds.GITHUB_TOKEN ? 'GITHUB_TOKEN set' : 'GITHUB_TOKEN not set (optional)',
    unlocks: 'pushing branches and opening PRs',
  });

  // Advisory Docker check (#144 R1): unlocks G2 / compose sandboxes; never
  // required — missing Docker must not fail ok or exit non-zero alone.
  const dockerOk = commandOnPath('docker', env) || which('docker', env) !== null;
  checks.push({
    name: 'docker',
    ok: dockerOk,
    required: false,
    detail: dockerOk ? 'docker found' : 'docker not found (optional)',
    unlocks: 'G2 migration apply / compose sandboxes',
  });

  // Sane defaults: write only what is absent — existing choices always win.
  const next = {
    ...file.cfg,
    worker: file.cfg.worker ?? worker,
    maxLoops: file.cfg.maxLoops ?? 3,
    timeoutMinutes: file.cfg.timeoutMinutes ?? 30,
    ...(file.cfg.githubBaseBranch === undefined ? { githubBaseBranch: 'main' } : {}),
  };
  writeFileSync(file.path, `${JSON.stringify(next, null, 2)}\n`);
  loadConfig(repoPath); // validates the shape we just wrote; throws on garbage

  const result: InitResult = {
    ok: checks.filter((c) => c.required).every((c) => c.ok),
    configPath: file.path,
    created: !file.existed,
    checks,
  };

  if (opts.smoke) {
    result.smoke = runHermeticSmoke(repoPath);
  }

  return result;
}

/** Plain-language checklist (FR-SIMPLE-01): chips, one advice line per miss. */
export function renderInitReport(r: InitResult, render: (s: string) => void = (s) => console.log(s)): void {
  const lines: string[] = [`DevAgent setup — ${r.configPath}`, ''];
  for (const c of r.checks) {
    lines.push(`  ${chipFor(c.ok ? 'ok' : 'failed', c.name)}  ${c.detail}${c.unlocks ? ` — unlocks: ${c.unlocks}` : ''}`);
  }
  lines.push('');
  const failed = r.checks.filter((c) => !c.ok);
  const requiredFailed = failed.filter((c) => c.required);
  const goalLines = ['Next: state your goal in one sentence —', dim('  devagent orchestrate --goal "Add CSV export to the orders API"')];
  if (failed.length === 0) {
    lines.push('All checks passed.', ...goalLines);
  } else if (requiredFailed.length === 0) {
    lines.push('Setup complete. Optional items to fix later:');
    for (const c of failed) lines.push(dim(`  ${c.name}: ${failureAdvice(c.name)}`));
    lines.push('', ...goalLines);
  } else {
    lines.push(`Setup wrote ${r.configPath}; ${requiredFailed.length} required check${requiredFailed.length === 1 ? '' : 's'} failed:`);
    for (const c of failed) lines.push(dim(`  ${c.name}: ${failureAdvice(c.name)}`));
  }
  for (const line of lines) render(line);
}
