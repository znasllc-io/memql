# The Library -- artifact files, deployables, import and export

**Date:** 2026-08-22
**Status:** approved in the 2026-08-22 backlog brainstorm (sub-project F of nine, plus the tenth it spawned)
**Owner:** `dsl/library`, `integrations/library`, `component/server` (two HTTP routes), `component/edge`, `dsl/platform`, `deploy/k8s`, `clients/portal`

Sub-project F of the 2026-08-22 backlog brief. Extends epic memql#4288 (the
Artifacts page and labels, shipped in #4297 / #4299), which scoped out
uploading bytes; this design adds the bytes, the search, the deployables,
and the cloud TLS that makes self-serve hosting real. A tenth sub-project,
**J -- Commerce memory for Shopify** (order, line and inventory mirroring by
webhook with Admin-API reconciliation), was split out during this design and
gets its own record; the storefront kind below is the front of that mirror.

---

## 1. What exists, and what the brief asks for

At `948de3de`:

- **The index.** `v1:library:artifact` (`dsl/library/concepts.memql:40-105`) is
  a locked pointer row (memql#693): `kind`, `source`, `lens`, `format`,
  `mimeType`, `labels`, provenance back-pointers, `sourceConceptRef` as the
  idempotency key -- and **no bytes, no size, no hash, no delete, no declared
  authz tier** (grandfathered in `rowauthz_undeclared_gate_test.go:667-674`).
  Five `index*OnCreate` automations promote generated outputs, notes, todos,
  calendar events and memories into it.
- **The page.** `/artifacts` (`clients/portal/src/artifacts/`, #4299) lists
  the index with label filtering and a create that mints a `generatedOutput`.
  It sits in the rail's **Cluster** group beside Sites.
- **Bytes.** Only `v1:common:attachment.blobUrl`, written by
  `POST /spaces/{id}/attachments` (`component/server/attachment_handler.go`:
  25 MB, an 18-entry MIME allowlist, gated on owning the space) through
  `integration.storage.upload` (Azure Blob; Azurite locally). Download is the
  sibling `GET`, bytes through the bff with ownership re-checked
  (`:511-579`). No signed URLs exist. `v1:cognition:space` is not declared in
  the engine tree (pack-owned), so "a library space per user" is not a
  promise the engine can make.
- **Search.** Nothing embeds artifacts. `similarTo` is concept-agnostic
  (`integrations/similarity/capabilities.go:105`); `knowledge.ingest` chunks
  and embeds text into a knowledge domain (`integrations/knowledge/capabilities.go:93`),
  and the upload pipeline never calls it. `v1:knowledge:document` and
  `v1:common:knowledgeDomain` are not declared in the engine tree either.
- **Sites.** `v1:platform:site` (`dsl/platform/concepts.memql:180-209`):
  `@rowAuthz(clusterOwner)` with the reason stated ("list every site in this
  cluster" must stay expressible), `kind enum("spa","static")`, `bundleRef`
  in three forms, the content-addressed `edge.Publisher` (`component/edge/publish.go`),
  and `POST /sites/{id}/bundles` for **service accounts only** -- the portal
  publishes by flipping `bundleRef` and never uploads. A new kind is
  documented as "a value and its resolution tail" (`:196`).
- **TLS.** The cloud front-door certificate names exact hosts only
  (HTTP-01; memql#4224). The `*.<domain>` Ingress rule routes every site to
  the edge, but a new site hostname has no certificate until an operator adds
  one. Locally, mkcert's wildcard covers everything.
- **Shopify.** `integrations/shopify` is a three-field read-only product
  index over the Storefront API; the inbound receiver already verifies
  Shopify webhooks (HMAC + dedupe headers) and applies `products/*`.

The brief: an **Artifacts** section for files of any type -- uploaded,
generated, created or edited -- that the graph indexes for semantic
similarity; a **Deployables** section absorbing Sites, typed SPA / Shopify
Storefront / Website now and Android / iOS / macOS "coming soon"; **import**
for training and **export** (download).

---

## 2. Decisions

### D1 -- A first-class library file behind the index, with its own HTTP pair

Chosen over riding the attachment routes in a per-user library space (zero
new HTTP, but the library would rest on a pack-owned concept the engine does
not declare, inherit the 25 MB cap and the MIME allowlist, and need a space
the engine would have to invent) and over chunked upload on the gRPC stream
(re-litigates the documented exception; every CI tool would need a MemQL
client). `POST /artifacts` and `GET /artifacts/{id}/content` are inside the
exception CLAUDE.md already records for multipart ("file uploads map poorly
to gRPC"), and **the owner approved them explicitly in the brainstorm**, the
way #3713 approved the bundle route. The index stays a pointer; the file is
the sixth backing row.

### D2 -- Similarity lives in the library namespace

Chunks are `v1:library:fileChunk`, embedded through `integration.embedding.store`,
searched through `similarTo`. Not `v1:knowledge:documentChunk`: that concept
requires a knowledge domain, and the domain concept is not declared in the
engine tree. Training into a knowledge domain is a separate, deliberate act
(D7).

### D3 -- Deployables are self-serve per user, and the cloud gets DNS-01

Chosen over a cluster-owner-only surface (Sites renamed; every new site an
operator ticket) and over self-serve locally but admin-only in the cloud
(splits behaviour by target, which the parity rule forbids). `site` gains an
owner and the composite tier from sub-project B; a user deploys to
`<slug>.<domain>` under the wildcard rule; a cert-manager DNS-01 issuer and
one wildcard certificate make a freshly deployed site live over TLS with no
operator step. The stated reason for `clusterOwner` -- listing every site --
stays expressible, because the composite tier gives cluster owners every row.

### D4 -- The Shopify storefront is a site MemQL hosts

Chosen over theme push to the merchant's store (an Admin-API integration in
which MemQL hosts nothing) and over both-with-one-deferred. A
`shopify_storefront` site is a SPA bundle served by the edge at
`<slug>.<domain>` with a typed binding to a store; checkout is Shopify's
hosted checkout. Sales, inventory and refunds reach MemQL through webhooks
and reconciliation -- sub-project J -- not through the storefront, so a sale
from any channel lands. The storefront is the front of that mirror.

### D5 -- Mobile and desktop apps are not sites

Android, iOS and macOS are artifact *distribution* (stores, TestFlight,
notarisation), not hostname-resolved web surfaces. They appear in the portal
as disabled "coming soon" kinds and get **no schema** until they are
designed; adding enum values that never resolve would be the wrong kind of
additive.

### D6 -- Deploy from the portal is server-side, from bytes the system holds

`sitePublishFromArtifact(siteId, artifactId)` reads a bundle artifact's blob
and runs `edge.Publisher.Publish`. No user-facing bundle upload route is
added; `POST /sites/{id}/bundles` stays the CI door. Atomicity is the
Publisher's: a new content-addressed version, then the row flip.

### D7 -- Upload and train are two acts

A file in the Library is yours the moment it lands; training it into a
knowledge domain (`libraryTrainFile`) is a decision recorded on the file.
Nothing is trained by default.

### D8 -- The index declares its tier

`v1:library:artifact` gets `@rowAuthz(owner="ownerUserId", clusterOwner)`
(B's composite), closing the grandfathered gap the research flagged, at the
moment the page becomes a primary surface. `file`, `fileChunk` and `site`
declare the same form.

### D9 -- Export is the content route, for every artifact

`GET /artifacts/{id}/content` streams a file artifact's bytes and, for a
note, a generated output or a memory, renders the backing row's body as a
download with a derived filename. One route exports the whole Library.

---

## 3. The Library

### 3.1 `v1:library:file`

| Field | Type | Notes |
|---|---|---|
| `ownerUserId` | string! | stamped from `actor.userId` |
| `name` | string! | original filename, sanitised |
| `mimeType` | string! | as uploaded; sniffed when absent; **any type** |
| `size` | int! | bytes |
| `sha256` | string! | hex; dedup hint, never an access key |
| `blobUrl` | string! | `library/{userId}/{fileId}/{name}` through `integration.storage.upload` |
| `source` | enum(`uploaded`, `exported`, `agent_generated`, `derived`)! | |
| `status` | enum(`stored`, `analyzing`, `ready`, `failed`)! | |
| `summary` | string | from the analysis pass, known types only |
| `embeddingStatus` | enum(`none`, `partial`, `complete`) | |
| `trainedIntoDomainIds` | []string | D7 |
| `archived` | bool | soft delete |

`@rowAuthz(owner="ownerUserId", clusterOwner)`. Automation `indexFileOnCreate`
promotes it: artifact `kind: file` (new enum value), `source` carried over,
`format` derived from the MIME, `sourceConceptRef` the idempotency key.

### 3.2 `v1:library:fileChunk`

`fileId!`, `artifactId!`, `ownerUserId!`, `seq!`, `text!`, `tokenCount`,
embedded at analysis time through `integration.embedding.store` (the same
call `dsl/harness` and `dsl/authoring` use), same tier. Chunking reuses the
knowledge splitter's defaults (about 1800 characters, 180 overlap).
`librarySimilarArtifacts(text | artifactId, limit)` runs `similarTo` over
the chunks and folds to artifacts by best score; `artifactSearch` is its
agent tool.

### 3.3 The artifact index

`kind` gains `file`; a soft `archived bool` and `archiveArtifact` (the first
write that removes something from the Library; the backing file is archived
with it); the composite tier (D8). Everything #4288 shipped stays.

### 3.4 Upload -- `POST /artifacts`

Multipart, field `file`, optional `name` and `labels` (comma-separated, the
same `[]string` the label builtins write). Bearer-authenticated as the user;
the actor owns the result. Streamed to blob storage under a size cap
`MEMQL_LIBRARY_MAX_UPLOAD_BYTES` (default 256 MB, so a site bundle fits;
multipart memory 32 MB as today). Any MIME; unknown types are stored
opaquely with `status: ready` and no chunks. 201 `{artifactId, fileId}`.
Then, detached, the analysis pass the attachment handler already runs
(`integration.files.extractText` for the known types, a summary), chunking
and embedding, and `status: ready` -- or `failed` with the reason on the
row, never a silent partial.

Declared through `server.ArtifactPaths()` (prefix `/artifacts/`) so
`cmd/frontdoorpaths` routes it (`make frontdoor-paths`); it is an
**authenticated** route and therefore appears in none of `PublicPaths()`,
`HandlerAuthorizedPaths()` or `SelfAuthenticatedPaths()`, exactly the class
the generator exists to catch. Mounted on the bff (`app/transport_*.go`).

### 3.5 Download and export -- `GET /artifacts/{id}/content`

Resolves the artifact under the caller's actor (`rowAuthzAdmits` on the
index row and on the backing row; a deny is 404, the attachment precedent).
File artifact: bytes through the bff with `Content-Type`, `Content-Length`,
`Content-Disposition: attachment; filename="..."`. Note / generated output
/ memory: the body as `text/markdown` or `text/plain` with a filename
derived from the title. Never a redirect to storage; there are no signed
URLs and this design does not add any.

### 3.6 Train -- `libraryTrainFile(fileId, domainId)`

A builtin (and agent tool `artifactTrain`) that runs `integration.knowledge.ingest`
over the file's extracted text into the named domain with
`sourceRef: artifact:<artifactId>`, appends the domain to
`trainedIntoDomainIds`, and audits it. The domain must be one the caller may
write to; the product's knowledge concepts stay where they are.

### 3.7 The portal

A new rail group **Library** holding Artifacts and Deployables (the Cluster
group keeps Integrations and the admin entries; `/sites` redirects to
`/deployables`, the retired-section precedent at `AppShell.tsx:113-127`).
Artifacts gains: upload (a drop zone and a picker; progress; the 201 lands
the row in the list), download / export on every row and on the detail
page, "search by meaning" (a query box over `librarySimilarArtifacts`,
results ranked, the label filter composing with it), "train into..." (a
domain picker over the domains the caller may write to), and archive behind
`ConfirmDialog`. All in the bespoke feature directory (`src/artifacts/`),
outside the predefined-view guard by the precedent #4288 set.

---

## 4. Deployables

### 4.1 `v1:platform:site`, extended

| Field | Change |
|---|---|
| `ownerUserId` | new, string; empty = cluster-owned (the portal row stays empty) |
| `artifactId` | new, string; the bundle artifact last published from |
| `kind` | enum gains `shopify_storefront` |
| `binding` | new, object: for `shopify_storefront`, `{storeDomain, storefrontTokenRef}` where the ref names a `globalSecret` |
| tier | `@rowAuthz(owner="ownerUserId", clusterOwner)` |

`createSite` stamps `ownerUserId` from the actor; a cluster owner may clear
it. Android / iOS / macOS: **no schema** (D5).

### 4.2 Hostname policy

A user-created site is `<slug>.<domain>`: slug `[a-z0-9-]{3,40}`, unique
across sites, not in the reserved list (`api`, `identity`, `mcp`, `portal`,
`www`, `admin`, `mail`, and the apex), derived from the domain the cluster
serves (`frontdoor`, the same derivation the portal host uses). Any other
hostname -- a custom apex, another domain -- stays cluster-owner-only and
hand-certified, as today. Enforced in the mutation and in the Go guard that
already refuses `systemOwned` deletes.

### 4.3 Deploy from the Library -- `sitePublishFromArtifact(siteId, artifactId)`

A builtin, executed by the bff under the caller's actor: the caller must own
the site (or be a cluster owner) and the artifact; the artifact must be a
file with a zip MIME; the bundle is read from blob storage, validated
(`index.html` at the root for `spa` and `shopify_storefront`; the same
per-file, total and count limits the CI route enforces, `site_bundle_handler.go:20-45`),
and handed to `edge.Publisher.Publish`, which writes the content-addressed
version and flips `bundleRef`; `artifactId` is stamped on the row. Rollback,
enable / disable and delete are unchanged (`useSiteDetail.ts`).

### 4.4 The storefront at request time

For a `shopify_storefront` site, the edge injects the binding into the site's
runtime-config document -- the mechanism that already gives the portal its
own config (`component/edge/runtimeconfig.go`) -- as
`{kind: "shopify_storefront", storeDomain, storefrontToken}`, resolving the
token from the named `globalSecret` at serve time. A Storefront API token is
a client-side credential by Shopify's design; the Admin token never reaches
a browser. Checkout is Shopify's hosted checkout. Orders and inventory are
sub-project J's concern; this design stores nothing about them.

### 4.5 DNS-01 in the cloud

`deploy/k8s/overlays/cloud` (and `cloud-entry`): a cert-manager
`ClusterIssuer` with the Azure DNS solver using a workload identity on the
pattern ESO already uses (`deploy/external-secrets/`), and one Certificate
for `*.<domain>` + the apex. The edge Ingress's wildcard rule gains its TLS
host; the role hosts keep their exact entries. `frontdoor_hosts_test.go`'s
exact-host assertion (memql#4224) becomes conditional: exact hosts when the
issuer is HTTP-01, the wildcard allowed when a DNS-01 issuer is declared;
`docs/public/operate/front-door.md` records the reversal and why it is safe
now (DNS-01 can issue a wildcard; HTTP-01 cannot). A zone for `<domain>` in
Azure DNS and the identity's `DNS Zone Contributor` role are install-time
prerequisites the runbook lists. Local is unchanged.

### 4.6 The portal

`/deployables` (feature directory `src/deployables/`, replacing `src/sites/`):
a list of the caller's deployables (cluster owners see all, with an owner
column), create with a kind picker -- SPA, Website (`static`), Shopify
storefront live; Android, iOS, macOS disabled with "coming soon" -- the slug
field with live validation against the policy, the storefront's binding
fields, "deploy from the Library" picking a zip artifact, and the detail page
with hostname, live URL, version history and rollback, enable / disable,
delete. The Sites page's existing behaviours move over unchanged.

---

## 5. Security posture

| Concern | Handling |
|---|---|
| Upload by anyone with a bearer | the actor owns what they upload; cap; streamed; unknown MIME stored opaquely, never executed; analysis runs the same extractor the attachment path trusts |
| Download across users | `rowAuthzAdmits` on index and backing row per request; deny is 404 |
| Bundle content | validated before publish; served by the edge under the per-site CSP as today; a zip bomb is bounded by the existing per-file / total / count limits |
| Hostname squatting | reserved list, uniqueness, derived domain only; custom hosts admin-only |
| Storefront token in the browser | Storefront API tokens are client-side by design; the Admin token is never injected |
| Wildcard certificate | one key for every site host; the edge already serves every host from one process, so the blast radius is unchanged; rotation is cert-manager's |
| Agent tools | `artifactSearch` reads under the agent's user; `artifactTrain` writes only to domains the user may write to |

---

## 6. Testing

1. Handlers: upload of three MIME types including an unknown one; the cap;
   a second user's download is 404; a note exports as markdown; the front-door
   path gate (`TestFrontDoorPathsAreNotStale`, `TestEveryServerPathDeclarationIsClassified`)
   passes after regeneration.
2. Promotion and analysis: a file becomes an artifact with `kind: file`; a
   text fixture yields chunks with embeddings; `librarySimilarArtifacts`
   returns the right artifact first; `failed` carries a reason.
3. Train: chunks land in the chosen domain with the artifact `sourceRef`;
   the file records the domain; a domain the caller may not write to is
   refused.
4. Sites: a user sees their own, a cluster owner sees all; slug validation
   and the reserved list; `sitePublishFromArtifact` on a fixture zip ends with
   the edge serving `index.html` at the new version and rollback restoring
   the old; a non-zip artifact is refused.
5. Storefront: the runtime-config document carries the binding for a
   `shopify_storefront` site and nothing for a `spa`.
6. Manifests: both cloud overlays build with the DNS-01 issuer and the
   wildcard Certificate; the local overlay is unchanged; the front-door host
   gate accepts the wildcard only with the issuer present.
7. Portal: upload, search, train, archive on Artifacts; create with each
   kind, the disabled kinds, deploy-from-Library, rollback on Deployables;
   `/sites` redirects; the repo-root guards pass.

---

## 7. Delivery

| PR | Contains | Depends on |
|---|---|---|
| 1 -- the Library | `file`, `fileChunk`, the index changes, the two routes, analysis + embedding + similarity, train, tools, the Artifacts page additions, the Library nav group | B's composite tier (#4312) |
| 2 -- Deployables | site fields + tier + policy, `sitePublishFromArtifact`, storefront runtime config, the Deployables page, the `/sites` redirect | PR 1 (bundle artifacts), B's tier |
| 3 -- cloud TLS | DNS-01 issuer, wildcard Certificate, the host-gate reversal, the front-door doc and runbook | nothing |

One `Closes #N` line per issue.

---

## 8. Out of scope

- Sub-project J (orders, inventory, refunds by webhook; Admin-API push of
  decisions) -- its own record.
- Theme push to a merchant's Shopify store (rejected in D4).
- Schema for Android / iOS / macOS (D5).
- Signed storage URLs; resumable or multi-file upload; versioning of files
  (a re-upload is a new file).
- Promoting chat attachments into the Library (a later automation over
  `v1:common:attachment`; nothing here prevents it).
- Anonymous storefront behaviour events (needs an ingestion path visitors can
  use; J or later).
- Custom hostnames and per-site certificates for users (admin-only stays).

---

## 9. References

- Code: `dsl/library/*.memql`, `integrations/library/library.go`,
  `component/server/{attachment_handler,site_bundle_handler,nethttp,unauthenticated_surface}.go`,
  `cmd/frontdoorpaths`, `component/edge/{publish,blob,runtimeconfig,csp}.go`,
  `dsl/platform/{concepts,mutations,seeds}.memql`, `integrations/{azureblob,fileprocessor,embedding,similarity,knowledge,shopify}`,
  `deploy/k8s/overlays/{cloud,cloud-entry,local}`, `deploy/external-secrets/`,
  `clients/portal/src/{artifacts,sites}/`, `clients/portal/src/app/AppShell.tsx`.
- Specs: `2026-08-22-artifacts-page-and-labels-design.md` (epic #4288),
  `2026-08-22-subscription-row-authz-design.md` (epic #4308, the composite tier).
- Docs: `docs/public/operate/front-door.md`, `docs/public/operate/inbound-delivery.md`,
  `docs/public/concepts/component-integration-pack.md`, CLAUDE.md "Allowed HTTP
  Exceptions".
