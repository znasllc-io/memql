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
| sites (wildcard) | `*.$MEMQL_DOMAIN` | every other site; certified by `memql-wildcard-tls` over DNS-01 (memql#4347) |
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
reach the edge for every site. It is a separate thing from the wildcard
CERTIFICATE below -- both are needed, and the certificate additionally needs
the zone to be an **Azure DNS** zone this cluster's identity may write to,
because that is how the DNS-01 challenge is answered.

**TWO certificates, split by which ACME challenge can issue each half**
(memql#4224, memql#4347):

| Certificate | Issuer | Names |
|---|---|---|
| `memql-front-door-tls` | `letsencrypt-prod` (HTTP-01) | `api.`, `identity.`, `mcp.`, `portal.`, the apex |
| `memql-wildcard-tls` | `letsencrypt-dns01` (DNS-01, Azure DNS) | `*.$MEMQL_DOMAIN`, the apex |

- **Do not add a `*.$MEMQL_DOMAIN` dnsName to `memql-front-door-tls`.** ACME
  cannot serve an HTTP-01 challenge for a wildcard, and one wildcard name
  fails the WHOLE order: the Certificate sits Pending, the Secret is never
  written, and every host serves ingress-nginx's self-signed default
  ("Kubernetes Ingress Controller Fake Certificate"; Safari: "This
  Connection Is Not Private"). That is how the first entry-shape bring-up
  went, and the hand-edit to exact names that followed is now the generated
  shape. The wildcard belongs on `memql-wildcard-tls` and nowhere else.
- **`tls.hosts` must name hosts the certificate that entry points at can
  cover.** ingress-nginx verifies the certificate against each host an
  Ingress lists under `tls` and falls back to the default for a host that
  certificate does not name -- and it builds a certificate-bearing server
  block per RULE host, never per `tls` host. So `edge-front-door` carries
  TWO `tls` entries: the apex against `memql-front-door-tls`, and
  `*.$MEMQL_DOMAIN` against `memql-wildcard-tls`. A portal with no exact
  rule of its own still reproduces the fake-certificate failure for
  `portal.$MEMQL_DOMAIN` with both certificates Ready; the generator emits
  `portal-front-door` (an exact rule to the same edge Service) for exactly
  that reason. The cluster-side `portal-front-door` Ingress that was
  hand-made as the ops workaround has the same name, so Argo adopts and
  overwrites it on the next sync.

**The wildcard certificate is what makes a hosted site live over TLS with
no operator step.** Before it, a customer site hostname routed by the
wildcard had no certificate until somebody added a `Certificate` and an
exact-host Ingress for it -- one object pair per site. Now `*.$MEMQL_DOMAIN`
already names it. A hostname OUTSIDE `$MEMQL_DOMAIN` -- a customer's own
apex, a second domain -- is still that object pair
([site-hosting.md](site-hosting.md#2-add-the-hostname)), and so is anything
more than one label deep (`shop.eu.$MEMQL_DOMAIN`).

### DNS-01 prerequisites, in order

Do these BEFORE the first sync. Skipping them is not fatal -- the wildcard
Certificate simply never becomes Ready and hosted sites fall back to the
controller's default, which is where they were before memql#4347 -- but the
role hosts and sign-in are unaffected either way, which is the whole reason
the two certificates are separate.

```bash
export ZONE_GROUP="<dns-zone-rg>"        # the resource group holding the DNS zone
export IDENTITY_GROUP="<cluster-rg>"     # where the managed identity is created
export CLUSTER_RG="<cluster-rg>"
export CLUSTER="<aks-name>"
export SUBSCRIPTION_ID="$(az account show --query id -o tsv)"

# 1. The DNS zone for the domain, delegated at the registrar. `az network dns
#    zone show` must succeed before anything below is worth running.
az network dns zone create -g "$ZONE_GROUP" -n "$MEMQL_DOMAIN"
az network dns zone show  -g "$ZONE_GROUP" -n "$MEMQL_DOMAIN" --query nameServers -o tsv
#    ...point the registrar at those name servers, then wait for the delegation
#    to resolve. `dig NS $MEMQL_DOMAIN` must answer with them.

# 2. A user-assigned managed identity, and DNS Zone Contributor ON THE ZONE.
#    Scope it to the zone, never to the subscription: this identity's only job
#    is writing _acme-challenge TXT records under one name.
az identity create -g "$IDENTITY_GROUP" -n id-certmanager-memql
DNS_CLIENT_ID="$(az identity show -g "$IDENTITY_GROUP" -n id-certmanager-memql --query clientId -o tsv)"
DNS_PRINCIPAL_ID="$(az identity show -g "$IDENTITY_GROUP" -n id-certmanager-memql --query principalId -o tsv)"
ZONE_ID="$(az network dns zone show -g "$ZONE_GROUP" -n "$MEMQL_DOMAIN" --query id -o tsv)"
az role assignment create --assignee "$DNS_PRINCIPAL_ID" \
  --role "DNS Zone Contributor" --scope "$ZONE_ID"

# 3. A federated credential for cert-manager's ServiceAccount -- the same
#    secret-less pattern ESO uses for Key Vault (deploy/external-secrets/README.md).
#    The subject is cert-manager's default SA in its own namespace.
OIDC="$(az aks show -g "$CLUSTER_RG" -n "$CLUSTER" --query oidcIssuerProfile.issuerUrl -o tsv)"
az identity federated-credential create -g "$IDENTITY_GROUP" \
  --identity-name id-certmanager-memql --name cert-manager \
  --issuer "$OIDC" --subject system:serviceaccount:cert-manager:cert-manager \
  --audiences api://AzureADTokenExchange

# 4. The four values the ClusterIssuer needs, for the Argo patch list below.
#    The webhook wiring itself (the SA client-id annotation and the pod label)
#    is already in git -- see the note under this block -- so there is nothing
#    to patch by hand here; $DNS_CLIENT_ID is the value you substitute into
#    deploy/cert-manager/install/kustomization.yaml.
echo "hostedZoneName    = $MEMQL_DOMAIN"
echo "resourceGroupName = $ZONE_GROUP"
echo "subscriptionID    = $SUBSCRIPTION_ID"
echo "clientID          = $DNS_CLIENT_ID"
```

> **INFO: the webhook wiring is committed, not hand-patched.** The
> workload-identity webhook keys off two things the upstream cert-manager
> manifest does not carry: `azure.workload.identity/client-id` on the
> `cert-manager` ServiceAccount and `azure.workload.identity/use: "true"` on the
> controller's pod template. Both are `patches:` entries in
> `deploy/cert-manager/install/kustomization.yaml`, so ArgoCD owns them and a
> cert-manager bump cannot silently drop them. The annotation ships as the
> placeholder `REPLACE-WITH-CERT-MANAGER-IDENTITY-CLIENT-ID`; substitute
> `$DNS_CLIENT_ID` from step 2 and commit it, the same way the cloud overlays
> carry the database identity's client id.
>
> Both values are read ONLY by that webhook, so this one shape serves every
> deploy target: on a cluster without the webhook -- every local k3d cluster,
> which reaches the same path through `scripts/k3d/up.sh` -- the label matches
> no admission rule and the annotation is a string nothing dereferences.
>
> Verify after a sync: `kubectl -n cert-manager get deploy cert-manager -o
> jsonpath='{.spec.template.metadata.labels}'` shows
> `azure.workload.identity/use`, and `kubectl -n cert-manager get sa
> cert-manager -o jsonpath='{.metadata.annotations}'` shows a real client id
> rather than the placeholder.

Then, after the first sync:

```bash
kubectl -n memql get certificate memql-front-door-tls memql-wildcard-tls
# both must reach READY True. A wildcard stuck at False is almost always the
# zone: `kubectl -n memql describe certificaterequest` names the Azure error.
```

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
  `$MEMQL_DOMAIN` (the apex, on `memql-front-door-tls`) and
  `/spec/tls/1/hosts/0` = `*.$MEMQL_DOMAIN` (the wildcard, on
  `memql-wildcard-tls`) -- TWO tls entries, one per certificate
- Certificate `memql-wildcard-tls` `spec.dnsNames` → `*.$MEMQL_DOMAIN`,
  `$MEMQL_DOMAIN` -- two entries in that order (`/spec/dnsNames/0`,
  `/spec/dnsNames/1`); the apex is on it as well as the wildcard because
  `*.<domain>` matches exactly one label and the apex has none
- ClusterIssuer `letsencrypt-dns01` → the four values step 4 above printed:
  `/spec/acme/solvers/0/dns01/azureDNS/hostedZoneName` = `$MEMQL_DOMAIN`,
  `.../resourceGroupName`, `.../subscriptionID`,
  `.../managedIdentity/clientID`, plus `/spec/acme/email`
- ConfigMap `memql-domain` key `MEMQL_DOMAIN` → the install domain

> **WARNING: the pointer set changed twice -- memql#4224, then memql#4347.**
> A patch written against the ORIGINAL shape (two dnsNames on
> `memql-front-door-tls`, a wildcard under the edge Ingress's
> `/spec/tls/0/hosts`) does not fail loudly everywhere: the two dnsNames
> replaces still succeed and put the wildcard back into the HTTP-01 order,
> which is the Pending Certificate again. A patch written against the
> memql#4224 shape fails differently and more quietly: it simply has no
> `/spec/tls/1/hosts/0` entry, so `edge-front-door` keeps the committed
> `*.memql.localhost` under tls -- a tls host the real wildcard certificate
> does not name, which is the memql#4224 symptom at the one host that change
> was about. Rewrite the Application patch to the list above before syncing.

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
4. Wait for DNS, then wait for BOTH certificates to become Ready:
   `kubectl -n memql get certificate memql-front-door-tls memql-wildcard-tls`.
   `memql-front-door-tls` must show `READY True` with the five exact
   dnsNames and no wildcard; `memql-wildcard-tls` must show `READY True`
   with `*.$MEMQL_DOMAIN` and the apex. Then prove the SNI match memql#4224
   was about, on every exact host:

```bash
for h in identity api portal mcp; do
  openssl s_client -servername "$h.$MEMQL_DOMAIN" -connect "$h.$MEMQL_DOMAIN:443" </dev/null 2>/dev/null \
    | openssl x509 -noout -issuer -subject
done
```

   Every line must name Let's Encrypt as the issuer. `Kubernetes Ingress
   Controller Fake Certificate` on any host means that host has no exact
   rule or is not in the certificate -- re-check the patch list above. Run
   the same probe against an arbitrary site name to prove the wildcard:

```bash
openssl s_client -servername "anything.$MEMQL_DOMAIN" -connect "anything.$MEMQL_DOMAIN:443" </dev/null 2>/dev/null \
  | openssl x509 -noout -issuer -subject
```

   Let's Encrypt with subject `*.$MEMQL_DOMAIN` means the DNS-01 half is
   live; the fake certificate there means the wildcard Certificate is not
   Ready or `/spec/tls/1/hosts/0` was never patched.
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

## Anthropic: federate instead of seeding a key (optional, after bring-up)

`MEMQL_AI_ANTHROPIC_API_KEY` is one of the keys `memql-secrets` carries at
bring-up. It can be removed entirely once the cluster authenticates to
Anthropic by workload identity federation: the pod presents the token
Kubernetes projects for it and Anthropic returns a one-hour bearer, so no
long-lived vendor credential is left in the cluster.

The manifests are already in place -- `deploy/k8s/base` gives every engine
Deployment the `memql-engine` ServiceAccount, the projected
`anthropic-identity` token and the empty `memql-anthropic-federation`
ConfigMap -- so the cutover is Console work plus three ids in the overlay.

Four values join this instance's per-cluster install values, alongside the
domain, the DB identity client id and the mail tenant ids:

```
MEMQL_AI_ANTHROPIC_FEDERATION_RULE_ID    fdrl_...
MEMQL_AI_ANTHROPIC_ORGANIZATION_ID       <organization uuid>
MEMQL_AI_ANTHROPIC_SERVICE_ACCOUNT_ID    svac_...
MEMQL_AI_ANTHROPIC_WORKSPACE_ID          (usually empty)
```

They are bound to THIS cluster's OIDC issuer. A re-created cluster gets a new
issuer, which invalidates the federation rule -- record them with the rest of
the per-cluster values, and expect to redo the Console steps if the cluster is
rebuilt.

Steps, deny reasons and the verification command:
[auth/anthropic-federation.md](auth/anthropic-federation.md).

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
- [ ] Create **a new `id-memql-mail` app registration for THIS instance** --
      never reuse another instance's. Two reasons, and the second one has
      already cost somebody a secret: the blast radius of a leaked client
      secret is one instance rather than all of them, and adding a secret to
      a shared registration is one flag away from destroying the others (see
      the `--append` warning below).
- [ ] Grant `Mail.Send` (Application) with admin consent -- on the mailbox
      tenant only.
- [ ] **Apply an `ApplicationAccessPolicy`, and do not treat it as optional.**
      Until you do, this app can send as ANY mailbox in the tenant. See the
      section below for the invocation; it is Exchange Online PowerShell and
      is not reachable from `az`, which is exactly why it gets skipped.
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

### `Mail.Send` is tenant-wide until you scope it

The Entra display name for the permission is literally **"Send mail as any
user"**, and that is what it grants. An app registration holding it can send
as every mailbox in the tenant -- the CEO's included -- not only as the sender
address you configured. Nothing in the Azure portal, in `az`, or in this
install path narrows it for you, so an instance stood up without the step
below has a tenant-wide send capability sitting in a Key Vault.

Narrowing it is Exchange Online PowerShell. There is no `az` equivalent and no
portal blade, which is the whole reason the step goes missing:

```powershell
# Install-Module ExchangeOnlineManagement -Scope CurrentUser
Connect-ExchangeOnline -Organization <mailbox-tenant-domain>

# 1. A mail-enabled security group whose only member is the sender mailbox.
#    The policy scopes to a GROUP, so this is how "only noreply@" is spelled.
New-DistributionGroup -Name "memql-mail-senders" `
  -Type Security `
  -PrimarySmtpAddress "memql-mail-senders@<domain>" `
  -Members "noreply@<domain>"

# 2. Restrict the app to that group. -AppId is MEMQL_EMAIL_AZURE_CLIENT_ID.
New-ApplicationAccessPolicy `
  -AppId <client-id> `
  -PolicyScopeGroupId "memql-mail-senders@<domain>" `
  -AccessRight RestrictAccess `
  -Description "MemQL transactional mail: may send only as noreply@<domain>."

# 3. Verify BOTH directions. A policy that grants is not evidence it denies.
Test-ApplicationAccessPolicy -Identity "noreply@<domain>"  -AppId <client-id>
Test-ApplicationAccessPolicy -Identity "<a-real-person>@<domain>" -AppId <client-id>
# want: Granted, then Denied. Allow up to ~30 minutes for replication before
# reading a Granted-everywhere result as a failed policy.
```

Microsoft also offers **RBAC for Applications** (`New-ManagementRoleAssignment
-App ... -Role "Application Mail.Send" -CustomResourceScope ...`) as a newer,
more granular way to say the same thing. It is likewise Exchange Online
PowerShell. Prefer it if your tenant supports it; the requirement this
checklist is enforcing is that the app is scoped by ONE of them, not which.

> **`az ad app credential reset` without `--append` DELETES every existing
> secret on the app registration.** On the bring-up this was found on, the
> secret it destroyed was labelled `memql-keep-it`. Any automation that
> provisions mail credentials must pass `--append`:
>
> ```bash
> az ad app credential reset --id <client-id> --append --years 2
> ```
>
> A one-flag difference between additive and destructive, on a command whose
> name says "reset" and whose default is "replace all", is the second and
> independent reason to give every instance its own app registration: on a
> registration nothing else uses, the wrong flag costs you nothing.

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
  `ClusterIssuer letsencrypt-prod` (HTTP-01) plus `letsencrypt-dns01`
  (DNS-01, which unlike the first one IS in git -- overlays/cloud-entry
  ships it), and the hand-seeded
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
reference `keyvault` are expected to stay unhealthy -- harmless
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
