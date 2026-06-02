// Playwright config for the headless deep-smoke tier (deployment-v2 #658).
// Single chromium project, no retries (a gate must be deterministic), CI-friendly
// reporters. The base URL + auth are env-driven (see deep-smoke.spec.js / README).
const { defineConfig, devices } = require('@playwright/test');

module.exports = defineConfig({
  testDir: '.',
  testMatch: '*.spec.js',
  timeout: Number(process.env.GATE_TEST_TIMEOUT_MS || 60000),
  expect: { timeout: Number(process.env.GATE_EXPECT_TIMEOUT_MS || 15000) },
  retries: 0,
  reporter: [['list'], ['json', { outputFile: 'gate-report.json' }]],
  use: {
    baseURL: process.env.GATE_BASE_URL || 'https://app.staging.copresent.ai',
    headless: true,
    ignoreHTTPSErrors: false,
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
});
