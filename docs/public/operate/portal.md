---
title: MemQL Portal -- operator guide
audience: public
status: stable
area: operate
sinceVersion: 0.16.0
owner: platform
---

# MemQL Portal -- operator guide

The MemQL Portal is the platform's own graphical operations console: a static
SPA served by the edge as site #1 (memql#3711) -- the same `v1:platform:site`
resolution, bundle opener and headers as any customer site, at its own
hostname -- dialing the same `/memql/ws` gRPC bridge every other client uses.
This page covers what an operator has to configure to make it usable, and
records the two design decisions that shape that configuration.

Related: [identity-service.md](auth/identity-service.md),
[access-model.md](auth/access-model.md),
[environment-parity.md](environment-parity.md).

---

## Which cluster the portal manages

**The one it was served by. There is no cluster registry, and that is
deliberate.**

The VS Code panel and the Cockpit read `~/.memql/clusters.yaml` and
authenticate with a PAT from that file. A browser can do neither: it has no
filesystem, and a long-lived PAT where page JavaScript can read it would be
strictly worse than the OAuth flow the identity service already runs.

Three options were considered:

| Option | Verdict |
|---|---|
| **Derive from the origin** -- the cluster is whatever origin served the page | **CHOSEN** |
| **Server-side registry** -- `v1:cluster:*` rows every operator sees | Deferred; the seam is left open |
| **Browser-local storage** -- a list in `localStorage` | Rejected |

Derive-from-origin costs nothing (no registry, no schema, no CRUD surface, no
sync problem) and matches how an operator thinks about a web console: the
console at `api.prod.example.com` *is* the production cluster's console.
It also makes one class of mistake impossible -- because the page and the
stream share an origin, the bundle cannot read cluster A while carrying a token
minted for cluster B.

**The cost, stated plainly: no multi-cluster switching.** An operator with
three clusters opens three tabs. That is an accepted trade, not an oversight.

Browser-local storage was rejected because it is per-browser and per-profile:
invisible to every other operator, invisible to the same operator on another
machine, and silently divergent from the `clusters.yaml` the Cockpit and the
VS Code panel share. A registry only one person can see is worse than none,
because it looks like one.

**If multi-cluster is revisited**, the seam is
`clients/portal/src/cluster/endpoint.ts` -- the single place the portal decides
what to dial. A server-side registry would surface as a `clusters: [...]` field
on the runtime-config document below, and that module would gain a resolver.
Nothing else in the portal would move.

---

## How the portal authenticates

OAuth 2.1 authorization code + PKCE against the identity service, exactly like
any other browser client. **No PAT is ever involved, and no credential ever
appears in a URL.**

```
  browser                     edge (portal.<domain>)      identity
     |                              |                         |
     |-- GET / --------------------->|                        |
     |-- GET /runtime-config.json -->|  (*)                   |
     |<-- {identityUrl, oauthClientId, authEnabled, domain} --|
     |                                                        |
     |== top-level navigation ================================>|
     |   GET /authorize?response_type=code&client_id=portal    |
     |       &redirect_uri=...&state=...&code_challenge=...    |
     |       &code_challenge_method=S256                       |
     |                                        magic-link email |
     |<== 302 /auth/callback?code=...&state=... ===============|
     |                                                        |
     |-- POST /oauth/token {code, code_verifier} ------------->|
     |<-- {access_token} + Set-Cookie: memql_refresh (HttpOnly)|
     |                                                        |
     |== WebSocket /_memql/memql/ws, subprotocols ["bearer", <jwt>] ==>| (edge proxies to the bff, memql#3712)
     |                                                        |
     |   ...~70% of the token's TTL later...                   |
     |-- POST /auth/refresh (cookie) ------------------------->|
     |<-- {access_token}                                       |
     |-- rotateAuth on the LIVE stream -----------------------> (bff, via the edge proxy)
```

(*) The portal is site #1 (memql#3711), served by the generic `component/edge`
-- which must not carry portal-specific config-serving logic
(`TestPortalHasNoSpecialCaseInTheServingPath`). `GET /runtime-config.json` is
answered by `component/edge/runtimeconfig.go` for EVERY hosted site alike,
not a portal branch: the cluster-wide fields (`identityUrl`,
`identityApiBaseUrl`, `authEnabled`) come from the same domain-derived env
`component/envregistry/domain.go` sets at boot, and the one per-site field
(`oauthClientId`) is looked up by matching the requesting site's own hostname
against `MEMQL_IDENTITY_REGISTERED_CLIENTS` -- an unregistered site still
gets a 200, just with an empty client id.

