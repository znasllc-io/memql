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

There are **eleven ordered steps** between a provisioned substrate and an ArgoCD
sync that means anything, and six of them are dependencies that exist on no
manifest in either repository. Every one of those six **fails silently** -- a
missing Secret named by a volume leaves a pod in `ContainerCreating` forever
with no log line, because the container never starts.

They are scripted, in order, and each is idempotent and verify-first:

```bash
# Steps 1-8: kubeconfig, cert-manager, CNPG + Barman, the ESO CRDs, External
# Secrets, ingress-nginx + its IngressClass, the prometheus-operator with this
# platform's alert rules, and the letsencrypt-prod ClusterIssuer -- all from the
# versions pinned in this repository.
scripts/deploy/install-cluster-operators.sh \
  --subscriptionId=<sub> --resourceGroup=<rg> --clusterName=<aks> \
  --acmeEmail=<ops@example.com> --dryRun=true

# Steps 8-9: the namespace, the EMPTY memql-secrets shell, and credential
# GENERATION into this instance's own vault. See step 6 below.
scripts/deploy/seed-instance-secrets.sh --keyVaultName=<kv> --dryRun=true

# Step 10: this instance's SecretStore and ExternalSecrets, rendered from its
# own vault and its own identity. esoClientId comes from provisioning's result
# envelope in step 2.
scripts/deploy/wire-external-secrets.sh \
  --keyVaultName=<kv> --tenantId=<tenant> --esoClientId=<guid> --dryRun=true

# Step 11: the ArgoCD repository credential and the AppProject sourceRepos
# entry. Without the second, the Application is refused as "repo not permitted
# in project" -- which stops reconciliation while looking harmless.
scripts/deploy/register-gitops-repo.sh \
  --repoUrl=<https://...> --transport=https --tokenFile=<path> --dryRun=true
```

Drop `--dryRun=true` to apply. Each writes one JSON envelope to stdout and its
logs to stderr, so `--print-spec` tells you the parameters without running
anything.

Two things worth knowing before you start:

- **`--acmeEmail` has no default, deliberately.** A wrong address is where the
  expiry warnings go. Omit it and the ClusterIssuer is skipped, which means
  Certificates stay `Pending` and ingress-nginx serves its self-signed default
  -- the site **loads**, with a browser warning, which is why this one is easy
  to miss.
- **The credential transport is an input, not a preference.** Deploy keys are
  disabled org-wide at some organizations, which makes the ssh path unavailable
  rather than unattractive.
- **The alert rules are installed here, and that is the point.**
  `deploy/k8s/monitoring` holds every alert this platform ships -- WAL
  archiving, volume fill, replication, auth -- and until now nothing installed
  it: its only invocation in the whole tree was a manual `kubectl` in its own
  README. That is the worst member of the silent-dependency class, because a
  missing Secret leaves a pod in `ContainerCreating` and **a missing alerting
  stack produces a cluster that looks quiet** -- and quiet is what a healthy
  system sounds like. One instance ran its entire life with WAL archiving broken
  because the two alerts that would have caught it were never deployed. If you
  already run a Prometheus stack, the operator install is skipped automatically
  (the CRD is the probe) and only the rules are applied; `--skipMonitoring=true`
  turns the whole step off, and the script says plainly what that costs.

The same eleven steps run as one automation -- `installInstance`, raised by
`instance.installRequested` -- with `argoSync` last and the settle after it.
[azure-entry-install.md](azure-entry-install.md) carries the manifest-level
detail. Follow it rather than improvising: several of its steps exist because a
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

`scripts/deploy/seed-instance-secrets.sh` (step 4 above) generates the seven
entries the mesh reads. Three properties of it are worth stating, because each
was learned the expensive way:

- **It GENERATES; it never migrates, and it has no flag that would.** A retired
  vault's entries are *another cluster's* master key, operator key and DSN. A
  new instance is a new trust domain. On the last rebuild, four of the seven did
  not exist in the retired vault at all, so a migration would have imported a
  dead value and still left four gaps.
