---
title: Bringing up an instance on Azure with scripts alone
audience: public
status: stable
area: operate
sinceVersion: 0.20.0
owner: platform
---

# Bringing up an instance on Azure with scripts alone

**Audience:** an operator -- or an agent session -- standing up a MemQL instance
on Azure from nothing, using only shell scripts and `az`/`kubectl`.
**Issue:** memql#4464.

Related: [azure-entry-install.md](azure-entry-install.md) (the manifest-level
detail) · [instance-scale-and-upgrade.md](instance-scale-and-upgrade.md) ·
[front-door.md](front-door.md) · [environment-parity.md](environment-parity.md)

## Why this page exists separately

MemQL can provision itself. `provisionInstance`, `installInstance`,
`repairInstance` and `deprovisionInstance` are authored automations in
`dsl/deployment/`, and driving them is the better path once an instance exists
to drive them from.

**But a running MemQL needs AI provider credentials**, and the first bring-up is
exactly the moment those are least likely to be in hand -- a new client, a new
subscription, no keys issued yet. Waiting for an API key to create a Kubernetes
cluster is a dependency nobody should accept.

So every capability behind those automations is a **capability script** that
runs identically whether an automation or a human invokes it (contract:
[capability-script-contract.md](../../internal/design/capability-script-contract.md)).
This page is the human half. Nothing here needs a MemQL instance, an API key,
or a cockpit.

```
automation provisionInstance                     <- needs a running MemQL
  -> action provisionAzureInfrastructure
    -> capability deploy.azureProvision
      -> scripts/deploy/azure-provision.sh       <- THIS PAGE. Needs nothing.
```

## What you need first

- `az` (logged in to the target tenant), `kubectl`, `jq`, `gh`.
- The instance repository for this client -- `memql-<client>`, made from the
  `memql-project` template. It holds `product.env`, the instance overlay, and
  the instance's own ArgoCD Application. **The engine repository holds none of
  this** and must not: a hostname is the operator's input and an Azure resource
  is the operator's property (memql#3593, `core/vendorname`).
- Owner or Contributor on the target subscription, plus permission to create
  role assignments (provisioning grants AcrPull and Key Vault access).

### The two-identities trap

