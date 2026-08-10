# Signing In Without Email — VS Code Handoff, Passkeys, and the "+" Entry Point

**Date:** 2026-08-09
**Status:** Design approved; ready for implementation planning
**Surfaces:** identity service (`component/identity`), VS Code extension (`editors/vscode`), memQL Portal (`clients/portal`)
**Related:** [Local Cluster Install & Uninstall Wizard](2026-08-08-local-cluster-install-wizard-design.md)

---

## 1. Problem

Magic-link is the only way to authenticate a human to a memQL cluster, and the
VS Code extension cannot perform even that. Three failures compound:

1. **The extension has no login at all.** `editors/vscode/src/clusters/model.ts:68`
   defines `isOidcOnly()` for the sole purpose of telling the user
   *"Configured for OIDC. Authenticate in the memQL Cockpit, or add a PAT."*
   The only credential the extension can use is a Personal Access Token pasted
   by hand into `~/.memql/clusters.yaml`.

2. **Email is a single point of failure.** Every human login path terminates in
   a mailbox. Where mail is not configured the path simply ends.

3. **On a local cluster, mail is never configured.** The install-wizard design
   records this at §2.5: `integrations/email` falls back to `LogSender` when
   neither Graph nor SMTP credentials are present, so "check your email" is a
   dead end on exactly the install being built. Its D7 accepts this and
   specifies scraping the magic link out of the identity pod's logs.

The user-visible consequence: the fastest way to open a new local cluster in
VS Code is to install it, read a pod log, click a link scraped from that log,
mint a PAT, and paste the PAT into a YAML file.

A fourth, smaller gap sits next to these: clicking **+** in the extension's
Clusters view always assumes you want to register a remote cluster. It never
offers to install one, and it cannot tell whether one already exists.

---

## 2. Constraints discovered during design

Findings from the current tree, with evidence. Each closed off a design
direction.

### 2.1 Identity is already a complete OAuth 2.1 authorization server

`GET /authorize` (`component/identity/web/authorize.go`) validates client and
redirect URI against the registered-client table, **requires PKCE S256**, and
carries the OAuth context into login. `POST /oauth/token`
(`component/identity/http/token.go:50`) implements the `authorization_code` and
`refresh_token` grants. RFC 7591 dynamic client registration exists at
`POST /register`. JWKS, refresh and logout are all wired.

**Consequence:** the "open a browser, come back with a token" flow needs no new
authorization server. It needs a client.

### 2.2 Loopback-any-port redirect matching already exists, and was built for this

`component/identity/config.go:846` implements `matchesLoopbackAnyPort` — RFC
8252 §7.3. Its own comment at line 812 says it is "what lets a native client
(the cockpit)" listen on an ephemeral loopback port. A registered redirect URI
of `http://127.0.0.1/callback` with **no port** matches an incoming callback on
any port.

**Consequence:** the extension's loopback flow requires zero server-side change.

### 2.3 `/authorize` is factor-agnostic; magic-link is merely what is plugged into it

`handleAuthorize` validates the OAuth request and funnels the user into login.
`IssueMagicLink` stamps `clientId`, `redirectURI`, `state`, `codeChallenge` and
`codeChallengeMethod` onto the magic-link row's `oauthCtx`
(`component/identity/magiclink/issuer.go:282`); `handleComplete`
(`component/identity/http/complete.go`) decodes them and redirects to the
client's callback with `code` + `state`.

**Consequence:** a second authentication factor does not modify `/authorize`,
`/oauth/token`, or any client. It has to reach the same terminal state —
an auth code bound to the same OAuth context.

### 2.4 Auth-code minting is already a standalone primitive

`Store.CreateAuthCode` (`component/identity/store.go:219`) mints a one-time
OAuth code independently of magic links. `LookupAuthCodeByCodeHash` and
`ConsumeAuthCode` complete the lifecycle.

**Consequence:** a new factor can mint a code directly. No refactor of the
magic-link verifier is required, and none should be attempted opportunistically.

### 2.5 There is no device authorization grant and no WebAuthn anywhere

A tree-wide search returns zero hits for `device_code`,
`urn:ietf:params:oauth:grant-type:device`, `webauthn` and `passkey` across Go,
TypeScript, Markdown and templ sources.

**Consequence:** both are genuinely new server surface. Neither is a
modification of something existing.

### 2.6 A pairing-code primitive already exists, in the wrong direction

