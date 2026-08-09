# memQL Portal -- operator guide

The memQL Portal is the platform's own graphical operations console: a static
SPA served by the bff at `/portal/`, dialing the same `/memql/ws` gRPC bridge
every other client uses. This page covers what an operator has to configure to
make it usable, and records the two design decisions that shape that
configuration.

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
console at `cockpit.prod.example.com` *is* the production cluster's console.
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
  browser                     bff (/portal/)              identity
     |                              |                         |
     |-- GET /portal/ -------------->|                        |
     |-- GET /portal/runtime-config.json ->|                  |
     |<-- {identityUrl, oauthClientId, authEnabled} ----------|
     |                                                        |
     |== top-level navigation ================================>|
     |   GET /authorize?response_type=code&client_id=portal    |
     |       &redirect_uri=...&state=...&code_challenge=...    |
     |       &code_challenge_method=S256                       |
     |                                        magic-link email |
     |<== 302 /portal/auth/callback?code=...&state=... ========|
     |                                                        |
     |-- POST /oauth/token {code, code_verifier} ------------->|
     |<-- {access_token} + Set-Cookie: memql_refresh (HttpOnly)|
     |                                                        |
     |== WebSocket /memql/ws, subprotocols ["bearer", <jwt>] ==>| (bff)
     |                                                        |
     |   ...~70% of the token's TTL later...                   |
     |-- POST /auth/refresh (cookie) ------------------------->|
     |<-- {access_token}                                       |
     |-- rotateAuth on the LIVE stream -----------------------> (bff)
```

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
`GET /portal/runtime-config.json`. Nothing is baked into the bundle -- the
engine image is product- and environment-agnostic, so the same bytes run
against local, staging and a customer install.

```json
{
  "identityUrl": "https://identity.example.com",
  "identityApiBaseUrl": "https://identity.example.com",
  "oauthClientId": "portal",
  "authEnabled": true
}
```

Every field is public: an OIDC issuer URL, a public OAuth client id (RFC 6749
public clients have no secret -- that is why PKCE is mandatory), and whether
this cluster enforces auth. The document is served unauthenticated, on the same
public path as the bundle.

### On the node serving the portal (the bff)

| Variable | Default | Notes |
|---|---|---|
| `MEMQL_PORTAL_DIST` | `/app/portal` | Where the built bundle lives. |
| `MEMQL_PORTAL_OAUTH_CLIENT_ID` | `portal` | Must match a registered client on identity. |
| `MEMQL_PORTAL_IDENTITY_URL` | derived | Override only if the issuer is not the browser-facing URL. |
| `MEMQL_PORTAL_IDENTITY_API_BASE_URL` | derived | Set to `self` when the front door proxies identity's JSON endpoints. |

`identityUrl` is **derived** and usually needs no configuration: it comes from
`MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER`, which every engine node already
carries and which must equal identity's own `MEMQL_IDENTITY_BASE_URL`. An
issuer is by construction the public origin operators reach identity at.

> **Not `MEMQL_IDENTITY_VERIFIER_BASE_URL`.** That is the in-cluster address
> (`https://identity:8085` in every `deploy/k8s` manifest). Handing it to a
> browser produces a DNS failure on a name that resolves only inside the pod
> network -- it would work on a laptop running everything on localhost and fail
> on every real deployment.

### On the identity service

Two entries an operator **must** add, or sign-in fails with a 400 at
`/authorize` ("Unknown client" / "Invalid redirect URI"):

1. **Register the portal's OAuth client.** Identity matches `redirect_uri` by
   **exact string** -- a trailing slash or a missing port is a rejection.

   ```
   MEMQL_IDENTITY_REGISTERED_CLIENTS=[
     {"clientId":"portal",
      "redirectURIs":["https://cockpit.example.com/portal/auth/callback"]},
     ...
   ]
   ```

   The callback path is `<portal mount>/auth/callback`, i.e. `/portal/auth/callback`
   in the normal case (prefixed by `SERVER_PUBLIC_PATH` where one is set).

2. **Allow the portal's origin for CORS**, unless you take the proxy option
   below.

   ```
   MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS=https://cockpit.example.com,...
   ```

   Do **not** use `*` here: identity's CORS middleware emits
   `Access-Control-Allow-Credentials: true` alongside it, a combination
   browsers reject outright for credentialed requests.

### Cross-origin XHR, or a same-origin proxy

By default the portal calls `/oauth/token`, `/auth/refresh` and `/auth/logout`
**cross-origin** on the identity host, which is why the CORS entry above is
needed. This works, and it is the zero-infrastructure path.

[identity-service.md](auth/identity-service.md) prescribes the alternative and
explains why: browsers should reach those endpoints **same-origin** through the
front door, which proxies them to identity, because Safari has an HTTP/2
connection-coalescing bug that intermittently fails cross-origin credentialed
XHR to a sibling host sharing a wildcard certificate and IP. It surfaces as
`TypeError: Load failed` with no server-side trace at all.

To take that path: add `location =` rules on the portal's front door for
`/oauth/token`, `/auth/refresh`, `/auth/logout` and
`/.well-known/jwks.json`, then set `MEMQL_PORTAL_IDENTITY_API_BASE_URL=self`.
The refresh cookie is then set on the portal's own origin and no CORS entry is
required. Only the top-level `/authorize` navigation still goes to the identity
host directly -- it is an HTML page and has no same-origin variant.

