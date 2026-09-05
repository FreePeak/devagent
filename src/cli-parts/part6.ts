import type { Command } from 'commander';
import { registerCliPart6a } from './part6a.js';
import { registerCliPart6b } from './part6b.js';
import { registerCliPart6c } from './part6c.js';

export function registerCliPart6(program: Command): void {
  registerCliPart6a(program);
  registerCliPart6b(program);
  registerCliPart6c(program);
}