`POST /pair/codes` + `POST /pair/redeem` (`component/identity/http/pair.go`)
mint and redeem TTL'd pairing codes with `Authorization: Pair <code>`,
HTTPS-required, hashed at rest, rate-limited and audited. But it is initiated
by an **already-authenticated** operator and issues **worker** tokens.

**Consequence:** the right shape to copy, not the right mechanism to reuse. The
same is true of `POST /auth/badge/grant`
(`component/identity/http/badge_grant.go`), which is the closest existing
precedent for a non-magic-link factor producing a credential.

### 2.7 `v1:identity:identity` is a discriminated union built for new credential kinds

`identityType` is an enum with a `@variant(discriminator="identityType")` block
carrying nine credential shapes (`oauth`, `api_key`, `service_account`,
`magic_link`, `worker_token`, `node_token`, `voice_agent_token`, `badge`,
`account_token`). The `badge` variant demonstrates a physical-world credential
with a hash-at-rest lookup key and a grant exchange.

**Consequence:** a passkey is a tenth variant, not a new concept.

### 2.8 `/setup` cannot host passkey enrolment on the install-wizard path

`handleSetupGet` (`component/identity/web/handlers.go:394`) 404s once any user
exists. The install wizard's D7 uses the unattended bootstrap path
(`MEMQL_IDENTITY_BOOTSTRAP_*`), which completes setup from env **before**
`/setup` would ever render.

**Consequence:** on the install path there is no moment where a human is present
at `/setup`. First-credential enrolment needs its own entry point.

### 2.9 The extension writes credentials to a file it shares with the Cockpit

`editors/vscode/src/clusters/file.ts:55` maps `pat` as a plain string field in
`~/.memql/clusters.yaml`. `docs/public/operate/portal.md:20` confirms the
Cockpit reads the same file. The extension uses VS Code's SecretStorage nowhere.

**Consequence:** OAuth refresh tokens must not follow the PAT into that file.

### 2.10 The install substrate the "+" card needs is already landing

`editors/vscode/src/install/` carries `graph.ts`, `executor.ts`, `runner.ts`,
`receipt.ts` and `cli.ts`, with `defaultReceiptPath()` already in use
(`cli.ts:106`). `ClusterConfig.local` already exists in the registry model.

**Consequence:** presence detection is composition of existing parts.

---

## 3. Decisions

### D1 — Two axes, not two alternatives

Browser handoff and passkeys solve different problems and neither substitutes
for the other.

- **Transport** — how a token reaches VS Code. Answered by loopback + PKCE, with
  a device-code fallback.
- **Factor** — what proves identity once a browser is open. Answered by
  passkeys.

Transport alone still dead-ends at the mail problem on a local cluster: the
browser opens and asks for an email that will never arrive. Factor alone leaves
the extension unable to sign itself in. Both are required for the end-to-end
story; each is independently useful and independently shippable.

### D2 — Loopback + PKCE is primary; device code is a fallback

The extension binds an ephemeral `127.0.0.1` listener, wraps the URL in
`vscode.env.asExternalUri()` (which tunnels correctly under Remote-SSH and
Codespaces), and completes the standard authorization-code flow. Per §2.2 this
requires no server change.

A device-code flow (RFC 8628) is added as the fallback for environments where
the listener cannot be bound or reached — a genuinely headless box, a hardened
network. It is not the primary path: it costs a round trip through a second
device and a code the user must transcribe.

**Rejected:** a `vscode://` deep link as the redirect target. It requires
registering a custom scheme and behaves inconsistently across VS Code, Cursor,
VSCodium and the web build. Loopback avoids scheme registration entirely.

### D3 — The extension self-registers via DCR; operators configure nothing

On first sign-in to a cluster with no `clientId`, the extension calls
`POST /register` (RFC 7591) as a public client with redirect URI
`http://127.0.0.1/callback` — **portless**, so §2.2's matcher accepts any
ephemeral port. The returned `client_id` is stored in the existing
`ClusterConfig.clientId` field.

The alternative — publishing a well-known static client id that every operator
adds to `MEMQL_IDENTITY_REGISTERED_CLIENTS` — makes every cluster a
configuration step and every new cluster a support question.

### D4 — Passkeys are a tenth identity variant that converge on the existing auth code

A `passkey` variant on `v1:identity:identity` (§2.7) stores credential id, COSE
public key, sign count, AAGUID, transports, backup eligibility/state, label and
`lastUsedAt`.

Login is **usernameless**: discoverable credentials, empty `allowCredentials`,
required user verification. No email is typed at any point.

