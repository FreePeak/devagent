#!/usr/bin/env node
import { Command } from 'commander';
import { wireFrSimple } from './commands/fr-simple-wire.js';
import { DEVAGENT_VERSION } from './version.js';
import { registerCliPart0 } from './cli-parts/part0.js';
import { registerCliPart1 } from './cli-parts/part1.js';
import { registerCliPart2 } from './cli-parts/part2.js';
import { registerCliPart3 } from './cli-parts/part3.js';
import { registerCliPart4 } from './cli-parts/part4.js';
import { registerCliPart5 } from './cli-parts/part5.js';
import { registerCliPart6 } from './cli-parts/part6.js';

const program = new Command();

program
  .name('devagent')
  .description('Autonomous backend delivery agent: ticket to tested PR')
  .version(DEVAGENT_VERSION);

registerCliPart0(program);
registerCliPart1(program);
registerCliPart2(program);
registerCliPart3(program);
registerCliPart4(program);
registerCliPart5(program);
registerCliPart6(program);

wireFrSimple(program);
program.parseAsync();
