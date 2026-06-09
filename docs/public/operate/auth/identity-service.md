---
title: Identity Service (Operator Guide)
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Identity Service (Operator Guide)

`component/identity` is the in-house authentication provider for
the cluster. It runs as its own node-type binary
(`go build -tags identity .` or `make identity`) and owns:

- The public auth web pages (login, registration, magic-link,
  legal docs, `/me/*` self-service).
- The OAuth-style token endpoints (`/oauth/token`, `/auth/refresh`).
- The admin web app at `/admin/*` (users, sessions, audit, JWKS,
  cluster settings, partition management).
- The JWKS feed at `/.well-known/jwks.json` that every other node
  binary fetches to verify access tokens.
- The Personal Access Token (PAT) layer for CLI clients.

This document covers the operator-side narrative: what to set,
what to watch, how to rotate keys.

## Topology

A cluster runs:

- **One** `identity` binary -- holds the signing key on disk,
  publishes JWKS, mints + verifies tokens.
- **Many** other binaries (bff / voice / cognition / agent /
  planner) -- pull the public JWKS and verify incoming JWTs
  locally. They never see the private key.

CLI clients (`memql-cockpit`, custom tooling) authenticate against
the identity binary directly using `mql_pat_<...>` PATs. Browser
clients (CoPresent, the identity web app itself) authenticate via
magic link, then carry the resulting access JWT to bff/voice/etc.

### Browser-side routing of identity XHR

Browsers send `/auth/refresh`, `/auth/logout`, `/oauth/token`, and
`/.well-known/jwks.json` SAME-ORIGIN through the SPA's host (e.g.
`app.${DOMAIN}`), which the LB nginx proxies internally to the
identity binary. Top-level magic-link redirects (the `/login` UI,
the `/auth/callback` redirect-back) still go to
`identity.${DOMAIN}` directly.

Same-origin XHR avoids a Safari quirk where cross-origin fetch
to a sibling host that shares a wildcard cert + IP can be refused
intermittently with "TypeError: Load failed" / "Could not connect
to the server" -- HTTP/2 connection coalescing biting on
cookie-bound XHR-with-credentials. Routing the four XHR endpoints
through the SPA's own origin sidesteps the entire class of
issues. The dev cluster's nginx template
(`docker/nginx/templates/default.conf.template`) has explicit
`location =` blocks for each path; production setups should
mirror the same routing.

## Required environment variables

Identity-tagged binary:

| Variable                           | Required                  | Purpose                                                                                |
|------------------------------------|---------------------------|----------------------------------------------------------------------------------------|
| `IDENTITY_ENABLED`                 | yes (`true`)              | Gates the whole service.                                                               |
| `IDENTITY_BASE_URL`                | yes                       | Public origin (e.g. `https://auth.example.com`). Used as JWT `iss` and email links.    |
| `IDENTITY_KEY_DIR`                 | recommended               | Where the on-disk Ed25519 key files live. Default `var/identity/keys`.                 |
| `IDENTITY_KEY_ENCRYPTION_KEY`      | yes in non-localhost prod | Master secret (>=16 bytes) wrapping the on-disk private key.                           |
| `IDENTITY_REGISTERED_CLIENTS`      | yes for production        | JSON array of `{clientId, redirectURIs[]}` -- explicit, no wildcards.                  |
| `IDENTITY_REGISTRATION_MODE`       | recommended               | `open` / `domain_restricted` / `invite_only` / `waitlist`. Default `open`.             |
| `IDENTITY_INTERNAL_DOMAINS`        | recommended               | Comma-separated. Matches assign `internal=true` + `INTERNAL_DEFAULT_ROLE`.             |
| `IDENTITY_INTERNAL_DEFAULT_ROLE`   | recommended               | `owner` / `admin` / `writer` / `reader`. Default `writer`.                             |
| `IDENTITY_BRAND_NAME`              | recommended               | Subject prefix on outbound emails + admin UI title.                                    |

Other nodes (bff / voice / cognition / agent / planner):

