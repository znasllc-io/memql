---
title: Azure entry install -- sanctioned first bring-up on AKS
audience: public
status: stable
area: operate
sinceVersion: 0.18.0
owner: platform
---

# Azure entry install: sanctioned first bring-up

**Audience:** an operator standing up an entry-shape MemQL instance on
Azure. **Issue:** memql#4204.

**Placeholders.** Every Azure name below is the operator's, and this page
names none of them -- the same rule the hostnames already follow with
`$MEMQL_DOMAIN`. Substitute your own throughout: `<aks-name>` (the
cluster), `<cluster-rg>` (its resource group), `<backup-rg>` (the backup
resource group), `<registry-rg>` (the resource group holding the ACR and
Key Vault), `<instance-repo>` (the private repository that will hold this
instance's definition), and `<instance>` (the overlay directory inside
it).

First bring-up is **Argo in `<aks-name>` reconciling
`deploy/k8s/overlays/cloud-entry`**. It is not `make deploy`. It is not
`aks-deploy.sh` (that script is gone). Those are digest rolls against an
already-installed cluster. This page is the sanctioned entry-install
path.

Related: [front-door.md](front-door.md) ·
[reproduce-the-cloud-locally.md](reproduce-the-cloud-locally.md) ·
[deploy-bundle-runbook.md](deploy-bundle-runbook.md) ·
[environment-parity.md](environment-parity.md)

## What this instance is

