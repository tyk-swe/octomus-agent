import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: './site-tests',
  outputDir: './test-results/site',
  workers: 1,
  fullyParallel: false,
  use: {
    baseURL: 'http://127.0.0.1:4310/octomus-agent/',
    trace: 'retain-on-failure'
  },
  webServer: {
    command: 'python3 ../tests/serve_site.py',
    url: 'http://127.0.0.1:4310/octomus-agent/',
    reuseExistingServer: false
  },
  projects: [
    {
      name: 'desktop',
      use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 1000 } }
    },
    { name: 'mobile', use: { ...devices['iPhone 13'], defaultBrowserType: 'chromium' } }
  ]
});
