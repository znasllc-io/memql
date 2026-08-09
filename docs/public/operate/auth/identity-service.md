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
- What is left of the admin web app at `/admin/*`: the sign-in
  pages and `/admin/deployments`. Its other six screens (users,
  tokens, audit, JWKS, cluster settings, and the dashboard) moved
  into the memQL portal in memql#3324, writes and owner/admin gate
  together -- see [../portal.md](../portal.md). Deployments stayed
  because `DeployControlService` runs against an on-disk overlay
  checkout and therefore exists only on this node.
- The JWKS feed at `/.well-known/jwks.json` that every other node
  binary fetches to verify access tokens.
- The Personal Access Token (PAT) layer for CLI clients.
- The WebAuthn passkey ceremonies, the single-use enrolment link, and
  the RFC 8628 device authorization grant -- the paths that let someone
  sign in without a mailbox. Which one to reach for is
  [sign-in-paths.md](sign-in-paths.md).

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
clients (the product SPA, the identity web app itself) authenticate
via magic link, then carry the resulting access JWT to bff/voice/etc.

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
issues. A front-door nginx should carry explicit `location =`
blocks for each of the four paths; production setups should
mirror the same routing.

## Endpoints

The identity binary mounts two route sets: the JSON auth-flow API
(`component/identity/http`, `Server.Mount`) and the server-rendered web
UI (`component/identity/web`). `/.well-known/jwks.json` is mounted by
`component/identity.Service` directly.

### Auth-flow API

