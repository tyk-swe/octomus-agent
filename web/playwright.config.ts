import { defineConfig, devices } from '@playwright/test';
export default defineConfig({
  testDir: './tests',
  fullyParallel: false,
  workers: 1,
  use: { baseURL: 'http://127.0.0.1:4299', trace: 'retain-on-failure' },
  webServer: {
    command: 'python3 ../tests/serve_ui.py',
    url: 'http://127.0.0.1:4299/healthz',
    reuseExistingServer: false,
    timeout: 30000
  },
  projects: [
    {
      name: 'desktop',
      use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 1100 } }
    },
    { name: 'mobile', use: { ...devices['iPhone 13'], defaultBrowserType: 'chromium' } }
  ]
});