`domain` (memql#4249) is the fifth field and the one exception to "derived":
it is `MEMQL_DOMAIN` itself, the value every other derivation starts from,
published so that a client which has to **name this cluster** does not have to
reverse-engineer it back out of `identityUrl`. The portal's *Open definition in
VS Code* link is the reason it exists: the domain is the one value the VS Code
extension records for a cluster -- endpoint and issuer compose from it -- so it
is what a link naming a cluster has to carry. Additive, like every field here:
an older node omits it and the portal falls back to the `identity.` label of
`identityUrl`, which is exact rather than a guess because every role host is a
single label under the domain (memql#3767).

### Token storage, and the threat model

| Credential | Lifetime | Where it lives |
|---|---|---|
| Access token | ~15 min | A JavaScript closure variable. Nowhere else. |
| Refresh token | ~30 days | The HttpOnly `memql_refresh` cookie. The portal never reads it. |

Identity returns the refresh token in the `/oauth/token` and `/auth/refresh`
JSON bodies *as well as* in the cookie. The portal **deliberately discards that
field** (`clients/portal/src/auth/identityClient.ts`); taking it would hand a
30-day credential to page JavaScript, which is the exact thing the HttpOnly
cookie exists to prevent.

**The trade in three sentences.** An XSS on the portal's origin can read the
in-memory access token, so this split does not make XSS harmless -- it caps the
damage at one short-lived token for one live page instead of a 30-day refresh
token an attacker could exfiltrate and reuse from anywhere, which is the
difference between an incident and a persistent backdoor. The CSRF exposure
accepted in return is that the refresh cookie rides automatically on requests
the browser makes, and identity applies **no CSRF token** to `/auth/refresh`
(the JSON API in `component/identity/http` is mounted without the web package's
CSRF middleware) -- the defences are `SameSite=Lax` on the cookie plus
identity's exact-match CORS allowlist, which together mean a forged cross-site
POST either does not carry the cookie or cannot read the response. That is the
right way round: a token an attacker cannot *read* is worth more than one they
cannot cause to be *sent*, because the sent-but-unreadable case yields nothing.

`localStorage` was rejected for the access token because it **outlives the
page**: readable by any script that ever runs on the origin, at any later time,
including after the operator closed the tab -- and it syncs with the browser
profile. In-memory is strictly less exposure for no functional loss, since a
cold load rebuilds the session from the cookie in one request.

### Credentials are never in a URL

- The bearer rides the **WebSocket subprotocol** channel
  (`Sec-WebSocket-Protocol: bearer, <jwt>`, memql#2511), not the deprecated
  `?bearer_token=` query parameter, which leaks into every ingress access log.
- Access and refresh tokens travel in POST bodies and an HttpOnly cookie.
- The **authorization code** does appear on the callback URL -- that is
  inherent to the code flow, and it is precisely the design that keeps *tokens*
  out of URLs. The code is single-use, minutes-lived, and PKCE-bound to a
  verifier that never left the browser; identity audits a replay as
  `auth_code_replay`. The portal scrubs it from the address bar with
  `history.replaceState` the instant the callback renders.

Both properties are covered by tests that inspect the constructed URLs
(`clients/portal/test/identityClient.test.ts`,
`clients/portal/test/authRotation.test.ts`).

### Token rotation

The SDK owns rotation. `sdk/ts` `Connection` decodes the bearer's `exp`,
fires at 70% of the remaining TTL, calls the portal's `onTokenExpired` hook,
and installs the new token on the **live stream** via `rotateAuth`. A portal
left open on a dashboard is never torn down and redialled just because a
fifteen-minute token aged out -- subscriptions survive. Verified end to end in
`clients/portal/test/authRotation.test.ts`, which asserts that exactly one
socket is ever constructed across an expiry.

---

## Required configuration

The portal reads its configuration at runtime from the node that served it, at
`GET /runtime-config.json`. Nothing is baked into the bundle -- the
engine image is product- and environment-agnostic, so the same bytes run
against local, staging and a customer install.

```json
{
  "identityUrl": "https://identity.example.com",
  "identityApiBaseUrl": "",
  "oauthClientId": "portal",
  "authEnabled": true,
  "domain": "example.com"
}
```

Every field is public: an OIDC issuer URL, an XHR base that is **empty** so the
four identity JSON calls stay same-origin (the edge proxies them; see
[identity-service.md](auth/identity-service.md#browser-side-routing-of-identity-xhr)),
a public OAuth client id (RFC 6749 public clients have no secret -- that is why
PKCE is mandatory), whether this cluster enforces auth, and the cluster's own
domain. The document is served unauthenticated, on the same public path as the
bundle.

`domain` is the one field that is not about talking to this cluster but about
**naming it somewhere else** -- see [above](#how-the-portal-authenticates).
Nothing has to be set for it: it is `MEMQL_DOMAIN`, which the cluster already
has.

### On the node serving the portal (the edge)

The portal is site #1 (memql#3711): its bundle location is the seeded site
row's `bundleRef` (`dsl/platform/seeds.memql`), not an env var --
`MEMQL_PORTAL_DIST` and its three `MEMQL_PORTAL_IDENTITY_*` /
`MEMQL_PORTAL_OAUTH_CLIENT_ID` siblings are retired along with
`component/portal`, which used to read them. The identity-facing values
those four variables used to configure are no longer configuration at all:
they are DERIVED, per request, by `component/edge/runtimeconfig.go` from the
same domain-wide env `component/envregistry/domain.go` already sets
(`identityUrl` / `identityApiBaseUrl` / `authEnabled`), plus a lookup of the
requesting site's own hostname against `MEMQL_IDENTITY_REGISTERED_CLIENTS`
(`oauthClientId`) and a read of `MEMQL_DOMAIN` itself (`domain`). There is
nothing left for an operator to set on the edge node for this.

### On the identity service

Two entries an operator **must** add, or sign-in fails with a 400 at
`/authorize` ("Unknown client" / "Invalid redirect URI"):

1. **Register the portal's OAuth client.** Identity matches `redirect_uri` by
   **exact string** -- a trailing slash or a missing port is a rejection.

   ```
   MEMQL_IDENTITY_REGISTERED_CLIENTS=[
     {"clientId":"portal",
      "redirectURIs":["https://portal.example.com/auth/callback"]},
     ...
   ]
   ```

   The callback path is the portal's own origin's `/auth/callback` -- the
   portal is site #1, served at its own hostname rather than a `/portal/`
   sub-path of another node's origin, so the redirect URI carries no mount
   prefix. `component/envregistry/domain.go` registers this automatically from
   `MEMQL_DOMAIN`; a hand-rolled `MEMQL_IDENTITY_REGISTERED_CLIENTS` (a
   non-standard domain, a bespoke install) must match it byte for byte.

2. **Allow the portal's origin for CORS**, unless you take the proxy option
   below.

   ```
   MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS=https://portal.example.com,...
   ```

   `*` is **refused** here (memql#3716). Identity's CORS middleware emits
   `Access-Control-Allow-Credentials: true` on every match, so a wildcard would
   grant credentialed cross-origin read access to every origin on the internet;
   browsers reject the pair anyway, but identity no longer relies on that. List
   the origins.

   This env var is the **bootstrap** half of the allowlist. An origin that
   belongs to a customer's own website is granted per registered OAuth client
   instead, and takes effect with no identity restart -- see
   [identity-service.md](auth/identity-service.md#cross-origin-access-cors).

### Cross-origin XHR, only

The portal calls `/oauth/token`, `/auth/refresh` and `/auth/logout`
**cross-origin** on the identity host -- the CORS entry above is required for
it, and it works: `component/edge/csp.go`'s per-site CSP names the cluster's
identity origin in `connect-src` for every hosted site
(`identityOriginFromEnv`, reading the same domain-derived env
`runtimeconfig.go`'s `identityUrl` field does, memql#3711 fix round 2). The
original per-site policy accounted for a site's own data through `/_memql/*`
and forgot that a site with signed-in users also has to reach identity
directly -- that omission is what the fix round closed, not a design that
was ever intended to block this.

[identity-service.md](auth/identity-service.md) still names a reason to
prefer a same-origin path where one exists: Safari has an HTTP/2
connection-coalescing bug that intermittently fails cross-origin credentialed
XHR to a sibling host sharing a certificate and IP, surfacing as
`TypeError: Load failed` with no server-side trace at all. **For the portal,
that alternative does not exist.** The old `MEMQL_PORTAL_IDENTITY_API_BASE_URL=self`
toggle is retired along with `component/portal`, and nothing replaces it:
reaching identity same-origin would mean forwarding `/oauth/token` /
`/auth/refresh` / `/auth/logout` through the site's own `/_memql/*` proxy,
which today targets only the bff (which does not itself serve those paths)
-- a second proxy target and a second declared prefix, recorded as a
rejected-for-now design decision
(`component/edge/runtimeconfig.go`'s `IdentityAPIBaseURL` doc comment) rather
than left unmentioned. Cross-origin is not standing in for that; it is the
ordinary shape of an OAuth 2.1 + PKCE public client, and every deployment
gets it with no front-door rules at all. Only the top-level `/authorize`
navigation is unaffected either way -- it is an HTML page and has no
same-origin variant, so it always goes to the identity host directly.

### Clusters with authentication disabled

Where `MEMQL_IDENTITY_ENABLED=false` (troubleshooting only -- **never** in
staging or production), `authEnabled` is `false`, the portal shows no sign-in,
dials with no credential, and the header displays a persistent
**"Authentication disabled"** warning. Every stream in that mode is admitted as
the synthetic `local-dev` cluster owner, and an operator must not be able to
mistake that for having authenticated.

---

## What the UI tells you

The header answers the two questions an operations console must answer without
a click:

- **Which cluster** -- the origin host that served the page (the cluster's name
  under the derive-from-origin decision), plus the node id and version of the
  *replica* serving this stream, taken from the `ServerHello`. In a two-replica
  mesh that is what tells you which pod you are looking at.
- **Who you are** -- the email and cluster role, read from the cluster over
  `MyAccessMsg`, not decoded from the token the browser holds. If the two ever
  disagree -- a rotated token, a session revoked elsewhere, a role changed
  since the token was minted -- the header shows what the cluster is actually
  acting on.

---

## Authorization is server-side

The portal hides what a caller cannot do, but **nothing in the browser
enforces anything**. Route protection
(`clients/portal/src/app/RequireAuth.tsx`) decides what to *render*; the
`authEnabled` flag decides whether to *offer* a sign-in. Every read and write
is gated server-side, per stream by the identity verifier interceptors in
`component/grpc`, and per row by the DSL's owned/granted/admin/public
classification. An operator who deletes the route guard in a debugger gets an
empty console wired to a connection the server refuses.

See [per-row-authz-audit.md](auth/per-row-authz-audit.md).

---

## Administration

`/admin` is the operator console: the cluster's own state, rather than
the data it holds. Every surface is owner-and-admin. (Root-relative since
memql#3711 -- the portal is site #1, served at its own hostname rather than
a `/portal/` sub-path of another node's origin, so its own client-side
routes carry no mount prefix.)

| Surface | Address | What it answers |
|---|---|---|
| Sessions and tokens | `/admin/tokens` | Every personal access token issued against the cluster and who holds it, plus every node credential; revoke either. |
| Signing keys | `/admin/keys` | The Ed25519 keys the cluster publishes, which one is signing, whether an overlap window is open, and when it last rotated. |
| Cluster settings | `/admin/settings` | The runtime-editable settings in force -- registration policy, token lifetimes, branding -- and the form that changes them. |

Two surfaces that used to be here retired in memql#4264, and both redirect
rather than 404 -- an operator with either bookmarked did nothing wrong:

- **`/admin` (Overview)** answered the same question as the console at `/`,
  with the same tiles and the same "By role" band. It now sends you there. The
  readings it carried that the console does not -- which key is signing, when
  it last rotated, the registration policy -- are on Signing keys and Cluster
  settings, where they were already rendered.
- **`/admin/people`** was the CHANGE surface for one person, beside a People
  *view* listing the same population. The list was always the view's job, so
  the four things an owner or admin does to a person -- profile, cluster role,
  suspension, enrolment link -- moved onto that view's row detail, and
  `/admin/people` sends you to `/views/people`.

Putting those verbs on the view does not break the contract that makes the
predefined views work for a concept nobody designed for: the view BODY still
composes only view-kit elements, and the actions render in the row dialog from
`src/people/PersonActions.tsx`, outside `src/views/`. The guard that forbids
row markup and iteration inside a view is untouched.

The audit trail and deployments are **not** here either: they are populations,
and they live in the predefined views at `/views/audit` and
`/views/deployments`.

### Deployments, and the operations on them

One surface (memql#4264). There used to be two — this view's Ship band and a
separate Cluster ops page — and they did not merely duplicate each other, they
**disagreed about how dangerous the actions are**: the view fired deploy and
roll back on a single click, while the page confirmed every one and made repair
type-to-confirm. An operator's protection depended on which door they had
walked through, and the rail offered both.

The careful set won. Every verb states what will happen before it happens, and
the progress afterwards is the deployment RECORD's status — the History band is
graph state re-read live, not a client-side guess. `/cluster-ops` redirects here.

A **repair record is never a rollback target**: a repair pins no version (it
re-converges the cluster onto what is already committed), so "roll back to this
repair" names nothing to roll to. Nor is a record that did not succeed, nor the
version already running.

### Inviting somebody

An owner or admin invites a person from the **People view's** header, not from a
separate screen (memql#4272). The dialog states what an invitation MEANS on this
cluster, because that depends on the registration mode and an operator who does
not know which case they are in will misread the result:

| Mode | What the dialog says, and what the server does |
|---|---|
| `invite_only` | The link is the only way in. |
| `domain_restricted` | Names the allowed domains; the server **refuses** an address outside them, because a link the recipient cannot redeem is worse than a refusal — they find out after clicking. |
| `waitlist` | An invitation admits somebody directly, without waiting. |
| `open` | Anyone can already register, so the link is a convenience rather than a gate. |

The mode is read to word the dialog and is **never the check**: the gate in
`component/identity/adminops` applies the policy and refuses independently, and
a client deciding its own authorization is not a check.

The link is shown **once** — only its SHA-256 hash reaches the cluster, so it
cannot be shown a second time. An "Invited" band under the population lists who
is still outstanding, live, with a revoke for a link sent to the wrong address.
Revoking is a soft cancel: the record stays, because revoking does not make
whoever holds the link forget it.

### The gate is the cluster's, on both halves

**Reads.** Each admin read names a query carrying `requiresOwnerOrAdmin` in its
own filter (`searchUsers`, `patIdentitiesForUser`, `recentAuditEvents`,
`nodeTokenIdentitiesAdmin`), so a caller below the floor gets an empty result
from the engine rather than a page this console decided to hide. The signing
keys come from the public `/.well-known/jwks.json` feed -- the same document
every verifier node reads, which is what makes the page useful when a JWKS has
gone incoherent across replicas.

**Writes.** Every write goes through one bridged envelope,
`IdentityAdminMsg` / `IdentityAdminResult` on `MemqlService.Stream`, onto
`component/identity/adminops` (memql#3324). The console never calls the
underlying mutation directly, and that is the whole design: a MemQL mutation
**cannot carry a role predicate** -- `filter` is a read construct -- so
`updateUser` is server-origin-only with no client-reachable seam, and
`revokePATIdentity` / `updateClusterSettings` name an arbitrary target under a
coarse write check that admits every role from `writer` up. Calling them from a
browser would have handed a `writer` what the retired server-rendered console
reserved for an admin.

So the gate stayed in Go, where it already was, and moved from an HTTP
middleware to one package with one implementation of the rule and one audit
write. A refusal is `PERMISSION_DENIED` plus the same `admin_auth_forbidden`
event, with the same `role_not_admin` reason, that the retired route wrote --
so an audit trail an operator greps is unbroken across the move.

**Every write reports its audit event id**, refusals included, next to a link
into `/views/audit`. Quote it in an incident thread; it is the durable
artefact of the action, and a status line saying only "Saved." would have
thrown it away.

### What is deliberately NOT in the portal

| Capability | Where it is | Why |
|---|---|---|
| Force a signing-key rotation | Nowhere, by design | The key manager runs in-process on the identity node, and in every deployed environment the key arrives sealed in the env envelope (`MEMQL_IDENTITY_SIGNING_KEY_B64`) so each replica derives the same one. `KeyManager.RotationSupported()` is false in that mode, so the retired console's "Rotate now" button returned an error in staging and production alike. Rotation is a **re-seal and a rolling restart**. The scheduled rotation (`MEMQL_IDENTITY_KEY_ROTATION_DAYS`) applies only to the on-disk key directory a single-node dev cluster uses. |

### The server-rendered `/admin/*` console is otherwise retired

It served seven screens. Six of them -- the dashboard, users, tokens, audit,
JWKS and settings -- are gone, along with their routes, their handlers and
their templates, deleted in the same commits that landed their replacements.
The seventh, Deployments, followed in memql#3380: `DeployControlService` runs
shell scripts against an on-disk overlay checkout and therefore exists only on
the identity node, so the portal's deploy calls used to reach a bff with no
such service and come back `UNIMPLEMENTED`. A `NodeService` forward now carries
them to the identity node -- with the caller's authority attached, so the
owner-only rollback gate still runs against the human who pressed the button --
and `/views/deployments` both reads live state and acts.

What remains on the identity service is `/admin/login` (which establishes the
session); `/admin/` answers `410 Gone` and points here.

### The `/me/*` pages stay where they are

The identity service also serves `/me/dashboard`, `/me/devices`, `/me/export`,
`/me/settings` and `/me/tokens`. Those are **not** moving into the portal, and
the reason is who they are for rather than what they do.

The portal is an operator's console: it is reached with an operator's
credential, it assumes you are looking after a cluster, and every surface under
`/admin` shows nothing at all unless you are owner or admin. `/me/*` is
the opposite -- it is where an ordinary person reads their own sessions,
revokes their own device, exports their own data and closes their own account,
and every one of those flows is self-scoped by construction. Moving them behind
an operations console would put a reader's account settings inside a product
they have no other reason to open, and would make "manage my account" depend on
the portal bundle being deployed at all.

They also already sit where a person is *sent*: the magic-link flow, the
sign-in page and the account emails all land on the identity origin, so
`/me/*` is one navigation from where the person already is and zero from where
the portal is not.

So the split is by audience, and it is settled: **operator surfaces move,
self-service surfaces do not.** Nothing about `/me/*` is pending, and it is not
waiting on a seam.

---

## Troubleshooting

| Symptom | Cause |
|---|---|
| The portal's hostname 404s asset-by-asset, or serves an empty bundle | No bundle at the site row's `bundleRef` on the edge pod's disk (`file:///app/portal` by default). `make portal-build`, then `updateSiteBundle` to point at the built directory, or deploy an image whose portal stage built. |
| Sign-in page says "not configured for portal sign-in" | The node published no `identityUrl` / `oauthClientId`. Check `MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER` (or the `MEMQL_IDENTITY_BASE_URL` fallback) on the edge node -- `component/edge/runtimeconfig.go` reads it from there now, not the bff. |
| "Unknown client" on the identity page | Either `MEMQL_IDENTITY_REGISTERED_CLIENTS` has no entry at all for this domain, or its `redirectURIs` do not carry an exact `https://<this site's hostname>/auth/callback` -- `clientIDForHostname` (`component/edge/runtimeconfig.go`) matches by exact string, so a trailing slash or the wrong hostname resolves to an empty client id, not a fuzzy match. |
| "Invalid redirect URI" | The registered URI is not byte-identical to the portal's own origin's `/auth/callback` (no mount prefix -- the portal is site #1, not a `/portal/` sub-path of another node). |
| Sign-in button does nothing; console shows a CSP `connect-src` violation | Should not happen for the identity exchange itself -- `component/edge/csp.go` names the cluster's identity origin on every site's policy (memql#3711 fix round 2). If it does, check that `MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER` / `MEMQL_IDENTITY_BASE_URL` on the edge node resolves to the SAME origin the browser is actually being sent to -- a mismatch between what the edge derives and what identity is really reachable at is the remaining way this fires. |
| `TypeError: Load failed` on `/auth/refresh`, intermittently, Safari only | The known HTTP/2 coalescing bug. Take the same-origin proxy option above. |
| Signed in, then signed out again ~15 minutes later | `/auth/refresh` is failing. Check the CORS allowlist and that the `memql_refresh` cookie's `SameSite` suits your topology (`MEMQL_IDENTITY_REFRESH_COOKIE_SAMESITE=none` when the portal and identity are on different registrable domains). |
| Sign-in loops back to the sign-in page | Session storage is blocked for the origin. The portal reports this explicitly rather than redirecting. |
