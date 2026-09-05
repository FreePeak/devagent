import type { Command } from 'commander';
import { registerCliPart1a } from './part1a.js';
import { registerCliPart1b } from './part1b.js';

export function registerCliPart1(program: Command): void {
  registerCliPart1a(program);
  registerCliPart1b(program);
}
