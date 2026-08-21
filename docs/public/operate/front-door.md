---
title: The cluster front door — six host rules
audience: public
status: stable
area: operate
sinceVersion: 0.18.0
owner: znas
---

# The cluster front door — six host rules

A MemQL cluster has **one** door: port 443 on one L7 proxy — the k3s-bundled
traefik locally, nginx in the cloud — terminating TLS once with one certificate
and routing by hostname. Port 80 exists only to redirect.

Behind that door are **six host rules**, committed to `deploy/k8s`, plus a
**separate media plane** that does not and cannot go through it.

Six is the number, and it stays six no matter how many customers,
applications or websites the cluster serves. That property is what this page is
about.

The host **set** is DERIVED from the closed **role** set plus the platform's
own site (memql#3767, memql#4224) rather than maintained as a list. It grows
only when a ROLE is added, which is a design change; it never grows with
customers, apps or sites.

Related: [environment-parity.md](environment-parity.md) ·
[install-prerequisites.md](install-prerequisites.md) ·
[reproduce-the-cloud-locally.md](reproduce-the-cloud-locally.md)

---

## The six hosts

| Host | Backend | Protocol | Certificate SAN |
|---|---|---|---|
| `api.<domain>` | `svc/bff:50051` **and** `svc/bff-http:8085` | h2c (gRPC) + http | yes |
| `identity.<domain>` | `svc/identity:8085` | https | yes |
| `mcp.<domain>` | `svc/mcp:8090` | http | yes |
| `portal.<domain>` | `svc/edge:8085` | http | yes |
| `*.<domain>` | `svc/edge:8085` | http | **no** — see below |
| `<domain>` (apex) | `svc/edge:8085` | http | yes |

**Every host is a single label under the domain, and that is a routing
decision.** An Ingress wildcard matches exactly **one** label, so the one
`*.<domain>` rule routes every present and future site to the edge. It used to
be a TLS decision too — one `*.<domain>` certificate covering every role host
and every site, with the apex as the lone extra SAN — and that was never true
of a running cluster (memql#4224). **The cloud issuer solves HTTP-01 only.**
ACME cannot serve an HTTP-01 challenge for a wildcard, and one wildcard
`dnsName` fails the whole order, so the Certificate sat Pending; when it was
hand-edited to exact names, the edge Ingress whose `tls.hosts` still said
`*.<domain>` made ingress-nginx serve its self-signed default for
`portal.<domain>` (Safari: "This Connection Is Not Private"). So the front-door
certificate names **exact hosts only** — `api.`, `identity.`, `mcp.`, `portal.`
and the apex — every Ingress lists exactly its own exact rule hosts under
`tls`, and the union of those lists is the certificate's `dnsNames`. All three
are gated by `deploy/k8s/overlays/frontdoor_hosts_test.go`, against both
instance overlays.

**The wildcard rule has no certificate behind it.** A site routed by
`*.<domain>` terminates TLS with the ingress controller's default certificate
until it has a `Certificate` and an exact-host Ingress of its own — see
[site-hosting.md](site-hosting.md#2-add-the-hostname) — and that stays true
until the issuer gains a DNS-01 solver. The portal is the exception because it
is the one site the platform ships itself: its name exists before any operator
creates a row, so the generator can write its rule and SAN, and the engine seeds
its `v1:platform:site` hostname from `MEMQL_DOMAIN` through the same derivation
(`frontdoor.PortalHost`). The exact rule is a TLS artefact, not a second way to
serve the portal: ingress-nginx builds a certificate-bearing server block per
**rule** host, never per `tls` host, so a `tls.hosts` entry alone would have
changed nothing — which is why the ops workaround on the first keep-it cluster
was an extra Ingress, and why that is now the generated shape.

(The set was once the product of role × ENVIRONMENT, with a label that
hyphenated into role hosts — `api-staging.<domain>` — and nested into site
hosts. Epic memql#3943 removed the environment dimension: MemQL ships one
installation shape, and a second environment is a second install with its own
domain, so the product has one factor left.)

**`api.<domain>`** is the API edge, and it is named for that role rather than
for the first client that happened to connect to it. Everything that speaks to
the engine dials it: the Cockpit, the VS Code extension, `sdk/go`, `sdk/ts`,
workers, and the browser bridge at `/memql/ws`. So do strangers — a webhook
POSTing to `/inbound/<source>`, a recipient's mail client executing RFC 8058
one-click against `/unsubscribe`. It is the one host carrying two backends;
[the next section](#one-constraint-explains-most-of-this-page) is why.

**`identity.<domain>`** is the identity service: `/login`, the WebAuthn
ceremonies, the OAuth endpoints, `/.well-known/jwks.json`, `/me/*`, `/enroll`
and `/device`. It serves TLS in-cluster under the internal CA, which is why the
proxy speaks https to it and skips verification on that hop — the hop stays
inside the cluster network. See
[auth/identity-service.md](auth/identity-service.md).

**`mcp.<domain>`** is the MCP Streamable HTTP protocol head. It gets a host of
its own rather than a path under `api.` for the same per-Service reason below,
and it is not proxied through the edge either: MCP clients configure a URL,
they are not browsers, and an extra hop on a tool-calling path buys nothing.
See [mcp-connect.md](mcp-connect.md).

**`portal.<domain>`, `*.<domain>` and the apex** all point at the `edge` node,
which resolves the request `Host` header against a `v1:platform:site` row and
serves that site's bundle. The apex is not a special case: for a customer's own
cluster the bare domain **is** their main website, so it is a site row like
every other one. The portal is site #1 and takes the same path; its own rule
exists for the certificate's sake ([above](#the-six-hosts)), not because the
edge treats it differently.

> **INFO: the edge route and the edge backend both ship as of memql#3714.**
> `deploy/k8s/overlays/local/edge-front-door.yaml` carries the wildcard and
> apex rules (`portal-front-door.yaml` the portal's);
> `deploy/k8s/base/edge.yaml` carries the Deployment, Service and
> PodDisruptionBudget behind `svc/edge`. The `edge` node type itself — build
> tag `edge`, `make edge`, see [build-tags.md](../build/build-tags.md) — is
> now a normal member of the mesh like any other leaf node, and its release
> image ships from `.github/workflows/build-engine-images.yml` alongside
> every other node type (CLAUDE.md's build-server rule).
>
> **Before the backend existed, the absent-Service case produced a 404, not a
> 503** — worth keeping in mind, because it is the same 404 an UNROUTED
> hostname produces, and the two are not the same failure. Traefik drops the
> entire router when its backend Service is absent, so the request matches no
> router at all and gets traefik's own 404 — it never reaches a handler that
> could answer 503. Traefik said so on every reconcile:

```
ERR Cannot create service error="service not found" ingress=edge-front-door serviceName=edge servicePort=8085
```

> An operator seeing 404 on a hosted-site hostname could not tell a missing
> *rule* apart from an undeployed *node* from the status code alone — both are
> Go's byte-identical `http.NotFound`. `kubectl -n memql get svc edge` is the
> discriminator, and now that the Service is a permanent fixture of the base
> manifests, the case this warns about is a regression rather than the
> expected state of `main`. A live edge still answers 404 for any hostname
> with no matching `v1:platform:site` row, and 503 only for a row whose
> `status` is `disabled` — that part of the contract does not change.

The site-hosting runbook covering how to deploy a site, roll one back and what
the bundle contract is arrives with the same work. This page deliberately says
nothing more about sites than the rule that routes them.

### Exact-versus-wildcard precedence is declared, not inherited

`*.<domain>` also matches `api.<domain>`, `identity.<domain>`, `mcp.<domain>`
and `portal.<domain>`. So whether the named hosts keep their own backends is
not a detail — it is a **load-bearing assumption of the six-host design**, and
it is worth knowing the state of it. (For the portal the question is moot —
its rule and the wildcard reach the same Service — which is exactly why it is
not in the precedence probe's host set.)

**Precedence between an exact host and a wildcard host is
implementation-defined.** The Ingress specification says what a wildcard host
*matches* — exactly one DNS label, never the apex — and says nothing about which
rule wins when an exact rule and a wildcard rule both match. There is no spec
guarantee to lean on.

**On traefik's defaults it probably points the wrong way.** Traefik's Kubernetes
Ingress provider compiles a wildcard host into a `HostRegexp` matcher, and
traefik's default router priority is **the length of the matcher rule**. In
shape, the two rules are:

```
Host(`api.memql.localhost`) && PathPrefix(`/`)
HostRegexp(`^[a-zA-Z0-9-]+\.memql\.localhost$`) && PathPrefix(`/`)
```

The wildcard's rule is the **longer** string, so by that default heuristic it
would outrank the exact one. (The precise regex traefik emits varies by
version; the relative length is the part that carries the argument, and it is
not close.) Left undeclared, the consequence would not be a narrow band of
unusual hostnames — it would be that `api.<domain>/` goes to the site server,
so **the entire API would be served by the edge**: every SDK, the Cockpit, the
VS Code extension and every worker dials that host. ingress-nginx ranks
differently again — longest path, then oldest Ingress — so it would not even
be the same wrong answer everywhere.

**So precedence is declared, not inherited -- but not the way it first shipped
(memql#3714, revised memql#3810).** `api-front-door.yaml`, `front-door.yaml`
(identity) and `mcp-front-door.yaml` originally each carried an explicit
`traefik.ingress.kubernetes.io/router.priority: "100"` — comfortably above the
wildcard's computed default. That broke the API: the annotation is
**Ingress-level**, so the single value applied to every router that Ingress
generates, and `api-front-door.yaml` alone declares 22 paths whose relative
ordering had depended on traefik's default (the compiled rule length). A
uniform `"100"` erased that variance, so `/` — the h2c gRPC catch-all pointed
at `bff:50051` — outranked the 21 specific paths pointed at `bff-http:8085`,
and every HTTP path on `api.<domain>` answered `415 Unsupported Media Type`
with `Content-Type: application/grpc`.

The fix moved the declaration to the other side of the relationship instead of
removing it: **`api-front-door.yaml`, `front-door.yaml`, `mcp-front-door.yaml`
and `portal-front-door.yaml` carry NO `router.priority` annotation at all**
(each file says so explicitly, in a comment naming memql#3810). Precedence over
the wildcard is still declared — it has to be, or the wildcard genuinely does
swallow the exact hosts — but it is declared **on the wildcard itself**, in
`edge-front-door.yaml`, whose two rules (`*.<domain>` and the apex) carry
`traefik.ingress.kubernetes.io/router.priority: "1"`. Priority `1` loses to
every rule-length default, so all four exact hosts outrank the wildcard while
keeping their own paths ordered by length exactly as before — the wildcard's
two same-shaped `/` rules have no intra-host ordering for a uniform value to
destroy, which is what makes it safe to put the annotation there and nowhere
else. `deploy/k8s/overlays/local/render_priority_test.go` gates this shape:
no Ingress declaring more than one distinct path may carry a uniform priority,
and `edge-front-door.yaml` must carry one. On ingress-nginx nothing needs
declaring: nginx resolves `server_name` by specificity in its own core — an
exact name beats a leading wildcard — so the generated cloud front door carries
no priority annotation at all, and `cmd/frontdoorhosts/manifest.go` says not
to add one (an Ingress-level priority is what flattened the api host's path
ordering on traefik). The principle is the same on both: know what the
controller's ranking is, and do not trust a heuristic the spec does not
guarantee.

**Checked by `scripts/install/verify-frontdoor.sh`'s `precedence` check**,
reported per host for `api.` and `identity.` only — never `mcp.`, whose Ingress
routes `:8090` while the `/healthz` the check reads is on `:8085`, so it can
report no better than inconclusive for that host. Before `svc/edge` existed, a
run could not establish the property at all: with the backend Service absent,
traefik drops the wildcard router entirely, so there was nothing for an exact
host to take precedence *over*, and an exact host answering from its own
backend under those conditions measured the absence of the wildcard router, not
precedence. Reading that result as the latter is exactly how this assumption
came to be written down as a fact the first time.

Worth being clear about even with the check passing: a probe compares responses
at the paths it dials, so a passing check is evidence for the design rather than
proof of it at every path. The **declared** priority is what holds everywhere,
including the paths and hosts nothing dials — which is why declaring it matters
more than measuring it, and why the declaration above is not conditioned on the
check passing.

## One constraint explains most of this page

**An ingress controller's backend protocol is a per-Service setting, not a
per-port one.**

That single fact shapes the whole front door:

- It is why `bff` and `bff-http` exist as **two Services over one Deployment**.
  Locally the `bff` Service carries
  `traefik.ingress.kubernetes.io/service.serversscheme: h2c` so traefik speaks
  HTTP/2 cleartext to the gRPC edge on `:50051` — and that annotation applies
  to every port of that Service. The cloud has the mirror-image constraint
  (nginx `backend-protocol: GRPC`). An HTTP/1.1 asset fetch dialed as h2c does
  not work against Go's `net/http` server, so the portal bundle would simply
  fail to load.
- It is why `api.<domain>` needs **two rules**, not one: `/` to
  `svc/bff:50051` for gRPC, and the declared HTTP path set to
  `svc/bff-http:8085`.
- It is why MCP is a **host** rather than a path: `svc/mcp` is a different
  Service on a different port speaking plain HTTP.
- It is why the HTTP path set has to be **complete** — which is the next
  section, and the reason it is generated.

Splitting protocols across Services makes each backend's protocol
unambiguous, and it is the same shape everywhere. Only which ingress
controller points at it differs, and that is a value rather than architecture.

## What is generated

Two things in the front door are derived rather than authored, by two
generators that answer different questions and stay separable.

**The HOSTS** — `cmd/frontdoorhosts` writes `front-door.generated.yaml` into
each instance overlay (`deploy/k8s/overlays/cloud` and `overlays/cloud-entry`)
whole: the six Ingress rules and the cert-manager `Certificate` with its
exact-host SANs. Its whole input is the closed role set, the portal and the
domain, and it emits ~440 lines from those — which is what earns generation for
a listed target.

**The PATHS** on the api host — `cmd/frontdoorpaths` fills the block between
markers in every api front door, from `component/server`'s own declarations. The
hosts generator preserves that block across a regeneration rather than
re-deriving it; the rest of this section is about that half.

```yaml
          # BEGIN generated bff HTTP paths -- make frontdoor-paths
          # END generated bff HTTP paths
```

| Command | What it does |
|---|---|
| `make frontdoor` | Both, in order: hosts, then paths |
| `make frontdoor-hosts` | Regenerates every instance overlay's front door |
| `make frontdoor-hosts-check` | Fails when a generated front door is stale |
| `make frontdoor-paths` | Regenerates the path block in every api front door |
| `make frontdoor-paths-check` | Fails when a checked-in block is stale |

The **local** overlay's five front-door files stay hand-authored: they are
traefik rather than nginx, and they carry the measured reasoning for a priority
ranking that broke the API once already
([above](#exact-versus-wildcard-precedence-is-declared-not-inherited)). They are
not unchecked — `TestFrontDoorServesExactlyTheDerivedHosts` computes the host
set from the same `component/frontdoor` package the generator uses, so local's
committed defaults cannot drift from what the cloud overlay serves.

The set is exactly `server.PublicPaths()` ∪ `HandlerAuthorizedPaths()` ∪
`SelfAuthenticatedPaths()` minus the gRPC surface. Those functions already
exist, and `TestContractRoutesMatchesRegistration` already verifies them
against what the handler really registers — so the front door reads from a list
the repo keeps honest instead of from somebody's memory. Hand-edits between the
markers are reverted by the generator; the pair mirrors `make arch-model` /
`make arch-model-check`.

**Why it is generated is MemQL's own history.** `POST /inbound/{source}` and
`GET+POST /unsubscribe` are documented public HTTP exceptions that third
parties dial — a Shopify webhook, a mail client executing RFC 8058 — and **no
overlay in this repository routed either one.**

And here is the part worth memorising, because it is what makes a
hand-maintained list of ingress paths a bad idea rather than merely a chore:

> **WARNING: a missing rule does not 404.** The HTTP/1.1 request falls through
> to the catch-all `/` rule, reaches the gRPC backend, and comes back as an
> HTTP `415` describing a content-type problem.

```
HTTP/1.1 415 Unsupported Media Type
Content-Type: application/grpc
Grpc-Status: 3
Grpc-Message: invalid gRPC request content-type ""
Content-Length: 0
```

Measured on a live cluster against `api.memql.localhost`, on four paths that
were not yet in the generated block at the time: `/unsubscribe`,
`/inbound/shopify`, `/memql/query` and `/spaces/x/attachments`. All four
answered exactly that, with an empty body.

Read the trap carefully, because it is worse than an error that says nothing.
The transport is fine — curl completes, TLS is fine, the door is up. The
response **names a cause, and it is the wrong one**: it says the caller sent no
gRPC content-type, when what actually happened is that a routing rule is
missing and the request was handed to a server that only speaks gRPC. Nobody
debugging a webhook that stopped arriving is going to read "invalid gRPC request
content-type" and think "Ingress".

What makes it findable is that `Grpc-Status: 3` on a path you expected to be
plain HTTP is a specific, greppable signature. If you see it, check whether the
path is in the generated block before you look at anything else.

That is why the list became generated output with a CI gate: a new public HTTP
path on the bff either reaches the front door or breaks the build. For contrast,
on the same host the routed paths answer 200 — `/healthz` returns the bff's
health JSON, `/portal/` returns the portal's `index.html`.

See [inbound-delivery.md](inbound-delivery.md) and
[campaign-sending.md](campaign-sending.md) for the two exceptions themselves.

## Why the count is six, and must not grow

Because **a site is data, not infrastructure.**

`v1:platform:site` carries a hostname, a kind, a bundle reference and a status.
Deploying a site is an upload plus a row write; rolling one back is a row
write. There is no routing rule per site, no git commit per site and no ArgoCD
reconcile per site — so adding the tenth website to a cluster adds a **row**,
not a routing rule and not an overlay patch.

The `*.<domain>` rule is what makes that possible: one wildcard rule routes
every present and future site name to the one node that knows how to look them
up — every name, that is, that the five exact hosts have not claimed, which is
an assumption about rule ranking rather than a given
([above](#exact-versus-wildcard-precedence-is-declared-not-inherited)).

**The certificate is not part of that claim in the cloud** (memql#4224). It
names exact hosts, because the issuer is HTTP-01, so a new site gets routing
for free and TLS only with a `Certificate` and exact-host Ingress of its own —
one object pair per site, the one explicit exception to "a site is data", and
it stands until the issuer gains a DNS-01 solver. Locally the mkcert pair is a
wildcard, so a new site needs no reissue there — which is exactly why a site
that works over https locally is no evidence it has a certificate in the cloud.
The portal is the one site whose rule and SAN the generator writes, because it
is the one site whose name is known before any row exists.

This is not a hole in "ArgoCD is the only deploy path". That rule is about the
shape of the system: the edge Deployment lives in git and is reconciled like
everything else. A customer's landing page is no more a Kubernetes object than
a chat message is.

Two consequences worth stating plainly:

- **Adding a seventh host rule — a fourth ROLE — is a design change, not a
  configuration change.** If a proposal needs one, the thing being added is
  probably a site. `TestRenderedHostsAreExactlyTheProduct` fails on a seventh
  host rule that is not one of the six and is not the media plane.
- **A wildcard DNS record is not a wildcard hosts file.** In the cloud
  `*.<domain>` resolves at the DNS layer and nothing local is involved. On a
  developer machine there is no wildcard in `/etc/hosts`, so each name has to
  be listed in the managed block `scripts/install/hosts-entries.sh` owns. That
  is a property of hosts files, not of the front door.

## The media plane is separate, permanently

Voice is not one of the six hosts and never will be. **WebRTC media is UDP and
cannot traverse an HTTP front door.**

| Target | Media plane |
|---|---|
| Cloud | `livekit.<domain>` for signaling, plus a LoadBalancer carrying UDP 7882 and TCP 7881 for media |
| Local | LiveKit Cloud (the local overlay deletes the self-hosted livekit workloads) |

So the honest statement of the topology is **six HTTP host rules plus a
separate media plane** — said here rather than left for someone to discover
while wondering which Ingress rule is missing. See
[livekit-provision.md](livekit-provision.md) and
[telephony.md](telephony.md).

## Local versus cloud

The front door is the same shape everywhere. What differs is which
implementation of each interchangeable part is in play.

| | Varies | Does not vary |
|---|---|---|
| Proxy | traefik locally (`serversscheme` annotations) / nginx in the cloud (`backend-protocol`) | that there is exactly one L7 proxy on 443 |
| Certificate | mkcert locally (a `*.<domain>` + apex pair) / cert-manager in the cloud (exact hosts only — HTTP-01 cannot issue a wildcard, memql#4224) | one Secret, `memql-front-door-tls`, that every front-door Ingress terminates with, issued at install |
| DNS | the `/etc/hosts` managed block locally / real DNS in the cloud | the hostnames themselves |
| Secrets | `make secrets` locally / External Secrets + Key Vault in the cloud | — |

> **WARNING: the SAN set is the one place local is more permissive than the
> cloud.** The local wildcard covers every site; the cloud certificate covers
> the five exact hosts and nothing else. A site that works over https on the
> local cluster has proven its routing, not its certificate.

Fixed everywhere: **the six host rules, the paths behind them, and the dial
path a client uses** (`https://api.<domain>`, TLS at the proxy, gRPC to the
bff). The domain itself is a value too — one `MEMQL_DOMAIN`, from which every
domain-shaped setting is derived at boot — so no file under `deploy/` names a
domain except the Ingress hosts, which carry a committed default.

The domain reaches every side through ONE derivation: `cmd/frontdoorhosts`
composes the Ingress hosts and certificate SANs from it,
`component/envregistry/domain.go` composes the issuer, CORS origins, redirect
URIs and MCP public URL from it, and `component/memql`'s SeedMaterializer
composes the portal site row's hostname from it — all through
`component/frontdoor`. Two copies of that rule would be two copies that can
disagree, and the disagreement is an issuer nothing is served at, or a
certificate naming a host the site row does not carry — which fails as
"sign-in is broken" with every manifest looking correct.

What differs between the cloud and local is which of the interchangeable parts
above is in play — plus that the local overlay's front door is hand-authored
while the cloud one is generated, which is a source question rather than a shape
one: the same six rules either way, gated against the same derivation.

The standard those rows answer to, and the reason the divergences are confined
to annotations and secret sources, is
[environment-parity.md](environment-parity.md). This page does not restate it.

## Port 5432 is debug-only

The local cluster maps Postgres on `5432` and that is **not a connection
path**. It is there so an operator can attach `psql` to a local database. No
client reaches MemQL that way anywhere, nothing in the connection
model depends on it, and a proposal that routes application traffic through it
is the port-forward-as-architecture anti-pattern
[environment-parity.md](environment-parity.md) rejects.

## Verifying a front door

Requires a running cluster, so it belongs on an operator machine or in CI.

```bash
kubectl -n memql get ingress -o wide             # the Ingress objects and their hosts
scripts/install/verify-frontdoor.sh              # dns + tls + precedence per host, h2 once
curl -sS --http1.1 https://api.memql.localhost/healthz
```

`verify-frontdoor.sh` runs **seven** checks against the default host set, not
three per host: DNS, TLS and precedence for each of `api.` and `identity.`, plus
**one** gRPC/h2 check, against the first host only. `precedence` is the one that
CAN come back neither passed nor failed — an `inconclusive` check counts in
neither tally, so it leaves `allPassed` alone
([above](#exact-versus-wildcard-precedence-is-declared-not-inherited)) — but
that is a property of the check, not a permanent state of this cluster: with
`svc/edge` deployed (memql#3714) it is genuinely testable and reports
`passed`, measured live:

```
allPassed: true, passedCount: 7, failedCount: 0, inconclusiveCount: 0
PASS dns  api.memql.localhost: resolves to 127.0.0.1
PASS tls  api.memql.localhost: TLS handshake ok against a trusted certificate (HTTP 415)
PASS dns  identity.memql.localhost: resolves to 127.0.0.1
PASS tls  identity.memql.localhost: TLS handshake ok against a trusted certificate (HTTP 302)
PASS grpc api.memql.localhost: negotiated HTTP/2 (HTTP 415) -- gRPC can ride this door
PASS precedence api.memql.localhost: /healthz answered by nodeType=bff, not by the
  wildcard's nodeType=edge -- the exact host rule takes precedence
PASS precedence identity.memql.localhost: /healthz answered by nodeType=identity, not
  by the wildcard's nodeType=edge -- the exact host rule takes precedence
```

Before `svc/edge` existed, the same command reported
`allPassed: true, passedCount: 5, failedCount: 0, inconclusiveCount: 2` — both
`precedence` checks `INCONCLUSIVE` ("the wildcard router is not loaded, so
there is nothing for an exact host to take precedence over"), because with no
backend Service for the wildcard rule, traefik dropped that router entirely.
That is history, not the current state of this front door; it is recorded
[above](#exact-versus-wildcard-precedence-is-declared-not-inherited) as the
measurement that motivated declaring the priority explicitly rather than
trusting it to still be true once a competing router existed.

Two things in that output are deliberate and neither is obvious.

**The status code is informational, not a pass criterion.** Note the `PASS`
beside `HTTP 415`. The script measures *transport reachability* — does the name
resolve to the expected address, does TLS handshake against a trusted
certificate, does the door negotiate h2 — not application health. A 4xx passes
because a door that answers 415 is a door that is up. (That 415 is the same one
described above: the probe hits `/`, which is the gRPC backend.)

**The probe set is smaller than the hosts block, on purpose.** `DEFAULT_HOSTS`
is `api.` plus `identity.` only, while the managed block
`scripts/install/hosts-entries.sh` writes carries five names — the same five
exact hosts the cloud certificate names. Those are different jobs: `/etc/hosts`
has no wildcard semantics so it needs every name spelled out, whereas the probe
set is only the doors we assert are up.

> **WARNING: an h2 probe passes while an HTTP/1.1 path is broken.** That is
> exactly how `/unsubscribe` stayed unrouted without anyone noticing — and note
> the h2 check runs once, so it is not even asserting h2 on every host. Extending
> the script with a plain HTTP/1.1 probe of one bff HTTP path is tracked on epic
> memql#3700; until it lands, the `curl` line above is that check done by hand.
> `--http1.1` is not decoration — curl prefers HTTP/2 over TLS by default, and
> an HTTP/2 request is not what a mail client or a webhook sender will send.

## Reference

- Design: `docs/superpowers/specs/2026-08-13-cluster-front-door-design.md`
  (epic [memql#3700](https://github.com/znasllc-io/memql/issues/3700)) —
  decisions D3 (the five rules as designed; memql#4224 added the portal's
  exact rule and retired D2's wildcard SAN in the cloud), D5 (a site is data)
  and D9 (the same-origin proxy for hosted sites), each with the alternatives
  that were rejected.
- The Service split and its annotations:
  `deploy/k8s/components/engine-bff/bff.yaml`.
- The path declarations the block is generated from:
  `component/server/unauthenticated_surface.go`.
