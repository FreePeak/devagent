import { existsSync, readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

/**
 * Monitoring (v2 gap): render a zero-dependency HTML status board from the
 * structured JSONL run logs. Static file — open it or serve it anywhere.
 */

export interface RunSummary {
  runId: string;
  file: string;
  startedAt: string | null;
  lastAt: string | null;
  lastStage: string;
  lastLevel: string;
  lastMessage: string;
  eventCount: number;
  ok: boolean;
}

export function collectRunSummaries(runsDir: string): RunSummary[] {
  if (!existsSync(runsDir)) return [];
  const summaries: RunSummary[] = [];
  for (const f of readdirSync(runsDir).sort()) {
    if (!f.endsWith('.jsonl')) continue;
    const raw = readFileSync(join(runsDir, f), 'utf8').trim();
    if (!raw) continue;
    const lines = raw.split('\n');
    let first: Record<string, unknown> | null = null;
    let last: Record<string, unknown> | null = null;
    for (const line of lines) {
      try {
        const e = JSON.parse(line) as Record<string, unknown>;
        last = e;
        first ??= e;
      } catch {
        continue;
      }
    }
    if (!last) continue;
    const level = String(last.level ?? 'info');
    summaries.push({
      runId: String(last.runId ?? f.replace(/\.jsonl$/, '')),
      file: f,
      startedAt: first ? String(first.ts) : null,
      lastAt: String(last.ts),
      lastStage: String(last.stage),
      lastLevel: level,
      lastMessage: String(last.message),
      eventCount: lines.length,
      ok: level !== 'error',
    });
  }
  return summaries;
}

export function renderDashboard(summaries: RunSummary[], title = 'DevAgent Runs'): string {
  const rows = summaries
    .map(
      (s) => `<tr class="${s.ok ? 'ok' : 'err'}">
<td><code>${esc(s.runId.slice(0, 8))}</code></td>
<td>${esc(s.lastAt ?? '')}</td>
<td><span class="pill">${esc(s.lastStage)}</span></td>
<td>${esc(s.lastLevel)}</td>
<td>${esc(s.lastMessage)}</td>
<td>${s.eventCount}</td>
</tr>`,
    )
    .join('\n');

  return `<!doctype html>
<html><head><meta charset="utf-8"><title>${esc(title)}</title>
<style>
body{font-family:ui-sans-serif,system-ui,sans-serif;margin:2rem;background:#111;color:#eee}
table{border-collapse:collapse;width:100%}
th,td{padding:.4rem .6rem;border-bottom:1px solid #333;text-align:left;font-size:.85rem}
th{color:#999;text-transform:uppercase;font-size:.7rem}
tr.err td:first-child{border-left:3px solid #e5484d}
tr.ok td:first-child{border-left:3px solid #46a758}
.pill{background:#2a2a2a;padding:.1rem .5rem;border-radius:99px;font-size:.75rem}
</style></head><body>
<h1>${esc(title)} <small style="color:#888;font-weight:normal">(${summaries.length})</small></h1>
<table><thead><tr><th>run</th><th>last event</th><th>stage</th><th>level</th><th>message</th><th>events</th></tr></thead>
<tbody>
${rows}
</tbody></table></body></html>`;
}

export function writeDashboard(homeDir: string): { path: string; runs: number } {
  const summaries = collectRunSummaries(join(homeDir, 'runs'));
  const html = renderDashboard([...summaries].reverse()); // newest first
  const path = join(homeDir, 'dashboard.html');
  writeFileSync(path, html);
  return { path, runs: summaries.length };
}

function esc(s: string): string {
  return s.replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c]!);
}