`login/begin` stores the challenge server-side with the OAuth context stamped
on it — the same five fields `IssueMagicLink` stamps (§2.3). `login/finish`
verifies the assertion and calls `Store.CreateAuthCode` (§2.4), producing a
redirect to `redirect_uri?code=&state=` that is byte-identical to the
magic-link outcome.

This convergence is the load-bearing property: `/authorize`, `/oauth/token`,
the Cockpit, the portal, the SDK and the extension all keep working with no
knowledge of which factor ran.

### D5 — The first credential comes from a one-time enrolment link

A single-use `mql_enr_<43>` token, SHA-256 at rest, short TTL, redeemed at
`GET /enroll?code=...`, authorizes exactly one action: register a passkey as a
named user.

Two issuers:

- **The install wizard**, for the cluster owner. It just built the cluster, so
  it holds authority by construction. It opens the URL itself — the user copies
  nothing.
- **An owner or admin**, from the portal's People surface, for everyone else.

This replaces the wizard's D7 pod-log magic-link scrape as the intended first
sign-in. D7 remains valid as a fallback while this ships.

**Rejected:** enrolling only from an already-authenticated session. It cannot
produce a *first* credential, which is the case that matters.

**Rejected:** enrolment as a `/setup` step. Per §2.8 `/setup` does not render on
the install-wizard path, and it says nothing about the second user.

### D6 — Magic-link stays as the universal recovery route

Passkey is additive. Magic-link is untouched and is the documented recovery
path wherever mail works. On a local cluster with no mail, recovery is
"the wizard re-mints an enrolment link" — justified because filesystem access
to the machine already implies total authority over that cluster.

**Rejected:** one-time recovery codes. A whole credential type to mint, hash,
display once, store and revoke, for a case two existing mechanisms already
cover.

**Rejected:** mandating a second passkey at enrolment. It puts friction in the
exact flow this design exists to smooth.

### D7 — Passkeys are per cluster domain, and this is documented, not fixed

A passkey is bound to its RP ID. A credential for `staging.example.com` does
not work on `memql.localhost`, and reinstalling a local cluster wipes the
database and orphans its passkeys.

This is acceptable because the wizard mints a fresh enrolment link on every
install, and per-cluster credentials are arguably the correct security posture.
It is a documentation obligation, not a defect to engineer around.

### D8 — "+" branches on evidence, and never offers an install over an existing one

`clusterPresence` returns one of three verdicts from cheap signals — the
receipt file, a `local: true` registry entry, and a short-deadline dial:

| Verdict | Evidence | "+" offers |
|---|---|---|
| `absent` | no receipt, no `local: true` entry | **Install a local cluster…** (recommended) / **Connect to an existing cluster…** |
| `installed-healthy` | evidence present, endpoint answers | **Connect to an existing cluster…**, no picker |
| `installed-unreachable` | evidence present, dial fails | **Connect to an existing cluster…** / **Repair local cluster…** |

Two signals rather than one: the receipt is precise about what the wizard did,
and the registry entry catches a cluster built by hand with `make up`, which
the receipt alone would miss and then offer to overwrite.

The probe is a gRPC dial, **not** a Docker or k3d query. Rendering a menu must
never require Docker to be running. The verdict is memoized ~30s and
invalidated when install or uninstall completes.

### D9 — New HTTP endpoints, inside the existing auth exception

CLAUDE.md requires explicit approval for new HTTP endpoints. WebAuthn is a
browser API and RFC 8628 is defined over HTTP; neither has a gRPC form, and
both are identity-service auth surface. They sit in the documented
"Auth (identity service)" exception alongside `/auth/login`, `/oauth/token` and
`/.well-known/jwks.json`. Approved explicitly during design review.

---

## 4. Units

Five units with independently testable boundaries.

**Unit A — Identity: passkeys as a credential type.** The `passkey` variant,
`POST /auth/webauthn/register/begin|finish`, `POST /auth/webauthn/login/begin|finish`,
a *Sign in with a passkey* control on `/login`, and passkey management on the
existing `/me/devices` page. RP ID derives from `MEMQL_IDENTITY_BASE_URL`.

**Unit B — Identity: one-time enrolment links.** Token mint and redeem,
`GET /enroll?code=...`, wizard issuance, and admin issuance from the portal's
People surface.

**Unit C — Extension: OAuth client.** An `auth/` module: DCR registration,
PKCE + loopback via `asExternalUri`, token exchange, silent refresh, and
SecretStorage persistence keyed by cluster name. Deletes `isOidcOnly()` and its
message. PAT support is unchanged and keeps precedence.

