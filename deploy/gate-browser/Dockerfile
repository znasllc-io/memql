# Headless deep-smoke tier image: acrmemql.azurecr.io/deploy-gate-browser
# (deployment-v2 #658). Bundles Playwright + Chromium + the spec, so the gate can
# load the SPA, assert zero console errors, and confirm the app reaches the
# dashboard / the walkthrough completes. Run as a SEPARATE gate job (not inline)
# per the #658 latency-vs-coverage decision (see README.md).
#
# Build (from this directory):
#   docker build -t acrmemql.azurecr.io/deploy-gate-browser:<tag> .
#   # or: az acr build --registry acrmemql --image deploy-gate-browser:<tag> \
#   #        deploy/gate-browser
FROM mcr.microsoft.com/playwright:v1.48.2-jammy
WORKDIR /gate
COPY package.json ./
RUN npm install --no-audit --no-fund
COPY playwright.config.js deep-smoke.spec.js ./
ENTRYPOINT ["npx", "playwright", "test"]
