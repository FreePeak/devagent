import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    include: ['test/**/*.test.ts'],
    environment: 'node',
    // Git-subprocess tests (e2e, rebase-stack, state-mirror) spawn real git
    // worktrees; under full-suite parallelism they can exceed the 5s default.
    testTimeout: 30_000,
    hookTimeout: 30_000,
  },
});
