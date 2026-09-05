#!/usr/bin/env node
import { Command } from 'commander';
import { wireFrSimple } from './commands/fr-simple-wire.js';
import { DEVAGENT_VERSION } from './version.js';

/**
 * Temporary thin entry for #144 CI: register the command shells that
 * `wireFrSimple` overlays (init / validate / ledger / queue list).
 * Full main cli.ts restore follows once a large-file push path is available.
 */
const program = new Command();

program
  .name('devagent')
  .description('Autonomous backend delivery agent: ticket to tested PR')
  .version(DEVAGENT_VERSION);

program
  .command('init')
  .description('Guided setup')
  .option('--repo <path>', 'repository to set up', process.cwd())
  .option('--worker <name>', 'worker CLI')
  .option('--model <id>', 'model id')
  .action(async () => {
    /* replaced by wireFrSimple */
  });

program
  .command('validate')
  .description('Run gates')
  .option('--worktree <path>', 'path', process.cwd())
  .action(async () => {
    /* replaced by wireFrSimple */
  });

program
  .command('ledger')
  .description('Show orchestration ledger')
  .option('--repo <path>', 'repo', process.cwd())
  .option('--task <id>', 'task id filter')
  .option('--summary', 'summary card', false)
  .option('--clusters [n]', 'failure clusters')
  .action(async () => {
    /* replaced by wireFrSimple */
  });

const queue = program.command('queue').description('Task queue');
queue
  .command('list')
  .description('List queued tasks')
  .option('--repo <path>', 'repo', process.cwd())
  .option('--status <status>', 'filter')
  .option('--json', 'JSON', false)
  .action(async () => {
    /* replaced by wireFrSimple */
  });

wireFrSimple(program);
program.parseAsync();