**Unit D — Device-code fallback.** `POST /device/code`, `GET /device`
verification page, the `device_code` grant on `/oauth/token`, and the
extension-side polling client.

**Unit E — Extension: the "+" entry point.** `clusterPresence` detection and
the branching menu of D8.

**Sequencing.** C ships first and alone — no server work, and it removes
PAT-pasting immediately. A and B ship as a pair; a passkey that cannot be
enrolled is useless. D and E are independent tails.

---

## 5. Flows

### 5.1 VS Code sign-in (loopback + PKCE)

1. No `clientId` for this cluster → `POST /register` as a public client with
   portless loopback redirect; store the returned id in `clusters.yaml`.
2. Generate `code_verifier` / `code_challenge` (S256) and `state`; bind
   `127.0.0.1:0`; wrap in `vscode.env.asExternalUri()`.
3. Open `/authorize`. The user authenticates with whatever factor the cluster
   offers. The extension has no opinion.
4. Callback arrives on the loopback listener; `state` is validated; a
   "you can close this tab" page is served; the listener closes. One request,
   one path, 120-second deadline.
5. `POST /oauth/token` with `grant_type=authorization_code` + verifier.
6. Access and refresh tokens are written to **SecretStorage**, keyed by cluster
   name. Silent refresh at ~80% of access-token lifetime; on 401, refresh once,
   then prompt.

### 5.2 Device code (fallback)

`POST /device/code` returns `{device_code, user_code, verification_uri,
verification_uri_complete, expires_in, interval}`. VS Code shows the user code
with a copy button. The user opens the URL on any device, and an approval page
naming the client, source IP and user agent completes it. The extension polls
`/oauth/token` with the `device_code` grant, honouring `authorization_pending`
and `slow_down`.

### 5.3 Passkey enrolment

The wizard mints an enrolment token and opens
`https://identity.<domain>/enroll?code=...` itself. The page runs
`navigator.credentials.create()` with a discoverable credential and required
user verification. Selecting "use a phone" in the browser's own picker shows a
QR code; the phone performs Face ID or fingerprint and returns the assertion
over the hybrid transport tunnel.

**The phone never contacts the cluster.** The desktop browser performs all
network I/O; the phone does only the cryptography. This is what makes phone
enrolment viable against a `*.memql.localhost` cluster the phone cannot
resolve — subject to the spike in §8.

`register/finish` writes the `passkey` identity row and starts a session.

### 5.4 Passkey login

`login/begin` issues a challenge carrying the OAuth context. The ceremony runs
with empty `allowCredentials` and required user verification. `login/finish`
verifies the assertion, rejects sign-count regression, stamps `lastUsedAt`,
emits an audit event, mints an auth code via `Store.CreateAuthCode`, and returns
the client callback target.

### 5.5 Composed

Click **Sign in** in VS Code → browser opens → "use a phone" → scan → Face ID →
browser redirects to `127.0.0.1` → the extension holds a token. No email, no
PAT, no pod logs, no Cockpit round trip.

---

## 6. Security posture

Every new surface follows the discipline established by `pair.go`, because the
threat shape is the same.

- **Public client, no secret.** PKCE S256 already mandatory; `state` validated
  on return. The listener binds `127.0.0.1` explicitly — never `0.0.0.0` —
  serves one request on one path, then closes.
- **Refresh tokens in the OS keychain** via SecretStorage; never in
  `clusters.yaml` (§2.9).
- **Enrolment tokens:** 32 bytes of randomness, `mql_enr_` prefix, SHA-256 at
  rest, single-use via `consumedAt`, 15-minute TTL, HTTPS-required, per-IP rate
  limited, audited on issue, redeem and failure.
- **WebAuthn:** user verification required; origin and RP ID verified in the
  ceremony; challenges single-use and TTL'd; sign-count regression rejected and
  audited as the cloned-authenticator signal; credential id uniquely indexed.
- **Device code:** ≥40 bits of entropy in the user code from an
  ambiguity-free alphabet (no `0`/`O`, `1`/`I`); 10-minute TTL; hashed at rest;
  per-IP limiter; approval page names client, source IP and user agent.
- **`POST /register`** must remain rate-limited through the existing `abuse`
  package. A public client id is not a secret, but unbounded registration is a
  junk-row vector.
- Every new path writes `v1:identity:auditEvent`.

---

## 7. Testing