| Variable                                   | Required           | Purpose                                                                |
|--------------------------------------------|--------------------|------------------------------------------------------------------------|
| `IDENTITY_VERIFIER_BASE_URL`               | yes for prod auth  | Public identity origin. Verifier fetches `${BASE}/.well-known/jwks.json`. |
| `IDENTITY_VERIFIER_AUDIENCE`               | recommended        | JWT `aud` value. Default `memql`.                                      |
| `IDENTITY_VERIFIER_EXPECTED_ISSUER`        | optional           | Override JWT `iss`. Defaults to `BASE_URL`.                            |
| `IDENTITY_VERIFIER_JWKS_REFRESH_SECONDS`   | optional           | Background refresh cadence. Default 300 (5 min).                       |
| `IDENTITY_VERIFIER_JWKS_FETCH_TIMEOUT_SECONDS` | optional       | Per-fetch HTTP timeout. Default 10.                                    |
| `IDENTITY_VERIFIER_JWKS_URL`               | optional           | Override the JWKS URL when internal-mesh routing differs from public.  |

Leaving `IDENTITY_VERIFIER_BASE_URL` unset on a non-identity node
boots it in **dev no-auth mode**: the synthetic `local-dev` admin
identity is stamped on every request. Never enable this in
production.

## Optional anti-abuse knobs

| Variable                                          | Default | Effect                                                  |
|---------------------------------------------------|---------|---------------------------------------------------------|
| `IDENTITY_RATE_LIMIT_PER_IP_PER_HOUR`             | 10      | Caps magic-link / access-request submissions per IP.    |
| `IDENTITY_RISK_THRESHOLD`                         | 50      | 0-100; lower = stricter. Blocks at-or-above the score.  |
| `IDENTITY_DISPOSABLE_EMAIL_BLOCKLIST_ENABLED`     | true    | Toggles the embedded blocklist.                         |
| `IDENTITY_MX_VALIDATION_ENABLED`                  | true    | Toggles per-domain MX-record DNS check.                 |
| `IDENTITY_TURNSTILE_SITE_KEY`                     | empty   | Optional Cloudflare Turnstile site key.                 |
| `IDENTITY_TURNSTILE_SECRET`                       | empty   | Optional Cloudflare Turnstile secret.                   |

Each rejection emits an audit event with `category=auth`,
`action=magic_link_blocked`, and a `failureReason` matching the
specific defense (`rate_limit` / `disposable_email` / `mx_invalid`
/ `turnstile` / `risk_threshold`). Surface these in your log
pipeline to tune thresholds.

## Token + session lifetimes

| Variable                                | Default      | Notes                                                              |
|-----------------------------------------|--------------|--------------------------------------------------------------------|
| `IDENTITY_ACCESS_TOKEN_TTL_SECONDS`     | 900 (15 min) | Short by design -- limits XSS blast radius.                        |
| `IDENTITY_REFRESH_TOKEN_TTL_SECONDS`    | 2,592,000    | Absolute lifetime. Idle/max-age policies enforce earlier expiry.   |
| `IDENTITY_MAGIC_LINK_TTL_SECONDS`       | 600          | 10 min.                                                            |
| `IDENTITY_INVITATION_TTL_DAYS`          | 7            | Admin-issued user invitations.                                     |
| `IDENTITY_SESSION_IDLE_DAYS`            | 14           | Refresh fails if `lastRefreshedAt + idle < now`.                   |
| `IDENTITY_SESSION_MAX_DAYS`             | 90           | Refresh fails if `firstAuthenticatedAt + max < now`.               |

### Refresh-token rotation grace window

The rotator persists each session's IMMEDIATELY-PREVIOUS refresh
hash in `previousRefreshTokenHash` + `previousRotatedAt` and
accepts that hash for 30 seconds after rotation. Covers the case
where the SPA hard-refreshes mid-rotation -- the server has
already minted the new pair and updated the cookie hash on disk,
but the browser aborted before consuming the `Set-Cookie`. The
new page's first `/auth/refresh` lands with the OLD cookie; the
rotator falls back to the previous-hash lookup, accepts it inside
the grace window, and rotates again. Without this, every rapid
hard-reload bounced the user to `/login`. Window is hard-coded
in `component/identity/refresh/rotate.go`
(`previousRefreshGraceWindow = 30 * time.Second`).

## Email delivery

The identity service composes emails (magic link, invitation,
admin notifications) and hands them to the `email` integration
plug-in. Configure exactly one sender:

- **Microsoft Graph** (`AZURE_TENANT_ID` + `AZURE_CLIENT_ID` +
  `AZURE_CLIENT_SECRET` + `MAIL_SENDER`) -- preferred.
- **SMTP fallback** (`SMTP_HOST` + `SMTP_PORT` + `SMTP_USERNAME` +
  `SMTP_PASSWORD` + `SMTP_FROM_ADDR`).
