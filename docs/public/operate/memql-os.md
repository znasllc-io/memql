---
title: MemQL OS -- operator guide
audience: public
status: stable
area: operate
sinceVersion: 0.20.0
owner: platform
---

# MemQL OS -- operator guide

MemQL OS is the platform's own graphical operations console: a static SPA
served by the edge as a `v1:platform:site` row (memql#4705) -- the same site
resolution, bundle opener and headers as any customer site, at its own
hostname -- dialing the same `/memql/ws` gRPC bridge every other client uses.
This page covers what an operator has to configure to make it usable, and
records the design decisions that shape that configuration.

It replaces `portal.md`. The MemQL Portal held this role until epic
[memql#4984](https://github.com/znasllc-io/memql/issues/4984) retired it; the
inventory of what moved, what was retired and what was deferred is
[the retirement record](../../superpowers/specs/2026-09-06-portal-removal-design.md).

Related: [identity-service.md](auth/identity-service.md),
[access-model.md](auth/access-model.md),
[environment-parity.md](environment-parity.md),
[front-door.md](front-door.md).

---

## Which cluster it manages

**The one it was served by. There is no cluster registry, and that is
deliberate.**

The VS Code panel and the Cockpit read `~/.memql/clusters.yaml` and
authenticate with a PAT from that file. A browser can do neither: it has no
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

| App | What it is for | Floor |
|---|---|---|
| Files | The Library as a live folder tree, with each file's provenance, versions and backups | none |
| Deployables | What serves where: the map, each deployable's source, build, domains, traffic and history | none |
| Fleet | Machines, routing policy and call history, workbenches | none |
| Users | People, roles, sessions, invitations and enrolment links | admin |
| Accounts | The client registry -- who this instance does work for | admin |
| Campaigns | Mail: audiences, templates, senders, rules and send control | none |
| Training | Teach MemQL from Library files; the review queue and knowledge domains | writer |
| Nexus | Goals, runs and the step spine; automations and the approvals queue | none |
| Materializer | Compose data from the memory graph into a file | none |
| Logs | The cluster log store, following and windowed search | admin |
| Bin | The archive destination; restore from here | none |
| Settings | Everything below | mixed |

### Settings

| Section | What it is for | Floor |
|---|---|---|
| About | Who you are signed in as, and what this shell is | none |
| Appearance | Light / dark / system, and the theme pack | none |
| Ask | How the Ask surface behaves | none |
| Apps | The installed roster, and what each one's sections are | none |
| Cluster | Cluster and identity facts, versions, mail sender -- and the editable **Policy** form | admin |
| AI providers | Anthropic federation and API keys, OpenAI keys, the per-provider registry, Verify and Apply | owner |
| Tokens | Every personal access token and node credential, with revoke | admin |
| Keys | The published JWKS keyset, whether the replicas agree on it, and the rotation history | admin |
| Integrations | What this cluster can talk to and what each one needs | owner or developer |
| Diagnostics | Connection and permission facts, and a report to paste into a thread | none |
| Logs | This app's own lines | admin |

Four of those arrived when the portal was retired, and each carries a rule
worth stating:

- **AI providers.** A secret is write-only in both directions: the field posts
  a value and nothing renders one back -- what returns is a fingerprint. A
  cluster with no provider configured is the NORMAL first state, not a fault.
  Federation leads the Anthropic block because the two paths are not equivalent
  options: federation leaves no credential at rest anywhere. **A save is not an
  Apply** -- saving writes the row, and Apply is what makes every node
  re-resolve its registry.
- **Tokens.** Personal access tokens and node credentials are kept apart
  because they are different kinds of thing revoked through different ops. A
  revoked row shows no Revoke button rather than a disabled one. The personal
  half is a bounded fan-out over the people list (there is no cluster-wide PAT
  read) and it **says how far it reached**, so a token you cannot find is never
  mistaken for a token that does not exist.
- **Keys.** It leads with whether the identity replicas AGREE on their keyset,
  not with a key table: divergent keysets fail roughly half of all auth
  (memql#3400) while every manifest looks correct. Four independent reads;
  disagreement is proof, agreement is evidence, and the sentence says which.
  There is no rotate control, because where the key arrives sealed in the
  environment envelope rotating it is a re-seal and a roll.
- **Cluster -> Policy.** Registration mode, the role an internal person gets on
  first sign-in, the four lifetimes, the cookie policy and the identity pages'
  brand. Lifetimes are asked in minutes and days and stored in seconds; blank
  means the cluster's own default. A change applies to the next token minted or
  link issued -- it does not shorten a session somebody already has.

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
