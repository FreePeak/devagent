#!/usr/bin/env node
// Compute the next semantic version from Conventional-Commit titles since
// the last v* tag. Covers both merge commits and squash-merged PRs (squash
// titles are exactly the PR title).
// Major > minor > patch: the highest bump present wins.
// - feat:    -> minor
// - fix:     -> patch
// - ! / BREAKING CHANGE -> major
// - anything else (docs:, chore:, config:, refactor:) -> patch floor
import { execFileSync } from 'node:child_process';

function git(args) {
  try {
    return execFileSync('git', args, { encoding: 'utf8' });
  } catch {
    return '';
  }
}

// The last tag must come from the REMOTE, not the local checkout snapshot:
// two pushes landing close together race — run A tags vX.Y.Z after run B
// checked out, so B's local describe misses the new tag and computes the
// same version again (gh release create then fails HTTP 422; live
// 2026-09-01: v0.9.3/v0.9.4 double-tap). ls-remote sees the authoritative
// tag list regardless of checkout timing.
const tagListRaw = git(['ls-remote', '--tags', 'origin', 'refs/tags/v*']);
// No origin (scratch repos, vendored use): fall back to the local tag list —
// still better than describe because sort is numeric and non-semver tags drop.
const tagLines = tagListRaw.trim()
  ? tagListRaw
  : git(['tag', '--list', 'v*']).split('\n').map((t) => `refs/tags/${t.trim()}`).join('\n');
const remoteTags = tagLines
  .split('\n')
  .map((l) => l.trim())
  .filter(Boolean)
  .map((l) => l.split('refs/tags/')[1] ?? '')
  .filter((t) => /^v\d+\.\d+\.\d+$/.test(t))
  .sort((a, b) => {
    const [aM, aN, aP] = a.slice(1).split('.').map(Number);
    const [bM, bN, bP] = b.slice(1).split('.').map(Number);
    return aM - bM || aN - bN || aP - bP;
  });
const lastTag = remoteTags.at(-1) ?? '';
const prev = lastTag ? lastTag.replace(/^v/, '') : '0.0.0';

// Commits since the last tag. When the local snapshot lacks the remote tag's
// commit (mid-race checkout), the range is empty — fall back to a bounded
// window of recent subjects so the bump floor stays correct.
let subjects = [];
if (lastTag) {
  subjects = git(['log', '--format=%s', `${lastTag}..HEAD`])
    .split('\n')
    .map((s) => s.trim())
    .filter(Boolean);
  if (subjects.length === 0) {
    subjects = git(['log', '--format=%s', '-20'])
      .split('\n')
      .map((s) => s.trim())
      .filter(Boolean);
  }
} else {
  subjects = git(['log', '--format=%s', 'HEAD'])
    .split('\n')
    .map((s) => s.trim())
    .filter(Boolean);
}

// PR titles arrive as "<type>[!][scope]: <subject>" via squash/merge commits.
const TYPE = /^(\w+)(\([^)]*\))?(!)?:\s/;
let bump = 'none';
for (const s of subjects) {
  const m = TYPE.exec(s);
  if (!m) continue;
  const [, type, , bang] = m;
  if (bang || /breaking/i.test(s)) {
    bump = 'major';
    break;
  }
  if (type === 'feat' && bump !== 'major') bump = 'minor';
  else if ((type === 'fix' || bump === 'none') && bump !== 'major' && bump !== 'minor') bump = 'patch';
}

const [maj, min, pat] = prev.split('.').map(Number);
let next;
switch (bump) {
  case 'major': next = `${maj + 1}.0.0`; break;
  case 'minor': next = `${maj}.${min + 1}.0`; break;
  case 'patch': next = `${maj}.${min}.${pat + 1}`; break;
  default:      next = `${maj}.${min}.${pat + 1}`; // docs/chore-only: patch floor
}

console.log(JSON.stringify({ prev, next, bump, prCount: subjects.length }));
