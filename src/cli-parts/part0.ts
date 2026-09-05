import type { Command } from 'commander';
import { registerCliPart0a } from './part0a.js';
import { registerCliPart0b } from './part0b.js';
import { registerCliPart0c } from './part0c.js';

export function registerCliPart0(program: Command): void {
  registerCliPart0a(program);
  registerCliPart0b(program);
  registerCliPart0c(program);
}
