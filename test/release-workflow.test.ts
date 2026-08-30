import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

const repoRoot = join(import.meta.dirname, '..');

// Tiny structure extractor for GitHub workflow YAML. The repo has no YAML
// dependency and adding one is out of scope; the test only needs the top-level
// `jobs:` mapping (one `  <name>:` key per job) and each job's `needs:` value,
// which GitHub always renders at fixed indent levels in our workflows.
interface WorkflowJobs {
  jobs: Record<string, { needs?: string[] }>;
}

function extractJobs(raw: string): WorkflowJobs {
  const lines = raw.split('\n');
  const jobs: Record<string, { needs?: string[] }> = {};
  let currentJob: string | null = null;

  for (const line of lines) {
    // Top-level "jobs:" opens the job map.
    if (/^jobs:\s*$/.test(line)) {
      currentJob = null;
      continue;
    }
    // Job names are keys at exactly two spaces of indent, after "jobs:".
    const jobMatch = line.match(/^ {2}([A-Za-z][\w-]*):\s*$/);
    if (jobMatch) {
      currentJob = jobMatch[1];
      jobs[currentJob] = {};
      continue;
    }
    // Job-level properties sit at four spaces of indent.
    const propMatch = currentJob ? line.match(/^ {4}([A-Za-z][\w-]*):\s*(.*)$/) : null;
    if (propMatch && propMatch[1] === 'needs') {
      const value = propMatch[2].trim();
      if (value.startsWith('[')) {
        const inner = value.slice(1, -1).trim();
        jobs[currentJob].needs = inner === '' ? [] : inner.split(',').map((s) => s.trim());
      } else if (value !== '') {
        jobs[currentJob].needs = [value];
      } else {
        // Block form:
        //   needs:
        //     - test
        const idx = lines.indexOf(line);
        const list: string[] = [];
        for (let i = idx + 1; i < lines.length; i += 1) {
          const item = lines[i].match(/^ {6}- (\S+)\s*$/);
          if (!item) break;
          list.push(item[1]);
        }
        jobs[currentJob].needs = list;
      }
    }
  }
  return { jobs };
}

const releaseRaw = readFileSync(join(repoRoot, '.github/workflows/release.yml'), 'utf8');
const release = extractJobs(releaseRaw);

describe('release workflow CI gating', () => {
  it('defines a test job that runs the real CI suite (mirrors ci.yml)', () => {
    expect(release.jobs.test).toBeDefined();
    // The gate must run the actual suite, not a stub: same commands as ci.yml.
    const ciRaw = readFileSync(join(repoRoot, '.github/workflows/ci.yml'), 'utf8');
    for (const cmd of ['npm ci', 'npx tsc --noEmit', 'npx vitest run']) {
      expect(ciRaw).toContain(cmd);
      expect(releaseRaw).toContain(cmd);
    }
  });

  it('gates the release job on the test job via needs', () => {
    expect(release.jobs.release).toBeDefined();
    expect(release.jobs.release.needs).toEqual(['test']);
  });

  it('test job is the root of the gate (no needs of its own)', () => {
    expect(release.jobs.test.needs).toBeUndefined();
  });

  it('keeps the release job behavior untouched', () => {
    // Gating must not alter tagging/versioning logic.
    expect(releaseRaw).toContain('scripts/release/next-version.mjs');
    expect(releaseRaw).toContain('gh release create');
    expect(releaseRaw).toContain("if: github.ref == 'refs/heads/main'");
  });
});
