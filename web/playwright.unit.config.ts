import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: './tests',
  testMatch: [
    '**/dashboard-helpers.spec.ts',
    '**/evidence.spec.ts',
    '**/control-eligibility.spec.ts',
    '**/cycle-history.spec.ts',
    '**/resource.spec.ts',
    '**/configuration-draft.spec.ts'
  ],
  fullyParallel: true,
  workers: 4
});