**Go.** A software authenticator driving both ceremonies. Enrolment-token
lifecycle: issue → redeem → expire → reuse-rejected → revoked. The device-code
state machine including `slow_down` and `expired_token`. The decisive test: a
passkey login produces an auth code redeemable at `/oauth/token` with the PKCE
binding intact, mirroring `component/identity/http/token_pkce_test.go`.

**Extension.** The PKCE/loopback module imports no `vscode` symbols, so it runs
in the fast `make vscode-test` lane. Command wiring and the "+" menu go in
`make vscode-test-host`. Presence detection gets a table test across all three
verdicts and both evidence sources.

**Manual.** New rows in
`docs/public/language/vscode-runtime-panel-verification.md`.

---

## 8. Risks and open questions

**Blocking spike — `.localhost` RP ID over hybrid transport.** Whether platform
authenticators accept an RP ID under the `.localhost` TLD for the QR-plus-phone
flow is unverified. If they refuse, local installs are limited to platform
authenticators (Touch ID, Windows Hello) and phone passkeys require the install
wizard's BYO-domain path (its D5 "Advanced"). This gates the **wording** of the
enrolment UI, not the ceremony code, so it must resolve before Unit B's page
copy is written — but it does not block Units A, C, D or E.

**`go-webauthn` dependency.** New Go module (BSD-3). Needs a license and CI
vendoring check.

**Per-cluster passkeys.** D7. A documentation obligation.

**VS Code forks.** Cursor and VSCodium implement `asExternalUri` with varying
fidelity. Loopback is chosen partly because it degrades to a plain browser open
rather than failing outright; the device-code fallback covers the remainder.

---

## 9. Deferred

- **Passkeys in the Cockpit and portal.** Both reach `/authorize` already, so
  both inherit passkey login the day Unit A lands, with no work. Native
  enrolment UI in those surfaces is separate.
- **Passkeys as a second factor / step-up authentication** for privileged
  operations. This design treats passkeys as a first factor only.
- **Retiring magic-link.** Explicitly not proposed. D6 keeps it as recovery.
- **Attestation verification and authenticator allowlists.** Enterprise
  controls with no current demand.

---

## 10. Work breakdown

Twelve issues under one epic, `epic:signin-without-email` (a new label following
the existing `epic:<slug>` convention).

| # | Issue | Labels | Depends on |
|---|---|---|---|
| 1 | Extension: PKCE + loopback auth module, DCR self-registration | `task` `area/vscode` `auth` | — |
| 2 | Extension: wire sign-in into ConnectionManager; delete the `isOidcOnly` dead end | `task` `area/vscode` `auth` | 1 |
| 3 | Extension: token lifecycle — SecretStorage, silent refresh, sign out | `task` `area/vscode` `auth` | 1 |
| 4 | SPIKE: `.localhost` RP ID + hybrid transport viability | `spike` `auth` `needs-human` | — |
| 5 | Identity: `passkey` identity variant + register begin/finish | `task` `auth` `security` `storage` | — |
| 6 | Identity: passkey login + OAuth-context carry + auth-code mint | `task` `auth` `security` | 5 |
| 7 | Identity: enrolment tokens, `/enroll` page, wizard + portal issuers | `task` `auth` `security` | 5 (4 gates the page copy only, not the code) |
| 8 | Identity: passkey management on `/me/devices` | `task` `auth` | 5 |
| 9 | Identity: RFC 8628 device-code endpoints + verification page | `task` `auth` `security` | — |
| 10 | Extension: device-code fallback wiring | `task` `area/vscode` `auth` | 1, 9 |
| 11 | Extension: "+" presence detection and menu | `task` `area/vscode` `dx` | — |
| 12 | Docs: sign-in paths, recovery, per-cluster passkey semantics | `task` `documentation` | 6, 7 |

---

## 11. References

- `component/identity/web/authorize.go` — the OAuth 2.1 authorization endpoint
- `component/identity/http/token.go` — grant handling
- `component/identity/config.go:846` — RFC 8252 loopback-any-port matching
- `component/identity/magiclink/issuer.go:282` — OAuth-context stamping
- `component/identity/store.go:219` — `CreateAuthCode`
- `component/identity/http/pair.go` — the credential-handling discipline to copy
- `component/identity/http/badge_grant.go` — precedent for a non-magic-link factor
- `dsl/identity/concepts.memql` — the `identityType` variant block
- `editors/vscode/src/clusters/model.ts:68` — `isOidcOnly`, to be deleted
- `editors/vscode/src/install/` — the receipt and executor substrate
- [Local Cluster Install & Uninstall Wizard](2026-08-08-local-cluster-install-wizard-design.md) — §2.5 and D7
