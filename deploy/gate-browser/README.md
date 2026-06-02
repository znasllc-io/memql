# Headless deep-smoke tier (deployment-v2 #658)

A Playwright/Chromium tier that catches what the API-level gate
(`deploy/rollouts/analysis/deploy-gate.yaml`) structurally cannot: **runtime JS /
`console.error`**, the app **stuck "initializing"**, and the **first-run
walkthrough** not completing — the exact class that went front-door-green while
the app was broken (#255 / copresent#259 / #624).

## Checks (`deep-smoke.spec.js`)

1. SPA boots and **reaches the dashboard** (not stuck on the initializing screen).
2. **Zero `console.error`** and zero uncaught page errors during boot.
3. (optional) the **first-run walkthrough completes** — enabled when
   `GATE_WALKTHROUGH_DONE_SELECTOR` is set.

## Resolved design questions (from #658)

- **Run as a SEPARATE job, not inline in the Rollout's prePromotionAnalysis.**
  The browser tier is slower (Chromium boot + walkthrough) than the API gate, so
  blocking every promotion on it hurts rollout latency. It runs as its own
  `AnalysisRun` (`deploy/rollouts/analysis/browser-gate.yaml`) that the operator
  attaches to a promotion when they want the deeper coverage, and which also runs
  post-deploy feeding the promotion record. The fast API gate
  (`deploy-gate.yaml`) stays the inline auto-abort gate.
- **Test identity = a short-lived USER access token**, injected into the SPA's
  token storage before boot (`GATE_AUTH_TOKEN` + `GATE_AUTH_STORAGE_KEY`). NOT
  the `service_account` JWT (#691) — that authenticates the BFF *gRPC* surface,
  whereas the browser needs a real user *session*. Mint a short-lived user token
  for a dedicated test user (mirrors the identity issuer; rotate per run).

## Env contract

| Var | Default | Meaning |
|---|---|---|
| `GATE_BASE_URL` | `https://app.staging.copresent.ai` | SPA origin |
| `GATE_AUTH_TOKEN` | — | user access token to seed an authenticated session |
| `GATE_AUTH_STORAGE_KEY` | `memql.auth.token` | localStorage key the SPA reads the token from |
| `GATE_DASHBOARD_SELECTOR` | `[data-testid="dashboard"]` | proves the dashboard rendered |
| `GATE_INITIALIZING_TEXT` | `Initializing` | the stuck-initializing text to assert absent |
| `GATE_WALKTHROUGH_DONE_SELECTOR` | — | optional walkthrough-complete marker |
| `GATE_CONSOLE_ERROR_ALLOW` | — | comma-sep substrings of console errors to ignore |

> The auth key + selectors are **CoPresent UI contracts** — confirm them against
> the live SPA at bring-up (defaults are reasonable placeholders). The
> console-error and not-stuck-initializing checks are SPA-agnostic and work as-is.

## Run

```bash
# Locally:
cd deploy/gate-browser && npm install && npx playwright install --with-deps chromium
GATE_BASE_URL=https://app.staging.copresent.ai GATE_AUTH_TOKEN=<user-jwt> npm test

# As the image (built on demand; ACR push rides the #702 image pipeline):
az acr build --registry acrmemql --image deploy-gate-browser:<tag> deploy/gate-browser
```

The Playwright image is heavy, so it is built on demand (`az acr build`) rather
than on every PR; the digest is recorded with the per-repo image pipeline (#702).