- **LogSender** -- both unset; emails are written to the slog
  stream. Dev only.

Branding controls (`IDENTITY_BRAND_NAME`,
`IDENTITY_BRAND_PRIMARY_COLOR`, `IDENTITY_BRAND_LOGO_DATA_URI`)
flow into all outbound templates.

## Key management

Ed25519 signing keys live in `IDENTITY_KEY_DIR`:

- `jwt-current.ed25519` -- the active signing key.
- `jwt-previous.ed25519` -- present only during the rotation
  overlap window. Retiring kid stays in JWKS so in-flight tokens
  still verify.

Files are 0600, the directory 0700. With
`IDENTITY_KEY_ENCRYPTION_KEY` set, the private bytes are wrapped
in AES-256-GCM with an Argon2id-derived key (32 MiB, t=2). With
the env var unset (dev only), private bytes are plaintext.

`Config.Validate()` enforces "encryption-at-rest is mandatory in
production": if `IDENTITY_BASE_URL` is not a localhost origin and
`IDENTITY_KEY_ENCRYPTION_KEY` is empty, startup fails. Don't try
to defeat the guard.

### Rotation

Two paths:

- **Cron**: a goroutine triggers `KeyManager.Rotate` every
  `IDENTITY_KEY_ROTATION_DAYS` (default 90). The retired key
  stays in JWKS for `IDENTITY_JWKS_OVERLAP_HOURS` (default 24).
- **Admin "Rotate now"**: button in the admin UI's JWKS panel
  calls `Service.RotateNow`. Same code path; same overlap.

The retired key is hard-removed by the rotation goroutine's sweep
once `RetiresAt < now`. Other nodes pick up the new kid on the
next JWKS background refresh (every 5 min by default), or on
demand when they encounter a token signed under an unknown kid.

### Recovery

If `IDENTITY_KEY_ENCRYPTION_KEY` is rotated incorrectly (the new
secret can't decrypt the old envelope), the binary fails to load
the key files at startup with a clear AES-GCM error. Restore the
original secret from your secret store, redeploy, then perform a
proper rotation: stand up the new secret, call "Rotate now" so a
fresh key is minted under it, and only then retire the old
secret.

## Dev quick-start

The full local stack runs via `docker compose -f
docker/docker-compose.full.yml up --build` -- see
[docs/public/overview/quickstart.md](../../overview/quickstart.md) for the prerequisite wildcard
DNS record (`*.${IDENTITY_BOOTSTRAP_DOMAIN}` -> `127.0.0.1`,
default `*.local.znas.io`) and the mkcert TLS setup
(`make setup-tls`). The compose stack already wires identity at
`https://identity.${IDENTITY_BOOTSTRAP_DOMAIN}` with every other
node verifying against that issuer.

For running the binaries standalone (no docker), set the same URL:

```bash
# Identity binary
IDENTITY_ENABLED=true \
IDENTITY_BASE_URL=https://identity.local.znas.io \
IDENTITY_REGISTRATION_MODE=open \
make identity-assets identity
./bin/memql-identity

# bff binary, points at the identity binary above
IDENTITY_VERIFIER_BASE_URL=http://identity:8081 \
IDENTITY_VERIFIER_EXPECTED_ISSUER=https://identity.local.znas.io \
make bff
./bin/memql-bff
```

Without `IDENTITY_VERIFIER_BASE_URL` the bff boots into no-auth
dev mode; convenient for solo development that doesn't need real
tokens, but the synthetic `local-dev` admin identity will be on
every request.

## Health + observability

- `GET /healthz` on the identity binary returns 200 once the key
  manager has loaded.
- `GET /.well-known/jwks.json` always reflects the current
  PublicKeySet (current + retiring during overlap).
- The admin UI's JWKS panel shows live key metadata (kid,
  createdAt, retiresAt) and the rotation cadence.
- Audit events for every auth lifecycle moment land in
  `v1:identity:auditEvent` (in addition to slog). Retention is
  controlled by `IDENTITY_AUDIT_LOG_RETENTION_DAYS` (default 365).

## Related

- [access-model.md](access-model.md) -- enforcement layers, role
  spectrum, the wire-side lifecycle.
- [user-provisioning.md](user-provisioning.md) -- registration
  modes, magic-link flow, invitations.
- `component/identity/CLAUDE.md` -- per-package developer guide.