- **Create-if-absent, never overwrite.** Regenerating the master key after
  anything has been encrypted under it destroys the ability to read it back, and
  the caller is an automation with at-least-once delivery. Rotation is a
  separate, explicit verb -- not a re-run.
- **The DSN and the CNPG credential carry ONE password.** Generate them
  independently and the cluster comes up **healthy** and the engine cannot log
  in: every pod Running, every probe green, an authentication failure in the
  logs. On a re-run the password is read back out of the existing DSN.

No value is written to a log line, to the result envelope, or to argv -- every
one reaches `az` and `kubectl` through a file in a mode-700 directory that is
shredded on exit. The envelope reports counts and names.

**If you are migrating provider API keys, read the old vault before deleting
it** -- those frequently exist nowhere else, and they are the one category that
is genuinely the operator's to carry across.

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

## Resetting an instance

"Start fresh" reads as "rebuild everything". For an **ownership reset** -- you
are re-testing the take-ownership flow, or handing the instance to a different
first owner -- it means one thing only:

> **Wipe the database. Leave everything else exactly where it is.**

Ownership lives in `v1:identity:user` rows and the credentials that point at
them. Dropping the database is what un-claims the instance. Nothing above the
database records who owns it.

### What an ownership reset must NOT rebuild

| Resource | Why leaving it alone is correct |
|---|---|
| The front-door **certificate** | Re-issuing walks into a Let's Encrypt rate limit -- see below. This is the one that bites. |
| The Azure **substrate** | Resource group, AKS, node pools and identities carry no ownership state. `azure-provision.sh` is idempotent, so re-running it is a slow no-op at best. |
| The container **registry** | Images are addressed by digest and are not owner-scoped. |
| The **key vault** | The master key, operator key and signing key are the instance's identity, not the owner's. Rotating them on a reset also invalidates every value already encrypted under the old master key. |
| **DNS** | The records point at the ingress controller's public IP, which the reset does not move. |

Rebuilding any of these is pure churn. Rebuilding the certificate is worse than
churn.

### The certificate is rate-limited, and the limit is not visible from here

Let's Encrypt enforces a **duplicate-certificate limit of 5 per exact set of
names per week**. Every front-door reissue for the same host list spends one.
A handful of reinstall loops exhausts it, and the instance is then left with
**no TLS and a multi-day wait** -- an outcome strictly worse than whatever was
being re-tested.

The trap is that the reinstall loop *feels* cheap and reversible, and for every
other resource on this page it is. This one resource breaks the pattern in both
halves: the counter is enforced on Let's Encrypt's side where the operator
cannot read it, and once exhausted **no action on the operator's side shortens
the wait**. There is nothing to retry, restart, or escalate.

So: an ownership reset does not touch cert-manager, does not delete the
`Certificate` or its Secret, and does not delete the namespace those live in.
If a certificate must genuinely be replaced, change the name set (which starts
a fresh bucket) rather than reissuing the same one.

### Doing the reset

```bash
# 1. Scale the mesh down so nothing reconnects mid-wipe.
kubectl scale deploy -n memql --all --replicas=0

# 2. Drop and recreate the application database. The CNPG Cluster, its PVCs,
#    and memql-db-app-creds all stay -- only the contents go.
kubectl exec -n memql memql-db-1 -- psql -U postgres -c \
  'DROP DATABASE memql WITH (FORCE);'
kubectl exec -n memql memql-db-1 -- psql -U postgres -c \
  'CREATE DATABASE memql OWNER memql;'

# 3. Scale back up. Migrations run on startup and the instance comes up
#    unclaimed, ready for the first owner to take it.
kubectl scale deploy -n memql --all --replicas=1
```

Then mint an enrolment or recovery credential for the new first owner exactly as
on a first install -- the instance is in the same state a fresh one is, because
the database is the only thing that recorded otherwise.

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
