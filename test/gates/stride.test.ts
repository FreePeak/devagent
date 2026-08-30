import { describe, expect, it } from 'vitest';
import { evaluateStride } from '../../src/gates/stride.js';

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

describe('evaluateStride (G5 gate executor)', () => {
  const positives: Array<{ file: string; added?: string[]; removed?: string[]; category: string }> = [
    {
      file: 'src/spoof.ts',
      added: ['const api_key = "sk-live-abcd1234";'],
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
    const r = await evaluateStride({ diff: diffFor('src/cred.ts', ['const api_key = "sk-live-abcd1234";']) });
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
});