If one email exists as BOTH a personal Microsoft account and a work account, the
Azure CLI cannot hold both: it fails with `Found multiple accounts with the same
username` (azure-cli #20168) and the login writes no profile. The cache must
hold exactly one identity at a time.

```bash
az account clear                                  # empties the MSAL cache
az login --tenant <tenant-id> --allow-no-subscriptions
```

Naming the tenant is what avoids landing in the wrong directory. In the browser
picker choose **Use another account** rather than a tile that already says
"Signed in" -- an existing session is what silently selects the wrong identity.

## 1. Verify the subscription can hold the cluster

A default instance is **6 vCPUs** (2 mesh + 1 database node, `Standard_D2as_v4`).
New subscriptions commonly allow 10.

```bash
az account show -o table
az vm list-usage --location eastus -o table | grep -E "Total Regional|DASv4"
```

If the family limit is 0 -- which happens for newer families like `DASv5` --
pick a family that has quota rather than requesting an increase mid-bring-up.

Register the providers, or every later command fails with an error that does not
mention registration:

```bash
for p in Microsoft.Compute Microsoft.ContainerService Microsoft.ContainerRegistry \
         Microsoft.Network Microsoft.Storage Microsoft.KeyVault \
         Microsoft.ManagedIdentity Microsoft.OperationalInsights; do
  az provider register -n $p
done
```

Registration is asynchronous; wait for `Registered` before continuing.

## 2. Provision the substrate

Plan first. The script is idempotent, but a dry run is how you check the names
before three of them become globally unique facts:

```bash
scripts/deploy/azure-provision.sh --print-spec | jq .

scripts/deploy/azure-provision.sh \
  --subscriptionId=<sub> \
  --resourceGroup=<rg> \
  --clusterName=<aks> \
  --registryName=<acr> \
  --keyVaultName=<kv> \
  --backupStorageAccount=<sa> \
  --location=eastus \
  --dryRun=true
```

Drop `--dryRun` to execute. It creates, in dependency order: resource group ->
registry -> key vault -> backup storage -> AKS -> **reads the OIDC issuer** ->
managed identities -> federated credentials -> role assignments.

**Three of these names are globally unique across all of Azure** -- the
registry, the key vault, and the storage account. If you are rebuilding under
names an old instance still holds, that instance must be deleted first, and a
key vault additionally **purged** (deletion reserves its name for 90 days).

**Two couplings the script now checks for you**, both learned from a real
bring-up:

- **VM size.** The default is not universally available -- newer subscriptions
  frequently carry v5/v7 and ARM families and offer no v4 at all. The script
  resolves each size against `az vm list-skus` for the region before creating
  anything, and refuses with exit 3 naming the region. This is most of what
  makes `--dryRun` worth running: without it a plan-only run reports "would
  create AKS cluster" and the real failure lands about four minutes later, after
  the resource group, registry, vault and storage account are already real.
- **Availability zones.** `--zones` defaults to `1`, and that default is
  load-bearing rather than cautious: `cloud-entry` pins
  `storageClass: managed-csi-premium-v2`, and **Premium SSD v2 attaches only to
  a VM in an availability zone**. A non-zonal cluster provisions fine and then
  strands the database pod on an attach error naming the disk type, three layers
  from the cause. Pass `--zones=` to opt out deliberately; the script warns.

  **A PVC-bind probe does not detect this.** The PV provisions and reports
  `ProvisioningSucceeded`; only the *attach* fails, so nothing short of a pod
  reaching `Running` proves it. It is also sticky -- PVs provisioned while
  non-zonal carry an empty zone topology that can never match a zonal node, so
  fixing the pool is not enough and the PVCs must be recycled.

**The ordering is load-bearing in one place.** A federated identity credential
names the cluster's OIDC issuer URL, which does not exist until the cluster
does. That is why provisioning is one script rather than a checklist: getting
this backwards produces identities that authenticate to nothing, and it surfaces
much later as pods that cannot read secrets.

Keep the result envelope. `esoClientId` and `dbClientId` are what the instance
overlay binds its service accounts to:

```bash
scripts/deploy/azure-provision.sh ... | tee /tmp/provision.json | jq .result
```

## 3. Get the engine images into the new registry

The instance pulls engine images from its own registry. Two ways to fill it:

- **Build them** on the GitHub build server (`build-engine-images.yml`,
  `workflow_dispatch` on `main` with a `version` input). This is the sanctioned
  path for anything deployed -- see the HARD RULE in `CLAUDE.md`.
- **Import them** from an existing registry with `az acr import`. This is the
  right tool when the source registry is in a DIFFERENT TENANT, because import
  authenticates with *registry* credentials rather than ARM, so it crosses a
  tenant boundary that role assignments cannot:

```bash
az acr import --name <new-acr> \
  --source <old-acr>.azurecr.io/memql-bff:<tag> \
  --username <token-name> --password <token-secret>
```

Do not hand-build and push release images from an operator machine. That path is
superseded by the build server (reproducible, native linux/amd64, provenance).

## 4. Bootstrap the cluster

```bash
az aks get-credentials -g <rg> -n <aks> --overwrite-existing
kubectl get nodes -o wide
```

Then install ArgoCD, cert-manager and the CNPG operator, and apply the
Applications. [azure-entry-install.md](azure-entry-install.md) carries the
manifest-level detail, including the DNS-01 prerequisites and the voice-off
holds. Follow it rather than improvising: several of its steps exist because a
previous install failed in a way that looked healthy.

## 5. Apply the instance's OWN Application

This is the step the first cloud instance got wrong, and everything else
followed from it.

```bash
kubectl apply -f /path/to/memql-<client>/deploy/argocd/apps/memql.yaml
```

That Application points at the **instance repository**, tracks a **branch**, and
carries **no inline patches**. Verify all three before applying:

```bash
grep -E "repoURL|targetRevision|path" .../deploy/argocd/apps/memql.yaml
grep -c "patches:" .../deploy/argocd/apps/memql.yaml     # expect 0
```

**Never** hand-build an Application that points at the engine repository and
carries the instance's configuration as `kustomize.patches`. It works, briefly.
Then the desired state of the installation exists only as an object on the
cluster it configures -- so the cluster is its own source of truth, and deleting
it destroys the specification. If the pinned revision is a bare commit SHA, a
squash-merge deletes it and Argo can never resolve its target again while still
reporting `Healthy`. All three of those failures shipped together on the first
instance (memql#4463); `deploy/k8s/overlays/argo_application_reproducible_test.go`
now fails the build on each.

## 6. Seed secrets

External Secrets reads the key vault as the `id-eso-*` identity, federated to
the `external-secrets-kv` service account in step 2. Write the values it expects:

```bash
az keyvault secret set --vault-name <kv> --name <secret-name> --value <value>
```

The set is instance-specific; take it from the instance repository rather than
this page, which would go stale. **If you are migrating, read the old vault
before deleting it** -- provider API keys frequently exist nowhere else.

## 7. Verify -- and do not accept `Healthy` as the answer

```bash
kubectl get pods -n memql -o wide                    # Running, READY n/n
kubectl get application memql -n argocd \
  -o jsonpath='{.status.sync.status}{"\n"}'          # Synced
kubectl get pdb -n memql                             # see the scale runbook
kubectl get ingress -n memql                         # the real hosts
```

ArgoCD's `Healthy` describes the resources it last managed to apply. An
Application can report `Healthy` while `OutOfSync`, pinned to a revision that no
longer exists, reconciling nothing at all. **`Synced` is the field that answers
the question you are asking.**

## 8. DNS

Read the ingress controller's public address and point the domain at it:

```bash
kubectl get svc -n ingress-nginx -o wide
```

Records needed (`<domain>` is the instance's `MEMQL_DOMAIN`):

| Record | Type | Points at |
|---|---|---|
| `api.<domain>` | A | ingress public IP |
| `identity.<domain>` | A | ingress public IP |
| `mcp.<domain>` | A | ingress public IP |
| `portal.<domain>` | A | ingress public IP |
| `*.<domain>` | A | ingress public IP |
| `<domain>` (apex) | A | ingress public IP |

The wildcard matches exactly one label, which is why every host is a single
label under the domain. Certificates are a separate matter from routing: an
HTTP-01 issuer cannot issue for the wildcard, so without a DNS-01 issuer the
certificate names exact hosts only. See [front-door.md](front-door.md).

## Tearing down

```bash
scripts/deploy/azure-teardown.sh \
  --subscriptionId=<sub> --resourceGroup=<rg> --confirm=<rg> --dryRun=true
```

`--confirm` must equal the resource group exactly. Credential and backup stores
are **preserved** unless `--deleteStores` is given, and freeing a key vault's
globally-unique name additionally needs `--purgeKeyVault`.

To stop compute spend without destroying anything else, name the cluster and
nothing is touched beyond it:

```bash
scripts/deploy/azure-teardown.sh ... --clusterName=<aks> --confirm=<rg>
```

That is usually what "shut it down" actually means: at the reference sizing the
cluster is roughly **$8.22/day** and everything else together is about
**$0.18/day**.