The portal's CSP is generated to match whichever choice is in effect
(`component/portal/csp.go` puts the XHR origin in `connect-src`), so the two
cannot drift apart.

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

`/portal/admin` is the operator console: the cluster's own state, rather than
the data it holds. Every surface is owner-and-admin.

| Surface | Address | What it answers |
|---|---|---|
| Overview | `/portal/admin` | How many people can sign in, how they divide by role, how a new person gets an account, which key is signing, and what has happened recently. |
| People | `/portal/admin/people` | Who can sign in, and the changes an owner or admin may make to one of them: profile, cluster role, suspension. |
| Sessions and tokens | `/portal/admin/tokens` | Every personal access token issued against the cluster and who holds it, plus every node credential; revoke either. |
| Signing keys | `/portal/admin/keys` | The Ed25519 keys the cluster publishes, which one is signing, whether an overlap window is open, and when it last rotated. |
| Cluster settings | `/portal/admin/settings` | The runtime-editable settings in force -- registration policy, token lifetimes, branding -- and the form that changes them. |

The audit trail and deployments are **not** here: they are populations, and
they live in the predefined views at `/portal/views/audit` and
`/portal/views/deployments`. Neither is the People *population*
(`/portal/views/people`) -- that view answers "who is in this organisation and
who is signed in", carries no controls, and composes only view-kit elements.
`/portal/admin/people` is the CHANGE surface, one person at a time. Putting an
owner-only write inside a predefined view would break the contract that makes
those views work for a concept nobody has designed for.

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
underlying mutation directly, and that is the whole design: a memQL mutation
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
into `/portal/views/audit`. Quote it in an incident thread; it is the durable
artefact of the action, and a status line saying only "Saved." would have
thrown it away.

### What is deliberately NOT in the portal

| Capability | Where it is | Why |
|---|---|---|
| Force a signing-key rotation | Nowhere, by design | The key manager runs in-process on the identity node, and in every deployed environment the key arrives sealed in the env envelope (`MEMQL_IDENTITY_SIGNING_KEY_B64`) so each replica derives the same one. `KeyManager.RotationSupported()` is false in that mode, so the retired console's "Rotate now" button returned an error in staging and production alike. Rotation is a **re-seal and a rolling restart**. The scheduled rotation (`MEMQL_IDENTITY_KEY_ROTATION_DAYS`) applies only to the on-disk key directory a single-node dev cluster uses. |
| Deployments -- the live gate, image digests and actions | `https://identity.<host>/admin/deployments`, the Cockpit, and `/portal/views/deployments` for the history | `DeployControlService` runs shell scripts against an on-disk overlay checkout, so it exists only on the **identity** node. The portal is served by the bff and dials the origin that served it, so its deploy actions reach a node with no deploy-control service and come back `UNIMPLEMENTED`. Until that RPC can cross the mesh, the identity console keeps the surface. See [deployment-console.md](deployment-console.md). |

### The server-rendered `/admin/*` console is otherwise retired

It served seven screens. Six of them -- the dashboard, users, tokens, audit,
JWKS and settings -- are gone, along with their routes, their handlers and
their templates, deleted in the same commits that landed their replacements.
What remains on the identity service is `/admin/login` (which establishes the
session) and `/admin/deployments`; `/admin/` redirects to the latter.

### The `/me/*` pages stay where they are

The identity service also serves `/me/dashboard`, `/me/devices`, `/me/export`,
`/me/settings` and `/me/tokens`. Those are **not** moving into the portal, and
the reason is who they are for rather than what they do.

The portal is an operator's console: it is reached with an operator's
credential, it assumes you are looking after a cluster, and every surface under
`/portal/admin` shows nothing at all unless you are owner or admin. `/me/*` is
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
| `/portal/` 404s with a message naming `MEMQL_PORTAL_DIST` | No bundle on the node. `make portal-build` and point the variable at `clients/portal/dist`, or deploy an image whose portal stage built. |
| Sign-in page says "not configured for portal sign-in" | The node published no `identityUrl` / `oauthClientId`. Check `MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER` on the bff. |
| "Unknown client" on the identity page | `MEMQL_PORTAL_OAUTH_CLIENT_ID` is not in `MEMQL_IDENTITY_REGISTERED_CLIENTS`. |
| "Invalid redirect URI" | The registered URI is not byte-identical to `<origin><mount>/auth/callback`. |
| Sign-in button does nothing; console shows a CSP `connect-src` violation | The portal's origin is reaching an identity origin the node did not publish. Check `MEMQL_PORTAL_IDENTITY_API_BASE_URL`. |
| `TypeError: Load failed` on `/auth/refresh`, intermittently, Safari only | The known HTTP/2 coalescing bug. Take the same-origin proxy option above. |
| Signed in, then signed out again ~15 minutes later | `/auth/refresh` is failing. Check the CORS allowlist and that the `memql_refresh` cookie's `SameSite` suits your topology (`MEMQL_IDENTITY_REFRESH_COOKIE_SAMESITE=none` when the portal and identity are on different registrable domains). |
| Sign-in loops back to the sign-in page | Session storage is blocked for the origin. The portal reports this explicitly rather than redirecting. |
