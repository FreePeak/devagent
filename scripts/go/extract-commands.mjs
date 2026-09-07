#!/usr/bin/env node
/**
 * Freeze the Node CLI's command surface — every command/subcommand with its
 * long flags — into internal/cli/testdata/commands.json. The Go parity test
 * compares the cobra tree against this file; regenerate with
 * `node scripts/go/extract-commands.mjs` when the Node surface changes
 * deliberately. The fixture is generated from the live CLI (requires a fresh
 * `npm run build`), never hand-maintained.
 */
import { execFileSync } from 'node:child_process';

function runHelp(args) {
  try {
    return execFileSync('node', ['dist/src/cli.js', ...args, '--help'], { encoding: 'utf8' });
  } catch (err) {
    return String(err.stdout ?? '');
  }
}

function parseFlags(helpText) {
  const flags = [];
  for (const line of helpText.split('\n')) {
    const m = line.match(/^\s{2}(?:-\w, )?(--[a-z][\w-]*)/);
    if (!m || m[1] === '--help') continue;
    flags.push(m[1]);
  }
  return [...new Set(flags)].sort();
}

function parseSubcommands(helpText) {
  const subs = [];
  let inCommands = false;
  for (const line of helpText.split('\n')) {
    if (/^Commands:/.test(line)) { inCommands = true; continue; }
    if (inCommands) {
      const m = line.match(/^\s{2}([a-z][\w-]+)/);
      if (m && m[1] !== 'help') subs.push(m[1]);
      else if (line.trim() === '') inCommands = false;
    }
  }
  return [...new Set(subs)].sort();
}

const surface = {};
function walk(path) {
  const helpText = runHelp(path);
  if (path.length > 0) {
    surface[path.join(' ')] = { flags: parseFlags(helpText), subcommands: [] };
  }
  for (const sub of parseSubcommands(helpText)) walk([...path, sub]);
}
walk([]);
console.log(JSON.stringify(surface, null, 2));
