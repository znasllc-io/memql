---
title: MemQL OS -- operator guide
audience: public
status: stable
area: operate
sinceVersion: 0.20.0
owner: platform
---

# MemQL OS -- operator guide

MemQL OS is the browser workspace for one MemQL cluster. Open its apps to
manage execution resources, inspect data, work with artifacts, and follow agent
work. The engine performs the operations and enforces authorization; the OS
presents them through a shared desktop, session, and connection.

For the default local cluster, open `https://os.memql.localhost/`. Sign in with
that cluster's identity service. If setup appears, follow
[first-run setup](first-run.md). App availability follows your account's grants
and the cluster's configuration; a hidden app is not proof its data is absent.

Start with [the app map](#the-apps). The rest of this page retains the operator
and authentication reference.

The approved design direction is
[Supervised Visual Composition](supervised-visual-composition.md): direct visual
composition and supervised proposals in a shared workspace. Fleet provides
visual composition, persistent routing-policy editing, and typed Ask proposal
review. Proposal generation requires configured compatible inference. The
approved Deployables redesign is implemented and verified on the local deployment;
proposed Settings/Logs layouts still await approval.

Related: [identity](auth/identity-service.md), [access](auth/access-model.md),
[environment parity](environment-parity.md), [front door](front-door.md).
The former portal's migration history remains in
[the retirement record](../../superpowers/specs/2026-09-06-portal-removal-design.md).

---

## Which cluster it manages

**The one it was served by. There is no cluster registry, and that is
deliberate.**

The VS Code panel reads `~/.memql/clusters.yaml` and authenticates with an
identity-issued JWT access token. Refresh credentials use editor SecretStorage.
A PAT does not authenticate this engine connection. A browser can do neither: it has no
filesystem, and a long-lived PAT where page JavaScript can read it would be
strictly worse than the OAuth flow the identity service already runs.

Derive-from-origin costs nothing -- no registry, no schema, no CRUD surface, no
sync problem -- and matches how an operator thinks about a web console: the
console at `os.prod.example.com` *is* the production cluster's console. It also
makes one class of mistake impossible: because the page and the stream share an
origin, the bundle cannot read cluster A while carrying a token minted for
cluster B.

**The cost, stated plainly: no multi-cluster switching.** An operator with
three clusters opens three tabs. That is an accepted trade, not an oversight.

Browser-local storage was rejected for the same job because it is per-browser
and per-profile: invisible to every other operator, invisible to the same
operator on another machine, and silently divergent from the `clusters.yaml`
the Cockpit and the VS Code panel share. A registry only one person can see is
worse than none, because it looks like one.

---

## How it authenticates

OAuth 2.1 authorization code + PKCE against the identity service, exactly like
any other browser client. **No PAT is ever involved, and no credential ever
appears in a URL.**

```
  browser                     edge (os.<domain>)          identity
     |                              |                         |
     |-- GET / --------------------->|                        |
     |-- GET /runtime-config.json -->|  (*)                   |
     |<-- {identityUrl, oauthClientId, authEnabled, domain} --|
     |                                                        |
     |== top-level navigation ================================>|
     |   GET /authorize?response_type=code&client_id=os        |
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

(*) `GET /runtime-config.json` is answered by `component/edge/runtimeconfig.go`
for EVERY hosted site alike, never a branch for this one
(`TestPortalHasNoSpecialCaseInTheServingPath` -- named for the site that first
proved the rule). The cluster-wide fields (`identityUrl`, `identityApiBaseUrl`,
`authEnabled`) come from the domain-derived env
`component/envregistry/domain.go` sets at boot, and the one per-site field
(`oauthClientId`) is looked up by matching the requesting site's own hostname
against `MEMQL_IDENTITY_REGISTERED_CLIENTS` -- an unregistered site still gets
a 200, just with an empty client id.

**The OAuth client id is `os`.** It was `portal`, carrying both hostnames'
redirect URIs, until memql#4984; renaming it was safe precisely because no
bundle hardcodes it -- each reads its own out of the document above.

`domain` is the fifth field and the one exception to "derived": it is
`MEMQL_DOMAIN` itself, the value every other derivation starts from, published
so that a client which has to **name this cluster** does not have to
reverse-engineer it out of `identityUrl`.

### Token storage, and the threat model

| Credential | Lifetime | Where it lives |
|---|---|---|
| Access token | ~15 min | A JavaScript closure variable. Nowhere else. |
| Refresh token | ~30 days | The HttpOnly `memql_refresh` cookie. The shell never reads it. |

Identity returns the refresh token in the `/oauth/token` and `/auth/refresh`
JSON bodies *as well as* in the cookie. The shell **deliberately takes only
`access_token`** (`clients/os/src/auth/identityClient.ts`); taking the other
would hand a 30-day credential to page JavaScript, which is the exact thing the
HttpOnly cookie exists to prevent.

**The trade in three sentences.** An XSS on this origin can read the in-memory
access token, so the split does not make XSS harmless -- it caps the damage at
one short-lived token for one live page instead of a 30-day refresh token an
attacker could exfiltrate and reuse from anywhere, which is the difference
between an incident and a persistent backdoor. The CSRF exposure accepted in
return is that the refresh cookie rides automatically on requests the browser
makes, and identity applies **no CSRF token** to `/auth/refresh` (the JSON API
in `component/identity/http` is mounted without the web package's CSRF
middleware) -- the defences are `SameSite=Lax` on the cookie plus identity's
exact-match CORS allowlist, which together mean a forged cross-site POST either
does not carry the cookie or cannot read the response. That is the right way
round: a token an attacker cannot *read* is worth more than one they cannot
cause to be *sent*, because the sent-but-unreadable case yields nothing.

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
  `auth_code_replay`.

### Token rotation

The SDK owns rotation. `sdk/ts` `Connection` decodes the bearer's `exp`, fires
at 70% of the remaining TTL, calls the shell's `onTokenExpired` hook, and
installs the new token on the **live stream** via `rotateAuth`. A console left
open on a desk is never torn down and redialled just because a fifteen-minute
token aged out -- subscriptions survive.

---

## Required configuration

Nothing, on a cluster brought up by `make up` or from the k8s base: the site
row is seeded, the front-door rule and certificate SAN are generated, and the
OAuth client and CORS origin are derived from `MEMQL_DOMAIN`. What follows is
what those derivations produce, so an operator can check them.

### On the edge

The bundle is baked into the edge image at `/app/os` and the seeded site row's
`bundleRef` names it (`file:///app/os`). Only the edge builds it: the
Dockerfile's `spa-build` stage is selected by `SPA_DIST_STAGE`, which
`scripts/lib/engine_build_args.sh` sets for the local edge build and the
release matrix sets for the `edge` entry.

### On the identity service

Three values, all derived from `MEMQL_DOMAIN` by
`component/envregistry/domain.go` and set-if-absent at boot:

| Env | What it must contain |
|---|---|
| `MEMQL_IDENTITY_REGISTERED_CLIENTS` | a client `os` whose redirect URI is `https://os.<domain>/auth/callback` |
| `MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS` | `https://os.<domain>` |
| `MEMQL_IDENTITY_BASE_URL` | `https://identity.<domain>` |

**A missing CORS origin is a silent sign-in death** (memql#3315): the browser
completes `/authorize`, then the `POST /oauth/token` fails at the preflight and
the page reports no token with nothing in identity's logs to explain it.

### Cross-origin XHR, only

`identityApiBaseUrl` is published EMPTY, which means same-origin: the browser
resolves `/oauth/token`, `/auth/refresh`, `/auth/logout` and
`/.well-known/jwks.json` against the site that served the page, and
`component/edge`'s `serveIdentityXHR` forwards exactly those four paths to the
identity binary. Sibling hosts that share a wildcard certificate and an IP
otherwise HTTP/2-coalesce, and a Mac browser's `POST /oauth/token` lands on
this site's SPA fallback -- 200 HTML, no access token (memql#4154).

### Clusters with authentication disabled

`MEMQL_IDENTITY_ENABLED=false` makes every node admit its stream as a synthetic
`local-dev` cluster owner. The shell reads `authEnabled` from the runtime
config and skips the sign-in flow. **Never set it false in a cloud cluster.**

---

## Authorization is server-side

Every role gate in the shell is PRESENTATION. `system/roles.ts` decides which
apps and sections to OFFER, from the `clusterRole` the cluster reported; the
engine decides what any of them can actually read or write, per row, on every
call. Hiding a control an operator cannot use teaches them who can; it is not
what stops them.

Two consequences worth knowing:

- **A refusal is not a zero.** Where a section's own floor is looser than the
  floor on one read inside it, the section renders and that read comes back
  refused -- in the engine's own words, in surface, never as an empty list.
  Settings -> Cluster does this deliberately: it admits admin, and its
  infrastructure and provider reads are owner-only.
- **A role SET is not a floor.** Settings -> Integrations is
  owner-or-developer and explicitly not admin, which a ladder minimum cannot
  express (developer outranks admin). Settings -> Permissions lists what a
  session cannot see and names the requirement as it was written.

---

## The apps

| App | Use it for |
|---|---|
| **Fleet** | Machines, models and app execution resources, routing, and workbenches |
| **Files** | Library folders, artifact content, provenance, and versions |
| **Deployables** | Sources, builds, domains, serving state, traffic, and deployment history |
| **Nexus** | Goals, runs, steps, automations, and approvals |
| **Concepts** | Declared concepts, field definitions, and rows visible to your account |
| **Cluster** | Readiness, modules, data origins, agents, and the audit trail |
| **Training** | Training from Library files and reviewing knowledge |
| **Materializer** | Composing data into a file |
| **Campaigns** | Mail audiences, templates, senders, rules, and sending |
| **Stores** | Shopify store configuration, health, subscriptions, and mirror state |
| **Users** | People, roles, invitations, enrolment, and sessions |
| **Accounts** | Accounts the cluster works for and their configuration |
| **Logs** | Cluster log search and following events |
| **Bin** | Archived items and supported restoration |
| **Settings** | Appearance, access, AI configuration, identity policy, and diagnostics |

**Ask** is a shared interaction surface and desk widget. Inference and voice
availability depend on configuration. It does not imply generalized autonomous
control of every app.

Access is granted by app and section through the engine's access model, not by a
single universal role floor. The current roster is declared in
`clients/os/src/apps/registry.tsx`. An app's presence does not grant permission
to its operations. See [app access](auth/access-model.md).

### Settings

Settings groups general preferences (About, Appearance, Ask, Apps), Access,
Cluster identity policy, Diagnostics, Benchmarks, Integrations, AI configuration,
Tokens, Keys, and this app's Logs. Which sections you see follows your grants.

The AI sections are **Doors**, **Levels**, **Rules**, and **Decisions**: where
inference can run, what a call needs, how it routes, and what actually happened.
Follow [AI settings](ai-settings.md) for the current controls and
[AI routing](ai-routing.md) for engine behavior. Older instructions to paste
OpenAI or Anthropic API keys into an OS "AI providers" page are superseded by
these source-specific setup flows.

Tokens distinguish personal and node credentials. Keys shows published identity
keysets and whether replicas agree. Cluster policy configures registration and
identity behavior; changes to token lifetimes apply when new credentials are
issued. Existing credentials do not silently acquire a new lifetime.

### What is deliberately NOT here

- **Deploy control.** Cutting a release and rolling a deployment stay with the
  [Cockpit](release-cutting.md) and `DeployControlService`.
- **Your own account.** Passkeys, sessions, personal access tokens, data export
  and the sign-in-policy switch are identity's own pages, at
  `identity.<domain>/me/{settings,devices,tokens,export}`. They are where the
  ceremony that registers a passkey has to run.
- **The server-rendered `/admin/*` console**, which answers `410 Gone`.

---

## Troubleshooting

| Symptom | Look at |
|---|---|
| Sign-in loops back to the sign-in page | `MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS` must name `https://os.<domain>` exactly |
| `/authorize` answers 400 | the registered client's redirect URI must be the origin the bundle is served from, matched as an exact string |
| The browser reports a name mismatch at `os.<domain>` | the front-door certificate names exact hosts; check `os.<domain>` is a SAN and that the exact Ingress rule exists (`make frontdoor-hosts-check`) |
| Every asset 404s | the edge image was built without `SPA_DIST_STAGE=spa-build`, so `/app/os` is empty |
| A section is missing | it is role-gated; Settings -> Diagnostics lists what this session cannot see and why |
| A list is correct on load and never moves | the concept's `graph.node.*` events have no cross-node routing rule (`component/node/routing.go`) |
| Sign-in works, then fails, then works | the identity replicas disagree on their keyset -- Settings -> Keys |
