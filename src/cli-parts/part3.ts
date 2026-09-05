import type { Command } from 'commander';
import { registerCliPart3a } from './part3a.js';
import { registerCliPart3b } from './part3b.js';
import { registerCliPart3c } from './part3c.js';

export function registerCliPart3(program: Command): void {
  registerCliPart3a(program);
  registerCliPart3b(program);
  registerCliPart3c(program);
}
