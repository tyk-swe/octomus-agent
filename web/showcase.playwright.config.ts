import { defineConfig, devices } from '@playwright/test';
export default defineConfig({
  testDir: './showcase-tests',
  testMatch: '*.spec.ts',
  workers: 1,
  fullyParallel: false,
  use: { baseURL: 'http://127.0.0.1:4307/showcase/', trace: 'retain-on-failure' },
  webServer: {
    command: 'python3 -m http.server 4307 --bind 127.0.0.1 --directory ../dist',
    url: 'http://127.0.0.1:4307/',
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