| Route | Auth | Purpose |
|---|---|---|
| `POST /auth/magic-link` | none | Issue a magic link (or an access request). The one route wrapped in the anti-abuse stack |
| `GET /auth/complete` | the link | Consume a magic link, redirect to the client with an auth code |
| `POST /oauth/token` | none | Redeem an auth code, a refresh token, or a `device_code` for tokens |
| `POST /auth/refresh` | refresh token | Rotate the refresh token |
| `POST /auth/logout` | session | Revoke the current session |
| `POST /register` | none | RFC 7591 dynamic client registration. Gated on `MEMQL_IDENTITY_OAUTH_DCR_ENABLED` (default true) |
| `POST /auth/webauthn/register/{begin,finish}` | Bearer JWT **or** `Authorization: Enrolment mql_enr_<...>` | Passkey registration (memql#3406, enrolment authorization memql#3408) |
| `POST /auth/webauthn/login/{begin,finish}` | none -- this pair IS the authentication | Usernameless passkey login; ends in the same auth code a magic link produces (memql#3407) |
| `POST /device/code` | none | RFC 8628 device authorization request (memql#3410). Answers `temporarily_unavailable` when no device-code store is wired |
| `POST /pair/codes` | Bearer JWT | Mint a worker-pairing code |
| `POST /pair/redeem` | `Authorization: Pair <code>` | Redeem a pairing code for a worker token |
| `POST /auth/badge/grant` | Bearer JWT | Exchange a registered badge id for a short-lived operator grant (memql#2513) |
| `POST /node/bootstrap` | `Authorization: Bootstrap <secret>` | Self-mint a `class="node"` JWT. Off unless `MEMQL_NODE_BOOTSTRAP_TOKEN` is set |

Every route above also answers `OPTIONS` for CORS, and every one is wrapped in
the security-headers middleware.

### Web UI

| Route | Auth | Purpose |
|---|---|---|
| `GET`/`POST /login` | none | Email-first sign-in. Carries the "Sign in with a passkey" control when a relying party is in scope |
| `GET`/`POST /setup` | none, pre-bootstrap only | First-owner wizard. 404s once any user exists |
| `GET /authorize`, `/check-email`, `/logout-complete`, `/error` | none | Flow pages |
| `GET /legal/tos`, `/legal/privacy` | none | Legal pages |
| `GET /enroll?code=mql_enr_<...>` | the enrolment token | Redeem an enrolment link and render the passkey registration page. Mounts only when the validator and audit sink are both wired (memql#3408) |
| `GET`/`POST /device` | signed-in user | RFC 8628 verification page. A signed-out visitor is bounced through `/login` and returned here with the code. Mounts only when the device flow is wired (memql#3410) |
| `GET /me/`, `/me/settings`, `/me/devices`, `/me/export`, `/me/deletion-pending` | client-side | Self-service shells. `/me/devices` lists **sessions**, not passkeys |
| `GET`/`POST /me/tokens`, `POST /me/tokens/revoke` | Bearer or admin cookie | PAT issuance and revocation. Mounts only when the PAT adapter is wired |
| `GET /admin/*` | admin session | The sign-in pages and `/admin/deployments`; the rest moved to the portal |
| `GET /static/` | none | Cached UI assets |

## Required environment variables

Identity-tagged binary:

| Variable                           | Required                  | Purpose                                                                                |
|------------------------------------|---------------------------|----------------------------------------------------------------------------------------|
| `MEMQL_IDENTITY_ENABLED`                 | yes (`true`)              | Gates the whole service.                                                               |
| `MEMQL_IDENTITY_BASE_URL`                | yes                       | Public origin (e.g. `https://auth.example.com`). Used as JWT `iss` and email links.    |
| `MEMQL_IDENTITY_SIGNING_KEY_B64`         | **yes for >=2 replicas**  | Shared base64-std 32-byte Ed25519 seed (#550). Every replica derives the SAME key + kid + JWKS. REQUIRED for any multi-replica / HA deployment -- see "Key management" below. |
| `MEMQL_IDENTITY_ALLOW_EPHEMERAL_KEY`     | dev / single-node only    | Opt into per-pod ephemeral file keys (no shared seed). Default `false`. Only safe for one replica or a shared key volume. |
| `MEMQL_IDENTITY_KEY_DIR`                 | recommended               | Where the on-disk Ed25519 key files live (file-key mode). Default `var/identity/keys`. |
| `MEMQL_IDENTITY_KEY_ENCRYPTION_KEY`      | yes in non-localhost prod | Master secret (>=16 bytes) wrapping the on-disk private key (file-key mode).            |
| `MEMQL_IDENTITY_REGISTERED_CLIENTS`      | yes for production        | JSON array of `{clientId, redirectURIs[]}` -- explicit, no wildcards.                  |
| `MEMQL_IDENTITY_REGISTRATION_MODE`       | recommended               | `open` / `domain_restricted` / `invite_only` / `waitlist`. Default `open`.             |
| `MEMQL_IDENTITY_INTERNAL_DOMAINS`        | recommended               | Comma-separated. Matches assign `internal=true` + `INTERNAL_DEFAULT_ROLE`.             |
| `MEMQL_IDENTITY_INTERNAL_DEFAULT_ROLE`   | recommended               | `owner` / `admin` / `writer` / `reader`. Default `writer`.                             |
| `MEMQL_IDENTITY_BRAND_NAME`              | recommended               | Subject prefix on outbound emails + admin UI title.                                    |

Other nodes (bff / voice / cognition / agent / planner):

| Variable                                   | Required           | Purpose                                                                |
|--------------------------------------------|--------------------|------------------------------------------------------------------------|
| `MEMQL_IDENTITY_VERIFIER_BASE_URL`               | yes for prod auth  | Public identity origin. Verifier fetches `${BASE}/.well-known/jwks.json`. |
| `MEMQL_IDENTITY_VERIFIER_AUDIENCE`               | recommended        | JWT `aud` value. Default `memql`.                                      |
| `MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER`        | optional           | Override JWT `iss`. Defaults to `BASE_URL`.                            |
| `MEMQL_IDENTITY_VERIFIER_JWKS_REFRESH_SECONDS`   | optional           | Background refresh cadence. Default 300 (5 min).                       |
| `MEMQL_IDENTITY_VERIFIER_JWKS_FETCH_TIMEOUT_SECONDS` | optional       | Per-fetch HTTP timeout. Default 10.                                    |
| `MEMQL_IDENTITY_VERIFIER_JWKS_URL`               | optional           | Override the JWKS URL when internal-mesh routing differs from public.  |

Leaving `MEMQL_IDENTITY_VERIFIER_BASE_URL` unset on a non-identity node
boots it in **dev no-auth mode**: the synthetic `local-dev` admin
identity is stamped on every request. Never enable this in
production.

## Optional anti-abuse knobs

| Variable                                          | Default | Effect                                                  |
|---------------------------------------------------|---------|---------------------------------------------------------|
| `MEMQL_IDENTITY_RATE_LIMIT_PER_IP_PER_HOUR`             | 10      | Caps magic-link / access-request submissions per IP.    |
| `MEMQL_IDENTITY_RISK_THRESHOLD`                         | 50      | 0-100; lower = stricter. Blocks at-or-above the score.  |
| `MEMQL_IDENTITY_DISPOSABLE_EMAIL_BLOCKLIST_ENABLED`     | true    | Toggles the embedded blocklist.                         |
| `MEMQL_IDENTITY_MX_VALIDATION_ENABLED`                  | true    | Toggles per-domain MX-record DNS check.                 |
| `MEMQL_IDENTITY_TURNSTILE_SITE_KEY`                     | empty   | Optional Cloudflare Turnstile site key.                 |
| `MEMQL_IDENTITY_TURNSTILE_SECRET`                       | empty   | Optional Cloudflare Turnstile secret.                   |

Each rejection emits an audit event with `category=auth`,
`action=magic_link_blocked`, and a `failureReason` matching the
specific defense (`rate_limit` / `disposable_email` / `mx_invalid`
/ `turnstile` / `risk_threshold`). Surface these in your log
pipeline to tune thresholds.

## Passkey, enrolment and device-code knobs

These are read directly by their handlers rather than through
`identity.Config`, so they take effect per process with no other wiring.
All are optional; the defaults are the shipped behaviour.

| Variable | Default | Effect |
|---|---|---|
| `MEMQL_IDENTITY_PASSKEY_REGISTER_PER_HOUR` | 60 | Per-IP cap on passkey **registration** attempts. Tight because enrolling is rare and deliberate |
| `MEMQL_IDENTITY_PASSKEY_LOGIN_PER_HOUR` | 240 | Per-IP cap on passkey **login** attempts. Looser -- signing in is routine, and this pair is unauthenticated, so the bucket is the only thing between a script and an unbounded stream of challenges |
| `MEMQL_IDENTITY_ENROLL_PER_HOUR` | 120 | Per-IP cap on `GET /enroll` redeems |
| `MEMQL_IDENTITY_ALLOW_INSECURE_ENROLL` | unset | Dev-only bypass of the HTTPS requirement on `/enroll`. Named separately from `MEMQL_IDENTITY_ALLOW_INSECURE_PAIR` on purpose -- debugging worker pairing must not thereby admit plaintext enrolment links. Logs a WARN on every request that slips through |
| `MEMQL_IDENTITY_DEVICE_CODE_PER_HOUR` | 60 | Per-IP cap on `POST /device/code`. Each accepted request burns a `user_code` out of a 40-bit space |
| `MEMQL_IDENTITY_DEVICE_CODE_TTL_SECONDS` | 600 (10 min) | The device authorization window. Capped at 30 minutes |
| `MEMQL_IDENTITY_DEVICE_VERIFY_PER_HOUR` | 120 | Per-IP cap on `GET`/`POST /device`. The page is a code oracle, so this is what bounds guessing against the 40-bit space |

WARNING: every limiter in the identity tree is **in-memory and therefore
per-replica**, as is the WebAuthn challenge store. A multi-replica deployment
gets per-replica budgets, and a ceremony that lands on a different replica than
the one that minted its challenge will not find it.

Two fixed values that have no env override, stated here because operators ask:

- `MEMQL_IDENTITY_BASE_URL` is the **only** source of the WebAuthn RP ID. Its
  hostname (port stripped) is the RP ID; the full `scheme://host[:port]` is the
  expected ceremony origin. It is never derived from the request `Host` header,
  and a base URL that yields no usable RP ID makes the passkey routes refuse
  rather than guess. Changing it orphans every passkey already enrolled.
- Enrolment tokens are 15 minutes by default with a 24-hour ceiling; a longer
  request is clamped, not refused. Set per-issue (`--ttl`, or the portal's
  lifetime picker), not by env.

## Token + session lifetimes

| Variable                                | Default      | Notes                                                              |
|-----------------------------------------|--------------|--------------------------------------------------------------------|
| `MEMQL_IDENTITY_ACCESS_TOKEN_TTL_SECONDS`     | 900 (15 min) | Short by design -- limits XSS blast radius.                        |
| `MEMQL_IDENTITY_REFRESH_TOKEN_TTL_SECONDS`    | 2,592,000    | Absolute lifetime. Idle/max-age policies enforce earlier expiry.   |
| `MEMQL_IDENTITY_MAGIC_LINK_TTL_SECONDS`       | 600          | 10 min.                                                            |
| `MEMQL_IDENTITY_INVITATION_TTL_DAYS`          | 7            | Admin-issued user invitations.                                     |
| `MEMQL_IDENTITY_SESSION_IDLE_DAYS`            | 14           | Refresh fails if `lastRefreshedAt + idle < now`.                   |
| `MEMQL_IDENTITY_SESSION_MAX_DAYS`             | 90           | Refresh fails if `firstAuthenticatedAt + max < now`.               |

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

Branding controls (`MEMQL_IDENTITY_BRAND_NAME`,
`MEMQL_IDENTITY_BRAND_PRIMARY_COLOR`, `MEMQL_IDENTITY_BRAND_LOGO_DATA_URI`)
flow into all outbound templates.

## Key management

### Multi-replica: the signing key MUST be a shared seed (#1515)

The identity service has two signing-key modes:

- **Shared-seed mode (`MEMQL_IDENTITY_SIGNING_KEY_B64` set)** -- every
  replica derives the SAME Ed25519 key + `kid` from the same seed
  (#550). JWKS is coherent across all replicas, survives restarts and
  DB resets, and the deployment can run `replicas: >=2` on a rolling
  update. This is the REQUIRED posture for staging and prod.
- **File-key mode (`MEMQL_IDENTITY_SIGNING_KEY_B64` unset)** -- each pod
  reads/generates an Ed25519 key on its OWN `MEMQL_IDENTITY_KEY_DIR` (which,
  in the cluster manifest, is the pod's ephemeral container
  filesystem -- there is no shared PVC). With >=2 replicas, **each
  replica mints its own key**, so `/.well-known/jwks.json` diverges
  across replicas and ~50% of token verifications (browser sessions
  AND mesh node tokens) fail with `unknown kid`, flapping login.

> **This caused the 2026-06-16 staging auth outage.** Staging ran
> identity at `replicas: 2` but had no `MEMQL_IDENTITY_SIGNING_KEY_B64` in
> its sealed genesis envelope, so it fell into per-pod file-key mode
> and JWKS flapped between the two pods' keys. The fix is the shared
> seed (added to staging's envelope) plus the startup guard below.

**Fail-fast guard.** `Config.Validate()` REFUSES to start a deployment
that has no `MEMQL_IDENTITY_SIGNING_KEY_B64` unless the operator explicitly
opts into per-pod keys with `MEMQL_IDENTITY_ALLOW_EPHEMERAL_KEY=true`. This
converts a silent ~50% auth-failure into a loud, copy-pasteable startup
error.

**Only a loopback issuer is exempt** -- `localhost`, `127.0.0.1`, `::1`,
`0.0.0.0`, or an RFC 6761 `*.localhost` name. Those name one process on one
machine and cannot have a second replica.

> The `*.local.<domain>` dev wildcard used to be exempt too, and that was
> the hole (memql#3400). `identity.local.znas.io` is the local parity
> cluster's **front door**: it resolves to 127.0.0.1, but what listens there
> is traefik, proxying to a Service that `make scale N=2` puts two identity
> pods behind. The guard therefore stayed silent on exactly the topology it
> exists for, and a local cluster served two `kid`s while reporting the
> resulting rejections as "invalid or expired token". The dev wildcard
> remains exempt from the *encryption-at-rest* requirement, which is a
> genuine dev-vs-prod distinction; it is not exempt from this one, which is
> a one-process-vs-many distinction.

Generate a fresh seed:

```bash
make identity-signing-key      # head -c 32 /dev/urandom | base64
```

Set the result as `MEMQL_IDENTITY_SIGNING_KEY_B64` on every identity replica
-- as a key on the `memql-secrets` Secret every node `envFrom`s, or inside
the sealed genesis envelope that Secret carries (autoload is set-if-absent,
so an explicit Secret key wins). **Locally there is nothing to do**:
`scripts/k3d/seed-secrets.sh` (`make secrets`, which `make up` runs)
generates a seed when the cluster has none and reuses it verbatim on every
later run.

Rotate by writing a new seed and rolling the deployment -- automatic
rotation is disabled in shared-seed mode. Rotation invalidates every live
browser session and every minted `class="node"` mesh token, which is why
`make secrets` never rotates as a side effect of a re-run.

Use `MEMQL_IDENTITY_ALLOW_EPHEMERAL_KEY=true` only for a genuinely
single-process deployment.

**Verify it.** `make status` reads `/.well-known/jwks.json` from every
identity replica through the apiserver pod proxy and reports
`identityKeysShared`. Run it after `make scale N=2`:

```
Identity litmus: JWKS keyset per identity replica (must be identical)
  identity-5f45458d79-924h5: kid(s) ln-RlCxzK8o
  identity-5f45458d79-qpcq2: kid(s) ln-RlCxzK8o

  PASS: all 2 identity replica(s) publish the same signing keyset.
```

### File-key mode (single-node / dev)

Ed25519 signing keys live in `MEMQL_IDENTITY_KEY_DIR`:

- `jwt-current.ed25519` -- the active signing key.
- `jwt-previous.ed25519` -- present only during the rotation
  overlap window. Retiring kid stays in JWKS so in-flight tokens
  still verify.

Files are 0600, the directory 0700. With
`MEMQL_IDENTITY_KEY_ENCRYPTION_KEY` set, the private bytes are wrapped
in AES-256-GCM with an Argon2id-derived key (32 MiB, t=2). With
the env var unset (dev only), private bytes are plaintext.

`Config.Validate()` enforces "encryption-at-rest is mandatory in
production": if `MEMQL_IDENTITY_BASE_URL` is not a localhost origin and
`MEMQL_IDENTITY_KEY_ENCRYPTION_KEY` is empty, startup fails. Don't try
to defeat the guard.

### Rotation

Two paths:

- **Cron**: a goroutine triggers `KeyManager.Rotate` every
  `MEMQL_IDENTITY_KEY_ROTATION_DAYS` (default 90). The retired key
  stays in JWKS for `MEMQL_IDENTITY_JWKS_OVERLAP_HOURS` (default 24).
- **Admin "Rotate now"**: button in the admin UI's JWKS panel
  calls `Service.RotateNow`. Same code path; same overlap.

The retired key is hard-removed by the rotation goroutine's sweep
once `RetiresAt < now`. Other nodes pick up the new kid on the
next JWKS background refresh (every 5 min by default), or on
demand when they encounter a token signed under an unknown kid.

### Recovery

If `MEMQL_IDENTITY_KEY_ENCRYPTION_KEY` is rotated incorrectly (the new
secret can't decrypt the old envelope), the binary fails to load
the key files at startup with a clear AES-GCM error. Restore the
original secret from your secret store, redeploy, then perform a
proper rotation: stand up the new secret, call "Rotate now" so a
fresh key is minted under it, and only then retire the old
secret.

### Staging DB reset stays auth-coherent (#1522)

The staging DB reset (`staging-db-reset.sh`, owned by the product pack's
deploy estate since the deploy/release tooling moved there) wipes the
staging database back to an empty schema. A DB wipe used to leave auth
HALF-BROKEN: every `v1:identity:authSession` row and every mesh
node-token grant lives in the DB, so the wipe invalidated them all, and
the cluster only recovered after a manual reseal + mesh roll. The reset
is now auth-coherent by construction -- it leans on two facts and you do
NOT have to do anything by hand afterward:

- **The signing key is NOT in the DB.** `MEMQL_IDENTITY_SIGNING_KEY_B64` rides
  the sealed genesis envelope (`MEMQL_GENESIS_B64` in the `memql-secrets`
  Secret); the wipe never touches the Secret, so every replica derives
  the same key + `kid` + JWKS after the reset -- as long as the seed is
  present. The reset **pre-flights** this BEFORE wiping: it refuses if
  the Secret carries no genesis envelope / direct seed, or if identity is
  running in the divergent per-pod ephemeral-key mode
  (`MEMQL_IDENTITY_ALLOW_EPHEMERAL_KEY=true` at `replicas >= 2`). A wipe in
  either state would leave auth unrecoverable, so it is blocked.
- **Mesh nodes re-mint their token on auth failure (#1521).** Once
  identity is back with the stable key, every leaf reconnects on its own.

The reset brings **identity up FIRST** (waited Available) so the JWKS
issuer is serving before any mesh node bootstraps, then restores the rest
of the workloads. Finally it **verifies** identity is Available and
probes the in-cluster JWKS document. Confirm end-to-end with the
pack's staging smoke target (checks JWKS + the login surface over the
public front door). No manual reseal or mesh roll is required.

## Dev quick-start

The local cluster runs via `make up` -- see
[docs/public/overview/quickstart.md](../../overview/quickstart.md) for the
port-forward reference. The local overlay wires identity at
`http://identity:8081` (cluster DNS) with every other node verifying
against that issuer; reach it from the host via
`kubectl port-forward -n memql svc/identity 8085:8085`.

For running the binaries standalone (no cluster), set the same URL:

```bash
# Identity binary
MEMQL_IDENTITY_ENABLED=true \
MEMQL_IDENTITY_BASE_URL=https://identity.local.znas.io \
MEMQL_IDENTITY_REGISTRATION_MODE=open \
make identity-assets identity
./bin/memql-identity

# bff binary, points at the identity binary above
MEMQL_IDENTITY_VERIFIER_BASE_URL=http://identity:8081 \
MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER=https://identity.local.znas.io \
make bff
./bin/memql-bff
```

Without `MEMQL_IDENTITY_VERIFIER_BASE_URL` the bff boots into no-auth
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
  controlled by `MEMQL_IDENTITY_AUDIT_LOG_RETENTION_DAYS` (default 365).

## Related

- [sign-in-paths.md](sign-in-paths.md) -- magic link, passkey, enrolment
  link, device code and PAT: what each is for, what recovery looks like
  with no mail, and why a passkey does not follow you between clusters.
- [access-model.md](access-model.md) -- enforcement layers, role
  spectrum, the wire-side lifecycle.
- [user-provisioning.md](user-provisioning.md) -- registration
  modes, magic-link flow, invitations.
- `component/identity/CLAUDE.md` -- per-package developer guide.
