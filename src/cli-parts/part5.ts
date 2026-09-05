import type { Command } from 'commander';
import { registerCliPart5a } from './part5a.js';
import { registerCliPart5b } from './part5b.js';
import { registerCliPart5c } from './part5c.js';

export function registerCliPart5(program: Command): void {
  registerCliPart5a(program);
  registerCliPart5b(program);
  registerCliPart5c(program);
}