MemQL ships one installation shape. A second environment is a second
instance -- own AKS, own Argo, own domain -- not a staging/prod split
inside one cluster (epic memql#3943). `overlays/cloud` stays on
`cnpg-db/presets/top` and mesh 2; it is what
`deploy/argocd/apps/memql.yaml` points at. **Do not apply
`overlays/cloud` to `<cluster-rg>`.** Do not add a second Application
next to `deploy/argocd/apps/memql.yaml`.

`cloud-entry` is the same base and the same CNPG shape, with the
numbers an entry / client install actually wants:

| | `overlays/cloud` | `overlays/cloud-entry` |
|---|---|---|
| Database | `cnpg-db/presets/top` | `cnpg-db/presets/entry` (1 instance, 32+16 GiB) |
| Mesh replicas | 2 | 1 |
| Voice / LiveKit / MCP | running pins + fail-closed placeholders | replicas 0 (voice-off) |

Voice-off is first-class replicas 0 on `voice`, `voice-agent`,
`livekit`, `livekit-sip`, `mcp`, and `livekit-redis`. It is not
`NODE_IP=0.0.0.0` and not an all-zeros MCP digest.

### The voice-off hold on the LiveKit Services

Replicas 0 stops the pods; it does not stop Azure allocating a public IP
for every `LoadBalancer` Service base declares -- `livekit-rtc` (media)
and `livekit-sip` (SIP) -- so the first entry install converted both to
`ClusterIP` by hand, and the next Argo sync was refused:

```
Service "livekit-rtc" is invalid: spec.externalTrafficPolicy: Invalid value: "Local": may only be set for externally-accessible services
```

The overlay still said `LoadBalancer` + `externalTrafficPolicy: Local`,
and a `ClusterIP` Service cannot carry that field (nor
`loadBalancerSourceRanges`). The pins applied by digest, but the
Application stayed OutOfSync/Failed.

Since memql#4225 the hold is part of `overlays/cloud-entry` itself: JSON
6902 patches on `livekit-rtc` and `livekit-sip` set `type: ClusterIP` and
remove `externalTrafficPolicy`, `loadBalancerSourceRanges` and the Azure
mixed-protocol annotation, beside the replica-0 patches. `livekit`
(signaling) is `ClusterIP` in base already. Nothing needs ignoring in the
Application and nothing needs hand-editing on the cluster; a hand-edited
Service is simply overwritten with the same values on the next sync.
Gated by `deploy/k8s/overlays/livekit_entry_voice_off_test.go` (text-level,
cannot skip) and `render_cloud_entry_test.go` (rendered). Verify:

```bash
kubectl kustomize deploy/k8s/overlays/cloud-entry | grep -A12 'name: livekit-rtc' | grep -E 'type:|externalTrafficPolicy'
kubectl -n memql get svc livekit livekit-rtc livekit-sip   # TYPE ClusterIP, EXTERNAL-IP <none>
```

Turning voice on for an entry install is a different decision and not
this overlay: `overlays/cloud` keeps the LoadBalancers and is untouched.

## Domain and hosts

The committed default in the overlay is `memql.localhost`. No file
under `deploy/` names a real domain (memql#3593). An install patches
`MEMQL_DOMAIN` and the Ingress / certificate hosts through the ArgoCD
Application's `spec.source.kustomize.patches`. `.localhost` is
unroutable externally, so a cluster reconciled before its domain is
set fails visibly instead of half-working.

The company apex site is not the cluster domain. **Do not point the
company apex at AKS.** The cluster domain is the install's
`MEMQL_DOMAIN` (the value lives on the install, not in this repo):

| Role | Host | Notes |
|---|---|---|
| identity | `identity.$MEMQL_DOMAIN` | sign-in; first-run wizard at `/setup` |
| api | `api.$MEMQL_DOMAIN` | gRPC + HTTP edge |
| sites (portal) | `portal.$MEMQL_DOMAIN` | site #1; its own exact rule and SAN (memql#4224) |
| sites (wildcard) | `*.$MEMQL_DOMAIN` | every other site; routing only, **no certificate behind it** |
| sites (apex) | `$MEMQL_DOMAIN` | same edge Service |
| mcp | `mcp.$MEMQL_DOMAIN` | exists in the generator; **leave it dark** |

The MCP host is part of the closed role set. Voice-off already sets
`mcp` replicas to 0. Do not publish it, do not send clients there.

## DNS and certificates

Point DNS at the cluster ingress, then wait for the certificate:

| Name | Type | Value |
|---|---|---|
| `$MEMQL_DOMAIN` | A | cluster ingress IP |
| `*.$MEMQL_DOMAIN` | A | same cluster ingress IP |

The wildcard DNS record is for ROUTING: it is what lets `*.$MEMQL_DOMAIN`
reach the edge for every site. It is not a wildcard certificate.

**The front-door certificate (`memql-front-door-tls`) names exact hosts
only** (memql#4224): `api.`, `identity.`, `mcp.`, `portal.` and the apex.
The issuer is `letsencrypt-prod`, which solves **HTTP-01 only** -- there is
no DNS-01 solver on this cluster. That matters in two ways:

- **Do not add a `*.$MEMQL_DOMAIN` dnsName.** ACME cannot serve an HTTP-01
  challenge for a wildcard, and one wildcard name fails the WHOLE order:
  the Certificate sits Pending, the Secret is never written, and every
  host serves ingress-nginx's self-signed default ("Kubernetes Ingress
  Controller Fake Certificate"; Safari: "This Connection Is Not Private").
  That is how the first entry-shape bring-up went, and the hand-edit to
  exact names that followed is now the generated shape.
- **`tls.hosts` must say the same names.** ingress-nginx verifies the
  certificate against each host an Ingress lists under `tls` and falls
  back to the default for a host the certificate does not name -- and it
  builds a certificate-bearing server block per RULE host, never per
  `tls` host. An edge Ingress that still lists `*.$MEMQL_DOMAIN` under
  `tls`, or a portal with no exact rule of its own, reproduces the
  fake-certificate failure for `portal.$MEMQL_DOMAIN` with a Ready
  certificate in hand. The generator emits `portal-front-door` (an exact
  rule to the same edge Service) for exactly this reason; the
  cluster-side `portal-front-door` Ingress that was hand-made as the ops
  workaround has the same name, so Argo adopts and overwrites it on the
  next sync.

A customer site hostname routed by the wildcard has **no certificate**
until it has a `Certificate` and an exact-host Ingress of its own
([site-hosting.md](site-hosting.md#2-add-the-hostname)). An engine-only
entry install hosts no such site, so nothing is missing; a client install
that hosts a site on the
cloud front door must plan for that object pair per site until a DNS-01
solver exists.

## Argo host-patch

`cmd/frontdoorhosts` writes `overlays/cloud-entry/front-door.generated.yaml`
with the committed default. Install-time patches repoint every host
the generator emitted, plus the `memql-domain` ConfigMap:

- Certificate `memql-front-door-tls` `spec.dnsNames` → `api.$MEMQL_DOMAIN`,
  `identity.$MEMQL_DOMAIN`, `mcp.$MEMQL_DOMAIN`, `portal.$MEMQL_DOMAIN`,
  `$MEMQL_DOMAIN` -- five entries in that order (`/spec/dnsNames/0` ..
  `/spec/dnsNames/4`), never a wildcard
- Ingress `api-front-door` and `api-front-door-grpc` → `api.$MEMQL_DOMAIN`
  (`/spec/rules/0/host` and `/spec/tls/0/hosts/0`)
- Ingress `identity-front-door` → `identity.$MEMQL_DOMAIN` (same two pointers)
- Ingress `mcp-front-door` → `mcp.$MEMQL_DOMAIN` (same two pointers; dark,
  still patch the name so it cannot drift)
- Ingress `portal-front-door` → `portal.$MEMQL_DOMAIN` (same two pointers)
- Ingress `edge-front-door` → `/spec/rules/0/host` = `*.$MEMQL_DOMAIN`,
  `/spec/rules/1/host` = `$MEMQL_DOMAIN`, `/spec/tls/0/hosts/0` =
  `$MEMQL_DOMAIN` -- the apex is its ONLY tls host; there is no
  `/spec/tls/0/hosts/1` any more
- ConfigMap `memql-domain` key `MEMQL_DOMAIN` → the install domain

> **WARNING: the pointer set changed in memql#4224.** A patch written
> against the previous shape -- two dnsNames, a wildcard under the edge
> Ingress's `tls.hosts` -- does not fail loudly everywhere: the two
> dnsNames replaces still succeed and put the wildcard back into the
> order, which is the Pending Certificate again. (`/spec/tls/0/hosts/1` on
> `edge-front-door` does fail the render now, which is the one place a
> stale patch announces itself.) Rewrite the Application patch to the list
> above before syncing.

The same derivation feeds every side: `cmd/frontdoorhosts` composes
the Ingress hosts and SANs, `component/envregistry/domain.go` composes
the issuer, CORS origins and redirect URIs, and the engine's
SeedMaterializer composes the portal site row's hostname from
`MEMQL_DOMAIN` -- all through `component/frontdoor`. Two copies of that
rule would be two copies that can disagree.

`make frontdoor` writes both instance overlays. Do not copy
`overlays/cloud/front-door.generated.yaml` into `cloud-entry` --
kustomize refuses a sibling path (load restrictor).

## First bring-up

1. Cluster `<aks-name>` in `<cluster-rg>` (AKS Free, entry node
   pools). Backups go to `<backup-rg>`; the ACR and Key Vault in
   `<registry-rg>` stay where they are.
2. Point that cluster's Argo at `znasllc-io/memql`, path
   `deploy/k8s/overlays/cloud-entry`, and the host-patches above.
   Do not retarget `deploy/argocd/apps/memql.yaml`.
3. Seed secrets the way a cloud install already does (External Secrets
   + Key Vault). Seed `memql-domain` with the install's `MEMQL_DOMAIN`.
4. Wait for DNS, then wait for the certificate to become Ready:
   `kubectl -n memql get certificate memql-front-door-tls` must show
   `READY True` with the five exact dnsNames and no wildcard. Then prove
   the SNI match memql#4224 was about, on every exact host:

```bash
for h in identity api portal mcp; do
  openssl s_client -servername "$h.$MEMQL_DOMAIN" -connect "$h.$MEMQL_DOMAIN:443" </dev/null 2>/dev/null \
    | openssl x509 -noout -issuer -subject
done
```

   Every line must name Let's Encrypt as the issuer. `Kubernetes Ingress
   Controller Fake Certificate` on any host means that host has no exact
   rule or is not in the certificate -- re-check the patch list above.
5. Open `https://identity.$MEMQL_DOMAIN/setup`. The domain field
   prefills from `MEMQL_DOMAIN` (memql#4216).
6. Verify:

```bash
scripts/install/verify-frontdoor.sh --hosts api.$MEMQL_DOMAIN,identity.$MEMQL_DOMAIN
```

That script establishes dns / tls / grpc / precedence for `api.` and
`identity.` only. `mcp.` is inconclusive by design (it does not serve
`/healthz` on the front-door port). Portal and the apex are served by
the edge by design -- the portal's exact rule and the apex rule point at
the same Service the wildcard does -- so there is no exact-host
precedence to prove for them.

## Later rolls (not this bring-up)

Cockpit digest rolls stay owner-gated and still default to
`overlayPath=deploy/k8s/overlays/cloud`. An entry instance must pass
`cloud-entry` explicitly:

```bash
memql-cockpit deploy --ref main --role owner --actor "$USER" \
  --input '{"provider":"azure","overlayPath":"deploy/k8s/overlays/cloud-entry"}'
```

Live `--apply` stays owner-gated. See
[deploy-bundle-runbook.md](deploy-bundle-runbook.md). No
`overlays/cloud`. No `top`. No extra monitoring addon.

## Mail: the Graph sender lives on the mailbox tenant

Magic links, invitations and admin notifications leave the identity
service through `integrations/email`'s Microsoft Graph sender, and on
this instance Graph is the ONLY path (memql#4218): no SMTP, no Azure
Communication Services.

The lesson from the first bring-up (memql#4226): AKS and the
Pay-As-You-Go subscription sit on one Entra tenant; the sender mailbox
(`noreply@<domain>`) and the public mail domain live on a DIFFERENT
Microsoft 365 tenant. Graph resolves `/users/<sender>` in the tenant the
token was issued for, so on the AKS tenant that user is a 404 (or a
guest object with no mailbox), whatever permissions the app holds there.
Everything the sender needs therefore lives on the MAILBOX tenant:

| What | Where | Value |
|---|---|---|
| App registration `id-memql-mail` + client secret | mailbox tenant | `MEMQL_EMAIL_AZURE_CLIENT_ID` / `MEMQL_EMAIL_AZURE_CLIENT_SECRET` |
| `Mail.Send` (Application) + admin consent | mailbox tenant | -- |
| Exchange `ApplicationAccessPolicy` scoping the app to `noreply@<domain>` | mailbox tenant | -- |
| Tenant id | mailbox tenant | `MEMQL_EMAIL_AZURE_TENANT_ID` -- NOT the AKS directory |
| Sender | -- | `MEMQL_EMAIL_SENDER=noreply@<domain>` |

Find the tenant from the domain, not from whichever portal you are
signed in to:

```bash
curl -s "https://login.microsoftonline.com/getuserrealm.srf?login=noreply@<domain>&json=1"
curl -s "https://login.microsoftonline.com/<domain>/.well-known/openid-configuration" | jq -r .issuer
# issuer = https://sts.windows.net/<tenant-id>/
```

### Next client install: mail checklist

- [ ] Identify the tenant that hosts the sender mailbox with the two
      commands above. Do not assume it is the subscription's tenant.
- [ ] On that tenant,
      `GET /v1.0/users/noreply@<domain>?$select=id,userType,assignedLicenses`
      returns a `Member` with `assignedLicenses` non-empty. Empty
      licenses, or `MailboxNotEnabledForRESTAPI` on the first send, means
      no Exchange mailbox -- license it or pick another sender before
      wiring anything.
- [ ] Create `id-memql-mail`, grant `Mail.Send` (Application) with admin
      consent, and apply the `ApplicationAccessPolicy` -- on the mailbox
      tenant only.
- [ ] Do NOT create the mail app on the AKS tenant. Do NOT recreate
      `noreply@<domain>` on the AKS directory: a same-named object on the
      wrong tenant sends every later diagnosis down the wrong path.
- [ ] Seed `MEMQL_EMAIL_AZURE_TENANT_ID` (the mailbox tenant id),
      `MEMQL_EMAIL_AZURE_CLIENT_ID`, `MEMQL_EMAIL_AZURE_CLIENT_SECRET` and
      `MEMQL_EMAIL_SENDER` through Key Vault + External Secrets like every
      other secret on the install.
- [ ] Send one magic link; the identity log shows `email: using Microsoft
      Graph sender` at boot and `sendMail` answering `202`. A `404` on
      `/users/<sender>` is the tenant; a `401` is the credential.

Full runbook:
[auth/identity-service.md](auth/identity-service.md#email-delivery).

## Handoff to an instance repo

An entry instance's definition is not in git today. This section is the operator's
record of where it lives, what the target shape is, and the exact switch --
from the read-only research pass on memql#4210 (the instance repo) and
memql#4205 (twice-daily entry deploys). Nothing below has been
executed: creating the repo, the ESO credential and vault, the AppProject and
repo-credential writes, and the Argo source switch are owner-gated.

### Today's source of truth

- **The hand-made ArgoCD Application `memql` in this instance's own Argo.**
  Source `https://github.com/znasllc-io/memql.git` at a pinned `main` SHA
  (rolled forward SHA by SHA, one engine pin PR at a time), path
  `deploy/k8s/overlays/cloud-entry`, manual sync. The engine repo
  deliberately carries no Application for this cluster:
  `deploy/argocd/apps/memql.yaml` points at `overlays/cloud`, which
  reconciles nothing live, and
  `TestCloudStaysOnTopAndTheInClusterAppIsUnchanged` fails the build if it
  is retargeted or a second Application appears.
- **Every install-time value lives ONLY in that Application's
  `spec.source.kustomize.patches`:** the host patches listed under
  [Argo host-patch](#argo-host-patch), the `memql-domain` ConfigMap's
  `MEMQL_DOMAIN`, the CNPG `serviceAccountTemplate` workload-identity
  client id (`REPLACE-WITH-DB-IDENTITY-CLIENT-ID` in the overlay), and the
  backup `ObjectStore` `destinationPath` (the storage account is an install
  value -- capture it, do not reconstruct it).
- **Out of band -- no engine manifest defines them:** the cluster-scoped
  `ClusterIssuer letsencrypt-prod` (HTTP-01), and the hand-seeded
  `memql-secrets` (the Graph mail credentials included). Until memql#4224
  and memql#4225 are deployed, two cluster-only workarounds sit beside
  them -- the `portal-front-door` Ingress and the LiveKit Services forced
  to `ClusterIP`; with those fixes in the engine tag the instance
  composes, both become generated / overlay content and the cluster
  copies are overwritten on sync.
- **Pins today are a human loop:** tag `vX.Y.Z` on `main`, dispatch
  `build-engine-images.yml` with `version=X.Y.Z` (it pushes the same image
  to ACR and to `ghcr.io/znasllc-io/memql-<node>:X.Y.Z`), edit
  `overlays/cloud-entry/kustomization.yaml`, land the PR through the merge
  queue, retarget the Application to the new `main` SHA, sync.
  `dispatch-engine-images-on-release.yml` fires only on a PUBLISHED GitHub
  Release, and tags are not published as releases, so it never fires today.

### The target shape

- **An instance repo that holds the cluster's definition, not the cluster's
  code** (`<instance-repo>`, private): an overlay, the pins, and an ArgoCD
  app-of-apps. It is NOT a product repo stamped from the `memql-project`
  template: the template stamps a product (DSL bundle, a client surface, a
  `bff-<product>` head, its own public entry and its own `letsencrypt-prod`
  ClusterIssuer), and its Makefile and CI fail by design with no
  `dsl/<domain>` and no `clients/<name>/`. An engine-only entry instance
  runs the plain engine plus the portal.
- **The mechanism exists and is verified** (kustomize v5.8.1, 54 documents
  rendered): compose the engine's entry overlay as a remote kustomize
  resource and override the install values on top.

```yaml
resources:
  - https://github.com/znasllc-io/memql//deploy/k8s/overlays/cloud-entry?ref=<engine tag>
  - memql-domain.yaml          # MEMQL_DOMAIN: $MEMQL_DOMAIN
  - cluster-issuer.yaml        # letsencrypt-prod, HTTP-01, captured from live
patches:                       # Certificate dnsNames + Ingress hosts (the Argo host-patch list, now in git),
                               # Cluster memql-db serviceAccountTemplate client-id,
                               # ObjectStore memql-db-backup destinationPath
images:                        # the eight engine digests (identity bff cognition voice agent planner workbench edge)
```

  kustomize shallow-clones the public engine repo at the ref and resolves
  `cloud-entry`'s `../../base` and `../../components/*` inside that clone;
  the engine's ArgoCD bootstrap already raises the repo-server exec
  timeout to 300s for this repo's size. The Application loses its
  `spec.source.kustomize` block entirely: the patches live in git, under
  review, like everything else.
- **Digests come from GHCR, with no Azure credential.**
  `ghcr.io/znasllc-io/memql-<node>:<version>` digests are byte-identical
  to the ACR pins on `main` (identity / bff / edge 0.19.6 checked), so a
  pin workflow resolves them anonymously.
- **What stays in the engine:** `overlays/cloud-entry` remains the
  canonical entry overlay -- `cmd/frontdoorhosts`, `make frontdoor` and
  the overlay gates all reference it -- and its `images:` become reference
  defaults that may lag what the instance runs; the live cluster no longer
  depends on them. `overlays/cloud` is untouched and is never
  auto-applied.
- **Who owns the pins** is the owner's decision. The recommendation is the
  shape above (the instance repo owns them). The alternatives were: the
  engine keeps them, so every entry deploy stays an engine PR through
  the merge queue; or the instance repo vendors a copy of `base` and
  `cloud-entry`, rejected because a CVE then means patching every fork.

### The External Secrets caveat

`deploy/external-secrets/` as committed cannot authenticate from this
cluster: its SecretStore uses a workload-identity federated credential
issued for ANOTHER cluster's OIDC issuer, and the vault it names holds that
retired cluster's values. That is why `memql-secrets` is hand-seeded here,
and why the two base ExternalSecrets (`livekit`, `telephony`) that
reference `keyvault-staging` are expected to stay unhealthy -- harmless
under voice-off, and part of the OutOfSync / Degraded noise.

**Do not "fix" it by repairing the federated credential or pointing the
SecretStore at the old vault.** Its entries are another cluster's master
key, operator key and DSN; syncing them over `memql-secrets` breaks auth
and database access on the live cluster. Secrets stay hand-seeded until
this instance has its own vault holding its own values -- an owner
decision, not part of the switch.

### Capture, switch, rollback

**Capture first (reads only).** The rollback artifact is the full
Application manifest; never the kubeconfig, never secret values:

```bash
kubectl -n argocd get application memql -o yaml > capture/memql-app.before.yaml
kubectl -n argocd get appproject memql -o yaml    > capture/appproject.before.yaml
kubectl -n argocd get applications -o name                          # are cert-manager / cnpg-operator Applications there?
kubectl -n argocd get secret -l argocd.argoproj.io/secret-type=repository -o name
kubectl get clusterissuer letsencrypt-prod -o yaml                  > capture/clusterissuer.yaml
kubectl -n memql get ingress,certificate,configmap/memql-domain -o yaml > capture/memql-ns.yaml
kubectl -n memql get objectstore memql-db-backup -o yaml             > capture/objectstore.yaml
kubectl -n memql get cluster memql-db -o jsonpath='{.spec.serviceAccountTemplate}'
kubectl -n memql get svc livekit livekit-rtc livekit-sip -o yaml     > capture/livekit-svcs.yaml
argocd app get memql ; argocd app manifests memql > capture/live-render.yaml
kubectl -n memql get deploy -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.template.spec.containers[0].image}{"\n"}{end}'
```

**Parity is the go / no-go.** Render the instance overlay and diff it
against the live render:

```bash
kubectl kustomize <instance-repo>/deploy/k8s/overlays/<instance> | diff - capture/live-render.yaml
```

The diff must be empty apart from the items deliberately being moved into
git (the ClusterIssuer, and the memql#4224 / memql#4225 workarounds until
the engine tag carries them).

**Switch (every line is a cluster write; each is reversible; the order
matters):**

```bash
gh repo deploy-key add argocd-instance-ro.pub --repo <instance-repo> --title argocd-instance-ro   # read-only
kubectl -n argocd create secret generic repo-instance --from-literal=type=git \
  --from-literal=url=git@github.com:<instance-repo>.git --from-file=sshPrivateKey=argocd-instance-ro
kubectl -n argocd label secret repo-instance argocd.argoproj.io/secret-type=repository
kubectl -n argocd patch appproject memql --type=json \
  -p '[{"op":"add","path":"/spec/sourceRepos/-","value":"git@github.com:<instance-repo>.git"}]'
kubectl -n argocd replace -f deploy/argocd/apps/memql.yaml                  # REPLACE, in place, same name `memql`
argocd app get memql --refresh                                              # Synced, or OutOfSync listing ONLY the moved items
argocd app diff memql                                                       # must be empty / known
argocd app sync memql --dry-run && argocd app sync memql && argocd app wait memql --health
kubectl -n memql rollout status deploy/identity                             # then bff, edge
scripts/install/verify-frontdoor.sh --hosts api.$MEMQL_DOMAIN,identity.$MEMQL_DOMAIN   # from an engine checkout
curl -s https://portal.$MEMQL_DOMAIN/runtime-config.json
```

Why each line is what it is:

- The repo Secret's `url` must match the Application's `repoURL` byte for
  byte, or Argo reports a ComparisonError; the AppProject `sourceRepos`
  entry must exist first, or the Application is refused as "repo not
  permitted in project" (harmless, but it stops reconciling).
- **`kubectl replace` the Application IN PLACE under the same name --
  never delete and recreate it.** It carries
  `resources-finalizer.argocd.argoproj.io`, so deleting it cascade-deletes
  the mesh AND the CNPG cluster, PVCs included. `replace` drops the old
  `spec.source.kustomize` block wholesale; with the render
  content-identical, the sync costs no pod churn.
- **Manual sync with prune OFF until `argocd app diff memql` is empty.** A
  prune sync from a render that omits anything the old render had deletes
  it. Only after a green sync: `automated: {selfHeal: true}` with prune
  still off until a second clean sync, then apply `root.yaml` so the
  app-of-apps manages itself.

**Rollback, at any step:** restore the captured manifest. The render is
content-identical, so the sync is a no-op.

```bash
kubectl -n argocd replace -f capture/memql-app.before.yaml && argocd app sync memql
# the AppProject entry and the repo Secret are harmless to leave; to remove them:
kubectl -n argocd replace -f capture/appproject.before.yaml ; kubectl -n argocd delete secret repo-instance
```

### The twice-daily entry pin loop (sketch)

`.github/workflows/pin-entry.yml` in the instance repo, `schedule: cron
'0 */12 * * *'` plus `workflow_dispatch`, with the default policy "latest
engine release TAG" -- a no-op until a human cuts a tag, so "twice daily"
means "within 12 hours of a cut":

1. `git ls-remote --tags https://github.com/znasllc-io/memql.git` -> the
   newest `vX.Y.Z`.
2. For each of the eight nodes,
   `docker buildx imagetools inspect ghcr.io/znasllc-io/memql-<node>:X.Y.Z`
   -> digest. Abort if any node lacks the tag (the images are not built
   yet; never half-pin).
3. Rewrite `?ref=vX.Y.Z` and the eight `digest:` lines in
   `deploy/k8s/overlays/<instance>/kustomization.yaml`; render. A render
   failure -- typically a patch target the new engine tag renamed -- means
   do not commit; an unchanged render is a no-op.
4. One-file commit: a PR with auto-merge, or direct to `main` -- the
   owner's call. With automated sync on the Application, Argo rolls within
   its refresh interval.
5. Post-roll gate (a `workflow_run` after the merge, or a second cron 30
   minutes later): `/healthz` on `identity.`, `api.` and `portal.`,
   `verify-frontdoor.sh --hosts api.$MEMQL_DOMAIN,identity.$MEMQL_DOMAIN`,
   and, once the SDK exposes it, ServerHello `engine_version == X.Y.Z`.
   Failure opens an issue; rollback is `git revert` of the pin commit plus
   a sync.

"Never auto-apply top" is structural, not a policy: the workflow edits one
file in the instance repo and has no path to `overlays/cloud`. The
engine-side process fix is the owner's: publish a GitHub Release for each
tag, so `dispatch-engine-images-on-release.yml` builds images without a
manual dispatch.

## What is not this page

- The local k3d + Argo cluster --
  [reproduce-the-cloud-locally.md](reproduce-the-cloud-locally.md)
- The six host rules in general -- [front-door.md](front-door.md)
- The `memql-project` template's own retirement of staging / prod and
  the Azure provisioning script -- the template repo and later tickets,
  not this page
