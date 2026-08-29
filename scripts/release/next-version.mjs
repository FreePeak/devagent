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

const lastTag = git(['describe', '--tags', '--abbrev=0', '--match', 'v*']).trim();
const range = lastTag ? `${lastTag}..HEAD` : 'HEAD';
const prev = lastTag ? lastTag.replace(/^v/, '') : '0.0.0';

const subjects = git(['log', '--format=%s', range])
  .split('\n')
  .map((s) => s.trim())
  .filter(Boolean);

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
