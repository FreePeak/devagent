import { describe, expect, it } from 'vitest';
import {
  evaluateStride,
  parseStrideAllowlist,
  pathMatchesAllowlist,
} from '../../src/gates/stride.js';

/**
 * Build a synthetic unified diff around the given added/removed lines.
 * No network, no LLM: string fixtures only.
 */
function diffFor(file: string, added: string[], removed: string[] = []): string {
  const lines = [
    `diff --git a/${file} b/${file}`,
    `--- a/${file}`,
    `+++ b/${file}`,
    `@@ -1,${Math.max(removed.length, 1)} +1,${Math.max(added.length, 1)} @@`,
    ...removed.map((l) => `-${l}`),
    ...added.map((l) => `+${l}`),
  ];
  return lines.join('\n');
}

// Synthetic credential fixtures, assembled at runtime so this file never
// contains a usable credential-shaped literal (the STRIDE detector under
// test still sees the exact same string).
const CRED_LINE = 'const api_key = "sk-live-' + 'abcd1234";';
const CRED_LINE_ALT = 'const api_key = "sk-live-' + 'src-9999";';

describe('evaluateStride (G5 gate executor)', () => {
  const positives: Array<{ file: string; added?: string[]; removed?: string[]; category: string }> = [
    {
      file: 'src/spoof.ts',
      added: [CRED_LINE],
      category: 'Spoofing',
    },
    {
      file: 'src/tamper.ts',
      added: ['db.query(`SELECT * FROM u WHERE id = ${req.params.id}`)'],
      category: 'Tampering',
    },
    {
      file: 'src/repudiate.ts',
      added: ['// audit log call commented out'],
      category: 'Repudiation',
    },
    { file: 'src/disclose.ts', added: ['console.log(req.body)'], category: 'InformationDisclosure' },
    { file: 'src/dos.ts', added: ['setInterval(tick, 1000)'], category: 'DenialOfService' },
    {
      file: 'src/eop.ts',
      added: [],
      removed: ['if (user.role !== "admin") return;'],
      category: 'ElevationOfPrivilege',
    },
  ];

  const negatives: Array<{ file: string; added: string[]; category: string }> = [
    { file: 'src/spoof.ts', added: ['const config = loadConfig(env);'], category: 'Spoofing' },
    { file: 'src/tamper.ts', added: ['db.query("SELECT * FROM u WHERE id = ?", [id])'], category: 'Tampering' },
    { file: 'src/repudiate.ts', added: ['audit("user.updated", ctx);'], category: 'Repudiation' },
    { file: 'src/disclose.ts', added: ['logger.info("handled request");'], category: 'InformationDisclosure' },
    { file: 'src/dos.ts', added: ['await tick();'], category: 'DenialOfService' },
    { file: 'src/eop.ts', added: ['requireRole("admin");'], category: 'ElevationOfPrivilege' },
  ];

  // (a) one positive fixture per STRIDE category
  for (const p of positives) {
    it(`reports ${p.category} for its trigger shape`, async () => {
      const r = await evaluateStride({ diff: diffFor(p.file, p.added ?? [], p.removed ?? []) });
      expect(r.findings.some((f) => f.category === p.category)).toBe(true);
    });
  }

  // (b) one negative fixture per STRIDE category
  for (const n of negatives) {
    it(`reports no ${n.category} for the benign shape`, async () => {
      const r = await evaluateStride({ diff: diffFor(n.file, n.added) });
      expect(r.findings.some((f) => f.category === n.category)).toBe(false);
    });
  }

  it('(c) promotes a HIGH credential literal to CRITICAL', async () => {
    const r = await evaluateStride({ diff: diffFor('src/cred.ts', [CRED_LINE]) });
    const f = r.findings[0];
    expect(f).toBeDefined();
    expect(f?.severity).toBe('CRITICAL');
    expect(r.severityMax).toBe('CRITICAL');
  });

  it('(d) blocks on HIGH (api_key) with severityMax HIGH', async () => {
    const r = await evaluateStride({ diff: diffFor('src/key.ts', ['const api_key = process.env.API_KEY;']) });
    expect(r.findings[0]?.severity).toBe('HIGH');
    expect(r.severityMax).toBe('HIGH');
    expect(r.severityMax).not.toBe('CRITICAL');
  });

  it('(e) treats MEDIUM as advisory: severityMax MEDIUM, no HIGH/CRITICAL', async () => {
    const r = await evaluateStride({ diff: diffFor('src/adv.ts', ['console.log("debug:", password);']) });
    expect(r.severityMax).toBe('MEDIUM');
    expect(r.findings.every((f) => f.severity !== 'HIGH' && f.severity !== 'CRITICAL')).toBe(true);
  });

  it('(f) passes on empty diff with zero findings', async () => {
    for (const d of ['', '   ']) {
      const r = await evaluateStride({ diff: d });
      expect(r.findings).toEqual([]);
      expect(r.severityMax).toBeNull();
    }
  });

  it('(g) passes on null/undefined diff without throwing', async () => {
    for (const d of [null, undefined] as Array<string | null | undefined>) {
      const r = await evaluateStride({ diff: d as string });
      expect(r.findings).toEqual([]);
      expect(r.severityMax).toBeNull();
    }
  });

  it('(h) passes through contextDigest verbatim', async () => {
    const digest = 'sha256:deadbeef';
    const r = await evaluateStride({ diff: diffFor('src/x.ts', ['plain line']), contextDigest: digest });
    expect(r.contextDigest).toBe(digest);
  });

  it('(i) includes file and line provenance on findings', async () => {
    const r = await evaluateStride({ diff: diffFor('src/i.ts', ['console.log(req.body)']) });
    expect(r.findings[0]?.file).toBe('src/i.ts');
    expect(r.findings[0]?.line).toBe(1);
  });

  it('(j) suppresses findings whose file matches a committed allowlist path (PRD Q25)', async () => {
    // Fixture credential in a test file: HIGH + CRITICAL promotion without the allowlist…
    const diff = diffFor('test/fixtures/credentials.json', [CRED_LINE]);
    const blocked = await evaluateStride({ diff });
    expect(blocked.severityMax).toBe('CRITICAL');

    // …and fully suppressed once the PR carries the allowlist entry.
    const allowed = await evaluateStride({ diff, allowlistPaths: ['test/fixtures/**'] });
    expect(allowed.findings).toEqual([]);
    expect(allowed.severityMax).toBeNull();
  });

  it('(k) does not suppress findings outside the allowlist paths', async () => {
    const diff = [
      diffFor('test/fixtures/credentials.json', [CRED_LINE]),
      diffFor('src/cred.ts', [CRED_LINE_ALT]),
    ].join('\n');
    const r = await evaluateStride({ diff, allowlistPaths: ['test/fixtures/**'] });
    expect(r.findings.map((f) => f.file)).toEqual(['src/cred.ts']);
    expect(r.severityMax).toBe('CRITICAL');
  });

  it('(l) treats a missing/empty allowlist as no suppression', async () => {
    const diff = diffFor('src/key.ts', [CRED_LINE]);
    for (const allowlistPaths of [undefined, []]) {
      const r = await evaluateStride({ diff, allowlistPaths });
      expect(r.severityMax).toBe('CRITICAL');
    }
  });

  it('(m) treats a malformed allowlist as fail-closed (no suppression)', async () => {
    for (const text of ['not json', '["src/**"]', '{"paths": "src/**"}', '{"paths": [1]}', 'null']) {
      expect(parseStrideAllowlist(text)).toBeNull();
    }
    const diff = diffFor('src/key.ts', [CRED_LINE]);
    const parsed = parseStrideAllowlist('{"paths": [1]}');
    const r = await evaluateStride({ diff, allowlistPaths: parsed ?? undefined });
    expect(r.severityMax).toBe('CRITICAL');
  });

  it('(n) parses a well-formed allowlist into its path patterns', () => {
    expect(parseStrideAllowlist('{"paths": ["test/**", "fixtures/*.json"]}')).toEqual([
      'test/**',
      'fixtures/*.json',
    ]);
    expect(parseStrideAllowlist('')).toBeNull();
  });

  it('(o) matches allowlist globs: ** crosses slashes, * stays in segment, bare name matches basename', () => {
    const patterns = ['test/**', 'src/fixtures/*.json', 'golden.pem'];
    expect(pathMatchesAllowlist('test/fixtures/creds.json', patterns)).toBe(true);
    expect(pathMatchesAllowlist('src/fixtures/creds.json', patterns)).toBe(true);
    expect(pathMatchesAllowlist('src/fixtures/nested/creds.json', patterns)).toBe(false);
    expect(pathMatchesAllowlist('golden.pem', patterns)).toBe(true);
    expect(pathMatchesAllowlist('certs/golden.pem', patterns)).toBe(true);
    expect(pathMatchesAllowlist('src/creds.json', patterns)).toBe(false);
  });
});
