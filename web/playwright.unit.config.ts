import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: './tests',
  testMatch: ['**/evidence.spec.ts', '**/control-eligibility.spec.ts'],
  fullyParallel: true,
  workers: 4
});
