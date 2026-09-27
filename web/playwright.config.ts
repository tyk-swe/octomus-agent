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
    // The fixture compiles tests/fixturedb with `go run`; a cold build cache needs longer.
    timeout: 120000,
    // SIGINT lets serve_ui.py stop the service and remove its temporary state directory;
    // without it Playwright SIGKILLs the process group and the directory is left behind.
    gracefulShutdown: { signal: 'SIGINT', timeout: 10000 }
  },
  projects: [
    {
      name: 'desktop',
      use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 1100 } }
    },
    { name: 'mobile', use: { ...devices['iPhone 13'], defaultBrowserType: 'chromium' } }
  ]
});
