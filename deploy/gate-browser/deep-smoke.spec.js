// Headless deep-smoke tier (deployment-v2 #658). Catches what the API-level gate
// (deploy/rollouts/analysis/deploy-gate.yaml) structurally can't: runtime JS /
// console errors, the app getting stuck "initializing", and the first-run
// walkthrough failing to complete -- the exact class that went front-door-green
// while the app was broken (#255/#259/#624).
//
// Env contract (all overridable; defaults target staging):
//   GATE_BASE_URL            SPA origin (default https://app.staging.copresent.ai)
//   GATE_AUTH_TOKEN          a user access token to seed an authenticated session
//   GATE_AUTH_STORAGE_KEY    localStorage key the SPA reads the token from
//                            (default "memql.auth.token"; confirm against the SPA)
//   GATE_DASHBOARD_SELECTOR  selector proving the app reached the dashboard
//                            (default [data-testid="dashboard"])
//   GATE_INITIALIZING_TEXT   text shown while stuck initializing (default "Initializing")
//   GATE_WALKTHROUGH_DONE_SELECTOR  optional: selector asserting the first-run
//                            walkthrough completed (skipped if unset)
//   GATE_CONSOLE_ERROR_ALLOW  comma-separated substrings of console errors to ignore
//
// The auth/selectors are env-driven because they are CoPresent UI contracts; the
// console-error + not-stuck-initializing checks are SPA-agnostic. See README.md.
const { test, expect } = require('@playwright/test');

const AUTH_TOKEN = process.env.GATE_AUTH_TOKEN || '';
const AUTH_KEY = process.env.GATE_AUTH_STORAGE_KEY || 'memql.auth.token';
const DASHBOARD_SEL = process.env.GATE_DASHBOARD_SELECTOR || '[data-testid="dashboard"]';
const INITIALIZING_TEXT = process.env.GATE_INITIALIZING_TEXT || 'Initializing';
const WALKTHROUGH_DONE_SEL = process.env.GATE_WALKTHROUGH_DONE_SELECTOR || '';
const ERROR_ALLOW = (process.env.GATE_CONSOLE_ERROR_ALLOW || '')
  .split(',').map((s) => s.trim()).filter(Boolean);

function seedAuth(context, baseURL) {
  if (!AUTH_TOKEN) return Promise.resolve();
  // Inject the token before any app script runs, so the SPA boots authenticated.
  return context.addInitScript(
    ([key, token]) => { try { window.localStorage.setItem(key, token); } catch (_) {} },
    [AUTH_KEY, AUTH_TOKEN],
  );
}

test('SPA boots clean, reaches the dashboard, no console errors', async ({ page, context, baseURL }) => {
  const consoleErrors = [];
  page.on('console', (msg) => {
    if (msg.type() !== 'error') return;
    const text = msg.text();
    if (ERROR_ALLOW.some((a) => text.includes(a))) return;
    consoleErrors.push(text);
  });
  const pageErrors = [];
  page.on('pageerror', (err) => pageErrors.push(String(err)));

  await seedAuth(context, baseURL);

  const resp = await page.goto('/', { waitUntil: 'domcontentloaded' });
  expect(resp, 'navigation response').toBeTruthy();
  expect(resp.status(), `GET / status`).toBeLessThan(400);

  // The app must REACH the dashboard, not sit on the initializing screen.
  await expect(
    page.locator(DASHBOARD_SEL),
    `dashboard (${DASHBOARD_SEL}) should appear -- app must not be stuck "${INITIALIZING_TEXT}"`,
  ).toBeVisible({ timeout: Number(process.env.GATE_DASHBOARD_TIMEOUT_MS || 30000) });

  await expect(
    page.getByText(INITIALIZING_TEXT, { exact: false }),
    'app must not remain on the initializing screen',
  ).toHaveCount(0);

  // No runtime JS errors compiled into the bundle, no console errors at boot.
  expect(pageErrors, `uncaught page errors:\n${pageErrors.join('\n')}`).toEqual([]);
  expect(consoleErrors, `console.error during boot:\n${consoleErrors.join('\n')}`).toEqual([]);
});

test('first-run walkthrough completes', async ({ page, context }) => {
  test.skip(!WALKTHROUGH_DONE_SEL, 'GATE_WALKTHROUGH_DONE_SELECTOR not set');
  await seedAuth(context);
  await page.goto('/', { waitUntil: 'domcontentloaded' });
  await expect(
    page.locator(WALKTHROUGH_DONE_SEL),
    `walkthrough completion marker (${WALKTHROUGH_DONE_SEL})`,
  ).toBeVisible({ timeout: Number(process.env.GATE_WALKTHROUGH_TIMEOUT_MS || 45000) });
});
