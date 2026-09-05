import type { Command } from 'commander';
import { registerCliPart2a } from './part2a.js';
import { registerCliPart2b } from './part2b.js';

export function registerCliPart2(program: Command): void {
  registerCliPart2a(program);
  registerCliPart2b(program);
}
