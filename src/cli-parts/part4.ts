import type { Command } from 'commander';
import { registerCliPart4a } from './part4a.js';
import { registerCliPart4b } from './part4b.js';
import { registerCliPart4c } from './part4c.js';
import { registerCliPart4d } from './part4d.js';
import { registerCliPart4e } from './part4e.js';

export function registerCliPart4(program: Command): void {
  registerCliPart4a(program);
  registerCliPart4b(program);
  registerCliPart4c(program);
  registerCliPart4d(program);
  registerCliPart4e(program);
}
