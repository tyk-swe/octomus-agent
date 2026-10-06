import { defineConfig, devices } from '@playwright/test';
export default defineConfig({
  testDir: './tests',
  fullyParallel: true,
  workers: 4,
  use: { baseURL: 'http://127.0.0.1:4299', trace: 'retain-on-failure' },
  webServer: {
    command: 'python3 ../tests/serve_ui.py',
    url: 'http://127.0.0.1:4299/healthz',
    reuseExistingServer: false,
    timeout: 120000,
    gracefulShutdown: { signal: 'SIGINT', timeout: 10000 }
  },
  projects: [
    {
      name: 'desktop',
      use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 1100 } }
    },
    // Phone emulation reruns only the specs that check layout, touch and navigation on a small screen.
    {
      name: 'mobile',
      grep: /@responsive/,
      use: { ...devices['iPhone 13'], defaultBrowserType: 'chromium' }
    }
  ]
});
