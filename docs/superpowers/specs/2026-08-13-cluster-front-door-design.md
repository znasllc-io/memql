# The cluster front door: five static rules, and sites as data

**Issue:** none yet — this design is brainstorm-first; issues are filed from the
implementation plan.
**Status:** design approved, not implemented.
**Depends on nothing.** Supersedes nothing. Builds directly on
[`2026-08-12-custom-local-domain-design.md`](2026-08-12-custom-local-domain-design.md)
(memql#3593), which made the domain a parameter; this makes the *set of names
under it* open-ended without making the set of routing rules grow.

---

## 1. Problem

memQL is about to be released as a platform someone runs to host web
applications, websites and thick clients — several of them, for their own
customers. The front door was built for a fixed set of three names and cannot
express that. This design says what it becomes.

### 1.1 What is exposed today (local)

| Endpoint | Protocol | Route | Backend | Purpose |
|---|---|---|---|---|
| `https://cockpit.<domain>/` | gRPC (h2) | traefik :443 | `svc/bff:50051` (h2c) | The API edge — `MemqlService.Stream` |
| `https://cockpit.<domain>/memql/ws` | HTTP/1.1 + Upgrade | traefik :443 | `svc/bff-http:8085` | Browser bridge to the same stream |
| `https://cockpit.<domain>/portal/*` | HTTP/1.1 | traefik :443 | `svc/bff-http:8085` | Portal SPA bundle |
| `https://identity.<domain>/*` | HTTPS/1.1 | traefik :443 | `svc/identity:8085` | Auth: login, WebAuthn, OAuth, JWKS, `/me/*`, `/enroll`, `/device` |
| `http://*:80` | HTTP | traefik :80 | — | Redirects |
| `localhost:8085` | HTTP | k3d serviceLB, **bypasses traefik** | `svc/identity-external` | A second entrance to identity |
| `localhost:7880` | — | k3d serviceLB | nothing | Vestigial; the local overlay deletes livekit |
| `localhost:5432` | Postgres | k3d serviceLB | `svc/postgres` | Debug |

Cloud and downstream overlays add `app.<domain>` (a product SPA — not defined in
this repo, but `component/genesis/domain.go` already derives its CORS origin and
OAuth redirect URI), `livekit.<domain>`, and `mcp.<domain>` on :8090.

### 1.2 What actually multiplies

Not the doors — there is one, port 443, one L7 proxy (traefik locally, nginx in
the cloud). What multiplies is the number of **places a hostname is declared**.
Adding one today touches seven: an Ingress rule, a certificate SAN, the
`/etc/hosts` managed block, an ArgoCD `kustomize.patches` entry, the derivation
in `genesis/domain.go`, the CORS origin list, and the OAuth redirect URIs.

memql#3593 collapsed that to one `MEMQL_DOMAIN` for the **fixed** set of names.
It made the domain a parameter. It did not make the name set open-ended, and a
hosting platform needs exactly that.

### 1.3 Three defects the current shape already has

**(a) Path enumeration silently drops routes.** Backend protocol is a
per-Service setting, which is why `bff` and `bff-http` exist as two Services
over one Deployment. So every HTTP path on the bff needs its own Ingress rule.
`/inbound/{source}` and `GET+POST /unsubscribe` are documented public HTTP
exceptions that third parties dial — a Shopify webhook, a recipient's mail
client executing RFC 8058 one-click — and **no overlay in this repo routes
either one.** A missing rule does not 404: it hands an HTTP/1.1 request to an
h2c backend and fails with a protocol error naming nothing.

**(b) Second entrances.** `identity-external` exposes identity on host port 8085
outside the front door — a connection path that exists in no other environment,
which is precisely what `docs/public/operate/environment-parity.md` forbids.
Port 7880 maps to a Deployment the local overlay deletes.

**(c) The name describes a consumer, not a role.** `cockpit.<domain>` is dialed
by the Cockpit, the VS Code extension (`editors/vscode/src/extension.ts:1550`
composes `cockpit.<domain>:443`), `sdk/go`, `sdk/ts`, workers, the portal's
`/memql/ws` — and by Gmail and Outlook, because
`docs/public/operate/campaign-sending.md:219` prints
`List-Unsubscribe: <https://cockpit.example.com/unsubscribe?...>` into the
headers of outbound mail. An endpoint named after the first thing that connected
to it is now a string strangers read.

---

## 2. Decisions

### D1 — Isolation is the cluster. One memQL cluster per customer.

A customer gets their own cluster, one domain, and as many sites, apps and
products as they want inside it.

*Rejected: shared multi-tenant cluster with account-scoped data.* Not on cost or
taste — on capability. `docs/internal/design/account-isolation-model.md` (status
ACCEPTED, memql#3321) measures that `actor.*` is a closed set with **no tenancy
dimension** (§5.2), so every account-scoped filter can only be
`accountId == args.accountId` — "authorization by honour system: the caller
names the tenant whose rows they would like." Its §6(b), a resolved account set
on `AccessContext`, is named the load-bearing item and is not built. Its own
summary rule: *"An `account` row is safe. Data hung off an `accountId` is not."*

And the decisive argument is not any of that. **You can hand over a cluster; you
cannot hand over a tenant.** "We build it, then give it to them if they want to
run it themselves" is a product promise no shared-tenancy design can keep, and
no amount of §6(b) work changes that.

*Consequence, deliberately accepted:* a per-customer floor of roughly
1.6 vCPU / 2 GiB in requests for the full mesh at one replica each — eight nodes
declare `200m/256Mi` in `deploy/k8s/base` (identity, cognition, agent, planner,
workbench, voice, voice-agent, mcp) and the bff declares none, so the real floor
is somewhat higher — plus the media plane and a database. Mitigated by
cluster profiles — node types are already separate binaries behind build tags,
and the local overlay already scales voice and voice-agent to 0 when LiveKit
credentials are absent (memql#2416). A website-hosting cluster runs identity +
bff + edge + Postgres, roughly 800m / 1 GiB.

*Consequence, named:* this trades tenancy-isolation complexity inside one
cluster for fleet-management complexity across many. That is a deliberate trade,
and a good one — a tenancy bug is a silent cross-customer leak, a fleet bug is a
visible outage — but it is a real cost, not a free win.

### D2 — One domain per cluster; wildcard plus apex certificate, issued at install

`*.<domain>` and `<domain>`, the SAN pair `scripts/lib/localtls.sh` already
issues locally (`MEMQL_LOCAL_TLS_HOSTNAMES="*.${DOMAIN},${DOMAIN}"`) and
cert-manager issues in the cloud. Domain setup is an **install-time step
performed by an operator**, not a runtime self-service feature.

*Rejected: per-site custom domains with runtime ACME.* Designed in full during
the brainstorm and shelved rather than discarded — HTTP-01 as the only viable
challenge type for domains whose DNS you do not control, a pending/verifying
state machine on the site row, and either cert-manager objects per domain or
TLS termination moved into the edge behind SNI passthrough. Not needed under D1,
because a customer's cluster is already on the customer's domain. If a cluster
ever needs a *second* domain that is a cluster config change; if that turns out
common, §11 records where the design went.

### D3 — Five static host rules, plus a media plane

Committed to `deploy/k8s`, never regenerated, identical in every environment.
Five **hosts** — the count that must not grow with customers, apps or sites:

| Host | Backend | Protocol |
|---|---|---|
| `api.<domain>` | `svc/bff:50051` **and** `svc/bff-http:8085` | h2c (gRPC) + http — see §5 |
| `identity.<domain>` | `svc/identity:8085` | https |
| `mcp.<domain>` | `svc/mcp:8090` | http |
| `*.<domain>` | `svc/edge:8085` | http |
| `<domain>` (apex) | `svc/edge:8085` | http |

`api.<domain>` is the one host carrying two backends, because backend protocol
is a per-Service setting (§1.3(a)). Its path split is **derived**, not authored —
§5 is the whole of that mechanism, and the reason it is the only generated rule
in the design.

The apex matters: for a customer cluster, `<domain>` **is** their main website,
so it is a site row like any other.

MCP gets its own rule rather than a path under `api.` for the reason in §1.3(a)
— backend protocol is per-Service — and rather than a proxy through the edge
because MCP clients configure a URL, are not browsers, and gain nothing from an
extra hop on a tool-calling path.

**Voice is not one of these and never will be.** WebRTC media is UDP and cannot
traverse an HTTP front door. Cloud is `livekit.<domain>` plus UDP ports; local
is LiveKit Cloud. The honest statement is *five HTTP rules plus a separate media
plane*, and the spec says so rather than letting someone discover it.

*Rejected: path-based consolidation onto one hostname.* The browser's security
boundary is the origin, not the path. Two sites sharing a hostname share
`localStorage`, `sessionStorage`, IndexedDB, service-worker scope and cookie
scope, and can script each other. Disqualifying even for one customer's own
`shop.` and `admin.`.

*Rejected: header-based routing at the proxy.* It works for all three protocols
— gRPC carries `:authority`, a WebSocket upgrade carries `Host`, SNI precedes
both — but Traefik expresses header matching through `IngressRoute` while
ingress-nginx needs a configuration snippet. That pushes an actual routing rule
into the one layer environment-parity currently confines to annotations. Where
in-band dispatch is wanted, it belongs inside memQL, not in the proxy.

### D4 — `api.<domain>` replaces `cockpit.<domain>`

Named for the role. Pre-release, so no alias and no redirect (CLAUDE.md rule 2).
Cutover sequencing in §8.

### D5 — A site is data, not infrastructure

`v1:platform:site` — hostname, kind, bundle reference, status. Deploying is an
upload plus a row write; rolling back is a row write. No Kubernetes object, no
git commit, no ArgoCD reconcile per site.

*Rejected: a Deployment, Service and Ingress per site, rendered into git.* Not
mainly on the pods — on what it does to the trust model. `deploy/argocd/apps/root.yaml`
watches `github.com/znasllc-io/memql.git`, so the portal would have to **author
Kubernetes manifests and push them to a repository ArgoCD reconciles.** Git stops
being the human-reviewed record and becomes a queue a web application writes to,
and the thing that renders customer websites becomes a cluster-takeover surface.

*Rejected: bytes into a volume the bff serves.* A site deploy would restart the
API node — publishing a landing page drops live gRPC streams. Fixing that with a
shared PVC rebuilds D5 with worse storage, and RWO volumes cannot be shared
across replicas.

**This does not violate "ArgoCD is the only deploy path."** That rule is about
the shape of the system. The edge Deployment lives in git and is reconciled like
everything else. A customer's landing page is no more a Kubernetes object than a
chat message is.

### D6 — The site concept is admin-tier, not owner-tier

`@rowAuthz(owner="ownerUserId")` would be wrong here, and measurably so. §5.1 of
the account-isolation design records that a concept declares **one** tier and
that `enforceRowAuthzOnPlan` ANDs the owned predicate **unconditionally**, with
no cluster-owner escape on the read path. So on an owner-tier concept, "list
every site in this cluster" is not merely unimplemented — it is *not
expressible*, and an admin asking for it gets a confidently wrong subset. That
query is the portal's primary screen.

Sites are cluster infrastructure. They are gated by a context-spec conjunct over
the `@actor` envelope (the `requiresAdmin` shape CLAUDE.md documents), which
also makes **cluster-wide hostname uniqueness checkable** — you cannot enforce
uniqueness against rows you are not allowed to see.

### D7 — The edge is its own node type

Build tag `edge`, `make edge`, its own Deployment and Service.

*Rejected: a handler mounted on the bff.* Three reasons. A website-only cluster
should be able to drop to four nodes (D1's cost mitigation). A site deploy must
never share fate with the API. And per-node-type binaries is the pattern the
repo already uses for exactly this reason.

### D8 — The portal is site #1

Same concept row, same storage resolution, same serving path, same versioning,
same headers as any customer site. The **only** difference is where its hostname
comes from: the cluster install, not the portal UI.

Its row is written by a `seed` construct — which resolves the bootstrap
chicken-and-egg (you cannot use the portal to create the portal) without a
special case, and re-seeds at boot so an admin cannot brick the cluster by
deleting a row. The row carries `systemOwned: true`, which blocks deletion; it
does not branch the serving path.

**Enforced, not intended:** a test asserts the portal row is created through the
same public path a customer site is, and that no `site.id == "portal"` branch
exists in the serving path. Same enforcement style as
`TestClientsDirectoryIsAllowlisted`.

`bundleRef` is a URI with a scheme, which is what makes "in the image" versus
"in blob storage" a difference in *data* rather than in *code path*:

- `file:///app/portal` — the bundle the image ships (the portal today)
- `blob://sites/<id>/<version>/` — an uploaded bundle
- `file:///abs/path/dist` — a working tree, preserving the
  `MEMQL_PORTAL_DIST=clients/portal/dist go run .` inner loop

### D9 — The edge proxies `/_memql/*` same-origin

A hosted site reaches the API through its own origin; the edge forwards to the
bff. No CORS, no cluster domain compiled into the customer's bundle, and no
dependence on cross-site cookies — `component/server/token_cookie.go:53` sets
the auth cookie `SameSite=Lax`, which is not sent cross-site in any browser
today, before third-party cookie deprecation is even considered.

Costs one internal hop, and only for hosted sites. The Cockpit, the VS Code
extension, the SDKs and workers keep dialing `api.<domain>` directly.

### D10 — Publish is an atomic version flip

A build uploads the **whole bundle under a new version prefix**, then flips
`bundleRef` on the row. Deploy is atomic; rollback is a single row write; the
graph's own history is the version list.

Credential: a `class="service_account"` JWT (memql#691) — it exists, it is the
CI/automation identity, and it verifies on the mesh where a PAT cannot.

Transport: **HTTP multipart, as a documented exception**, on the same reasoning
already recorded for `/spaces/{id}/attachments` — multipart bundles map poorly
to gRPC. This requires explicit sign-off under the endpoint-protocol policy and
must be added to that table in CLAUDE.md as part of the work, not after it.

### D11 — Dynamic data comes from the graph; prerendering is a build concern

For a headless storefront: the third party pushes to `/inbound/{source}`
(`component/inbound/`, deny-by-default source allowlist plus per-source HMAC),
data lands in the graph, and the site reads it same-origin through D9.

The build emits prerendered HTML per route so crawlers and first paint work.
**Prerendered HTML is a snapshot for crawlers and first paint; live values come
from the graph on hydrate.** Otherwise a price change makes every prerendered
page wrong until the next build.

The edge therefore stays a file server. Its resolution order is:
exact file → `<path>/index.html` → `<path>.html` → (kind `spa`) `index.html`,
(kind `static`) 404.

*Rejected: server-side rendering per site.* Reopens D5 immediately.
*Rejected: the browser calling the third-party API directly.* Legitimate, and
memQL would own nothing on the page — which makes this a hosting company rather
than a platform.

**No integration for any specific commerce provider is in scope here.** Its
shape is determined by this decision (inbound webhook, concepts, backfill), and
it is a separate project.

### D12 — `v1:identity:account` is parked, not removed

No customer registry is planned. The concept stays where it is, and
`account-isolation-model.md` gains a status note recording that
per-cluster-per-customer is the chosen isolation model and that §6(a)/(b)/(c)
are parked.

Leaving it is safe as a measured property, not a hope: §3.3 of that document
states the credential *"authorizes nothing today, and that is a checked property
rather than an aspiration"* — no verifier resolves the `mql_acct_` prefix, no
interceptor admits it, `dsl/identity` deliberately declares no by-`keyHash`
lookup for the family, and tests in `component/identity/accounttoken` pin both
absences. Removal spans ~15 non-test files plus proto messages and is a separate
decision, available later at no penalty precisely because the thing is inert.

`v1:identity:accountEntitlement` is a different concept — a per-user concurrency
cap keyed on user id (`dsl/planner/queries.memql:171`: "the account == the user
in v1") — and is **not** in scope.

---

## 3. Architecture

```
                       :443                              :80
                         │                                │
                 ┌───────┴────────┐                   redirect
                 │  traefik/nginx │
                 └────────┬───────┘
    TLS terminated once, with *.<domain> + <domain>, issued at install
                          │
   ┌──────────┬───────────┼────────────┬──────────────────┐
   ▼          ▼           ▼            ▼                  ▼
api.<d>  identity.<d>  mcp.<d>      *.<d>            <d> (apex)
   │          │           │            │                  │
   │      identity:8085  mcp:8090      └────── edge:8085 ──┘
   │       (https)       (http)                 (http)
   │                                              │
   ├── /                  ──▶ bff:50051 (h2c)     │
   └── derived HTTP set   ──▶ bff-http:8085       │
       (§5 — generated, not authored)             │
                                                  │
                          ┌───────────────────────┼───────────────────┐
                          ▼                       ▼                   ▼
                   v1:platform:site          blob storage      /_memql/* ──▶ bff
                   (hostname → bundle)        (bundles)      (same-origin proxy)

separately, and permanently:  livekit.<domain> + a UDP media plane
```

Request paths:

- **A client dials the API.** `api.<domain>:443` → TLS at the proxy → h2c to
  `svc/bff:50051`. Unchanged from today except the name.
- **A browser loads a hosted site.** `shop.<domain>` → proxy → `svc/edge` →
  hostname lookup against `v1:platform:site` (cached) → bytes from
  `blob://sites/<id>/<version>/`.
- **That site's JS calls the API.** `shop.<domain>/_memql/ws` → same edge →
  forwarded to the bff. Same-origin throughout.
- **A third party posts a webhook.** `api.<domain>/inbound/shopify` — which
  requires the bff's HTTP paths to be reachable on `api.<domain>`, closing
  §1.3(a). See §5.

---

## 4. `v1:platform:site`

| Field | Type | Notes |
|---|---|---|
| `hostname` | `string!` | Fully qualified. Cluster-unique — enforced server-side, which D6 makes possible |
| `kind` | `enum("spa","static")!` | Selects the tail of the D11 resolution order |
| `bundleRef` | `string!` | URI; `blob://` or `file://` (D8) |
| `status` | `enum("draft","live","disabled")!` | `draft` resolves for nobody; `disabled` answers 503 rather than 404, so a paused site is distinguishable from a typo |
| `apiProxy` | `bool` | Mount `/_memql/*` for this origin (D9) |
| `systemOwned` | `bool` | Blocks deletion. The portal only |
| `title`, `notes` | `string` | Operator-facing |

Authorization tier: **admin**, per D6, gated by a context-spec conjunct over the
`@actor` envelope. Not `@public` — §7 of the account-isolation document records
that `@public` is matched ahead of every other tier, carries no runtime
semantics, and permanently blocks tier inference for the concept afterwards.

Versioning is the graph's own history: `bundleRef` on an earlier row version is
the previous deploy, so rollback needs no separate version table.

---

## 5. The bff's HTTP paths, finally routed

`api.<domain>` must carry both protocols, which is the constraint that shaped
today's `bff` / `bff-http` split. Two rules on that host, not one:

- `/` → `svc/bff:50051` (h2c) — gRPC
- the declared HTTP set → `svc/bff-http:8085`

The HTTP set is **not enumerated by hand in YAML**. It is exactly
`server.PublicPaths()` ∪ `HandlerAuthorizedPaths()` ∪ `SelfAuthenticatedPaths()`
minus the gRPC surface, and those functions already exist and are already
verified against real registration by `TestContractRoutesMatchesRegistration`.
The work is to **generate the Ingress paths from them and fail CI when the two
disagree**, so §1.3(a) cannot recur: a new HTTP path on the bff either appears
in the front door or breaks the build.

This is the one place a rule is derived rather than written, and it is derived
from a list the repo already keeps honest.

---

## 6. The edge node

Built from `component/portal` generalized: it already does SPA fallback, asset
caching, CSP and security headers for exactly one site. What it gains:

- **Hostname → site resolution**, cached, invalidated on the concept's change
  feed (the mechanism `CodeProfileSubscriber` already uses for
  `v1:observability:codeProfile`).
- **A storage-backed bundle reader** behind the `bundleRef` scheme.
- **The `/_memql/*` reverse proxy** (D9), enabled per site.
- **Per-site CSP**, derived from the site's own origin rather than from
  `MEMQL_IDENTITY_BASE_URL` as `component/portal/csp.go` does today.

Config: `MEMQL_EDGE_*`. `MEMQL_PORTAL_DIST` survives as the portal site row's
`bundleRef` default, so the existing inner loop keeps working.

`component/architecture/embedded/topology.model.json` is auto-generated and must
be regenerated when the node type lands.

---

## 7. What is deleted

- `deploy/k8s/overlays/local/identity-external.yaml` and the `8085`
  loadbalancer mapping — a second entrance to identity that exists in no other
  environment.
- The `7880` mapping — it points at a Deployment the local overlay deletes.
- The three path rules on the cockpit host, replaced per §5.
- `cockpit.<domain>` itself. No alias, no redirect (CLAUDE.md rule 2).
- The static client list in `component/genesis/domain.go:61`. OAuth clients come
  from `v1:identity:oauthClient` — the dynamic-registration store that shipped
  in #1573/#1586 and that `ClientAllowsRedirectURI` already consults alongside
  the static list. Dogfooding here is mostly deletion.

`5432` stays, documented as debug-only and not a connection path.

---

## 8. Cutover sequence

These move together or a developer's machine breaks mid-flight. Ordered:

1. `component/genesis/domain.go` — derive `api.` (and the portal's own origin),
   keep deriving nothing else that names a host.
2. `deploy/k8s/overlays/local` — the five rules; delete the two extra host ports.
3. `scripts/lib/localtls.sh` — unchanged (already a wildcard), verified.
4. `scripts/install/hosts-entries.sh` — the managed block's default hostname set.
5. `scripts/install/verify-frontdoor.sh` — default probe hosts.
6. `scripts/k3d/up.sh` — the `--port` table.
7. `editors/vscode` — `composeEndpointFromDomain`, the collect-screen default,
   and the install-wizard steps that name hosts.
8. `~/.memql/clusters.yaml` — a documented one-line edit in the release notes.
   Not migrated automatically: it is the operator's file.

The certificate needs no reissue at any step, which is what makes this
sequencable at all.

---

## 9. Documentation

`docs/CLAUDE.md` requires the affected page to be updated **in the same commit**
as the change. Documentation is therefore an acceptance criterion per phase, not
a trailing phase.

| File | What changes |
|---|---|
| `docs/public/operate/environment-parity.md` | Connection-model diagram and the parity table both name `cockpit.<domain>` |
| `CLAUDE.md` (root) | Endpoint-protocol table (the D10 exception), local-cluster section, node-type list, build tags |
| `docs/public/build/build-tags.md` | The `edge` tag |
| `docs/public/operate/reproduce-staging-locally.md` | Hostnames, ports, runbook |
| `docs/public/operate/portal.md` | The portal's own origin |
| `clients/README.md` | "Served by a Go package at a sub-path" becomes "a site row served by the edge" |
| `component/portal/doc.go` | Its no-`go:embed` rationale becomes the edge's general case |
| `docs/public/operate/install-prerequisites.md`, `docs/public/overview/quickstart.md` | Hostname set |
| `docs/public/operate/campaign-sending.md` | The unsubscribe base-URL example |
| `docs/public/operate/auth/identity-service.md` | CORS origins, registered clients |
| `GLOSSARY.md` | Index entries for the two new pages |
| **New** `docs/public/operate/front-door.md` | The five rules, what is behind each, why |
| **New** `docs/public/operate/site-hosting.md` | Deploy a site, roll it back, the bundle contract |
| `docs/internal/design/account-isolation-model.md` | Status note per D12 |

---

## 10. Verification

**Render.** `kustomize build deploy/k8s/overlays/local` asserts exactly five
HTTP Ingress rules, that no `cockpit.` literal survives anywhere under
`deploy/`, and that the bff HTTP path list equals §5's derivation. This is the
test that makes §1.3(a) structurally unable to recur.

**Front door.** `scripts/install/verify-frontdoor.sh` extends to the new host
set: dns, tls and h2 per host, plus a plain HTTP/1.1 probe of one bff HTTP path
(`/healthz` through `api.<domain>`) — the check that would have caught the
missing `/unsubscribe` route.

**Edge.** Hostname resolution including miss (404), `draft` (404) and `disabled`
(503); the D11 resolution order for both kinds; bundle read from both `bundleRef`
schemes; the `/_memql/*` proxy preserving the WebSocket upgrade; per-site CSP.

**Dogfood.** The portal row is created through the public path; no
`site.id == "portal"` branch exists in the serving path; deleting the row and
restarting restores it.

**Publish.** Atomicity — a failed mid-upload leaves the previous version live;
the flip is a single write; rollback restores byte-identically.

**Authorization.** The site concept classifies `admin` in
`test/dslconformance`; `@public` on it is refused; hostname uniqueness holds
across owners.

**Cannot be verified from the authoring machine.** That box has no docker group,
k3d or kubectl. Every cluster-gated check runs on an operator machine or in CI:

```bash
make up-refresh
kubectl -n memql get ingress -o wide
scripts/install/verify-frontdoor.sh
curl -sS https://api.<domain>/healthz
curl -sS https://<domain>/            # the apex site
```

---

## 11. Out of scope

- **Per-site custom domains and runtime ACME.** Designed and shelved under D2.
  The shape, if it is ever needed: HTTP-01 only, a pending/verifying state
  machine on the site row, and either cert-manager objects per domain (single
  routing plane, two objects per domain, unrehearsable locally) or TLS
  termination inside the edge behind SNI passthrough (zero objects, but the
  passthrough-versus-terminated precedence differs between traefik and
  ingress-nginx and must be spiked before it is committed to).
- **Any specific commerce integration.** Shape determined by D11; separate project.
- **Fleet management across clusters.** D1's accepted cost. `clusters.yaml` and
  `v1:cluster:deployment` already exist; aggregation above them is later work.
- **Removing `v1:identity:account`** (D12) and **`accountEntitlement`** (unrelated).
- **§6(a)/(b)/(c) of the account-isolation design.** Parked, not cancelled.
- **Cluster profiles as a first-class concept.** D1 names the mitigation and the
  precedent; formalizing it is separate.
- **Rebuild-on-data-change for prerendered routes.** D11 makes hydration the
  freshness mechanism; triggering a rebuild when a new route appears is later.

---

## 12. Open risks

**The bff HTTP path derivation (§5) is the one generated rule.** If the
generator and `PublicPaths()` disagree in a way CI does not catch, §1.3(a)
recurs in a new form. The mitigation is that the disagreement is what the test
checks, but the test has to be written before the generator is trusted.

**MCP's ingress is asserted, not measured.** `deploy/k8s/base/mcp.yaml` says a
public ingress targets :8090, but no overlay in this repo defines one. The rule
in D3 is therefore the first real exercise of that port and may surface protocol
details (streamable HTTP versus SSE, timeouts) that nothing here has tested.

**Per-customer cost is a business assumption, not a technical one.** D1's floor
is measured; whether it is affordable per customer depends on a price point this
document has no opinion about. If it is not, cluster profiles stop being a
mitigation and become a prerequisite.

**The `/_memql/*` proxy puts Go on a path that was previously direct.** Only for
hosted sites, but WebSocket upgrade handling, backpressure and connection
lifetime through a reverse proxy are where this class of thing goes wrong. It
deserves its own load test rather than a unit test.
