---
title: What a real first bring-up needed that the lifecycle automations do not model
audience: internal
status: draft
area: planning
sinceVersion: 0.20.0
owner: platform
---

# What a real first bring-up needed that the lifecycle automations do not model

**Source:** the ZNAS LLC instance bring-up, 2026-08-25 -- a genuinely empty Azure
subscription to a running mesh, driven by shell only.
**Audience:** whoever is building out `dsl/deployment/` (`provisionInstance`,
`installInstance`, `repairInstance`, `bringUpInstance`, `deprovisionInstance`).
**Related:** [azure-instance-bringup.md](../../public/operate/azure-instance-bringup.md) ·
[azure-entry-install.md](../../public/operate/azure-entry-install.md) ·
memql#4463, memql#4464

## The one-sentence finding

`bringUpInstance` is `provisionInstance` then `installInstance`, which is
*substrate* then `argoSync` -- and **every single thing that actually went wrong
lives in the gap between those two steps.** That gap is not small and it is not
incidental: it is eleven ordered, failure-prone steps, six of which are
undeclared dependencies that exist on no manifest in either repository.

`provisionAzureInfrastructure` worked on its first real execution and stayed
idempotent across three runs. Nothing after it was modelled at all.

---

## 1. The gap between `provisionInstance` and `installInstance`

`argoSync` against a freshly provisioned cluster syncs nothing, because on that
cluster there is no ArgoCD. In order, what had to happen first:

| # | Step | Currently modelled? |
|---|---|---|
| 1 | `az aks get-credentials` -- a kubeconfig for every later step | no |
| 2 | cert-manager (`deploy/cert-manager/install`) | no |
| 3 | CNPG operator + Barman plugin (`deploy/cnpg/install`) | no |
| 4 | **ESO CRDs** -- fetched separately, see §3 | no |
| 5 | External Secrets (`deploy/external-secrets/install`) | no |
| 6 | **ingress-nginx** + its IngressClass | no |
| 7 | A `letsencrypt-prod` ClusterIssuer | no |
| 8 | `memql` namespace, and an EMPTY `memql-secrets` shell (see §4) | no |
| 9 | Key Vault secret generation (see §2) | no |
| 10 | SecretStore + ExternalSecrets, patched with provisioning's client ids | no |
| 11 | ArgoCD repo credential + AppProject `sourceRepos` entry (see §6) | no |

Only then does `argoSync` mean anything.

**Recommendation.** `installInstance` today is a one-step alias for `argoSync`.
It should become the phase that owns steps 1-11, with `argoSync` as its LAST
step. Steps 2-7 are a natural `action installClusterOperators`; 8-10 are a
natural `action seedInstanceSecrets`; 11 is `action registerGitOpsRepo`.

---

## 2. Secrets: generation is the operation, not migration

The runbook's step 6 says "seed the vault", and the instinct on a rebuild is to
copy the old vault forward. **That is wrong, and the engine's own docs forbid
it** (`azure-entry-install.md`: the retired vault's entries "are another
cluster's master key, operator key and DSN").

Concretely, of the **seven** Key Vault entries the mesh reads, **four did not
exist in the retired vault at all** -- it predates memql#3958/#3960 -- and it
still carried `memql-genesis-b64`, a secret that was *retired*. A migration
would have imported a dead value and still left four gaps.

So the lifecycle needs a **generate** capability, not a **copy** one:

| Vault entry | Generator |
|---|---|
| `memql-master-key` | `openssl rand -hex 32` (64 hex chars; `component/secret/encryption.go`) |
| `memql-operator-key` | `openssl rand -hex 32` -- deliberately NOT the master key (memql#3519) |
| `memql-node-bootstrap-token` | `openssl rand -hex 32` |
| `memql-identity-signing-key-b64` | `head -c 32 /dev/urandom \| base64` (memql#550) |
| `memql-identity-signing-key-created-at` | RFC3339 now (memql#3960) |
| `memory-nodes-database-dsn` | composed, see below |
| `memory-nodes-database-direct-dsn` | same endpoint -- no PgBouncer in this shape |

**Three properties the capability must have**, each learned the hard way:

- **Create-if-absent, never overwrite.** Regenerating `memql-master-key` after
  anything has been encrypted under it destroys the ability to read it back.
  This is the single most destructive thing a redelivered
  `instance.provisionRequested` event could do, and at-least-once delivery is
  the delivery model. A rotation must be a separate, explicit verb.
- **The DSN and the CNPG bootstrap secret are ONE value.** CNPG creates the
  database from `memql-db-app-creds`; the engine connects with
  `MEMQL_DATABASE_DSN`. If those disagree the cluster comes up *healthy* and the
  engine cannot log in. On a re-run the password must be read back out of the
  existing DSN, never regenerated.
- **No value in a log line, a result envelope, or argv.** argv is world-readable;
  `seed-bootstrap.sh` already makes this argument for `--from-file`. Values
  reached `az` through a file here for the same reason.

A working implementation is in this session's scratch as
`seed-instance-secrets.sh` (contract-conformant, `--print-spec`, idempotent:
second run reported `created: 0, kept: 9, changed: false`). It is a candidate
for `scripts/deploy/`.

---

## 3. Six undeclared dependencies, and why they all fail silently

This is the deepest finding and the one most worth encoding. Each of these is
assumed-installed by some committed manifest, installed by nothing, and fails in
a way that does not name itself.

1. **ESO CRDs.** `eso-values.yaml` sets `installCRDs: false` with the comment
   "CRDs are managed out-of-band (already installed on the cluster)". On a fresh
   cluster they are not, and the ESO controller **CrashLoopBackOffs** while the
   webhook stays happily Running. The fix is a separate pinned apply of
   `deploy/crds/bundle.yaml` at the matching tag.
2. **`memql-ca` + `identity-tls`.** Every mesh Deployment mounts `memql-ca`;
   identity mounts `identity-tls`; nodes dial `https://identity:8085` and verify
   against `/etc/memql/cacerts/ca.crt`. **Nothing in base, components or
   cloud-entry creates either.** Locally `make secrets` mints them from mkcert;
   in the cloud they were hand-seeded on the retired cluster. This is memql#4463's
   "the cluster is its own source of truth" in a second place nobody had looked.
   Now declared as a cert-manager self-signed chain in the instance repo.
3. **ingress-nginx.** Six Ingresses declare `ingressClassName: nginx`; no
   IngressClass and no controller exist. Every Ingress sits with `ADDRESS`
   empty, which reads like "still starting".
4. **A `letsencrypt-prod` ClusterIssuer.** Referenced by the front-door
   annotations. `cloud-entry/dns01-wildcard-tls.yaml` exists at `v0.19.6` but is
   **absent from that kustomization's `resources:` list**, so it ships nothing.
5. **cert-manager**, already fixed upstream (memql#3845) -- the precedent for
   treating the other five the same way.
6. **CNPG + Barman plugin**, likewise installed by `deploy/cnpg/install` but by
   no lifecycle step.

**The shared failure mode is silence.** A missing Secret named by a volume does
not error -- the pod sits in `ContainerCreating` **forever**:

```
MountVolume.SetUp failed for volume "memql-ca" : secret "memql-ca" not found
```

Seven mesh Deployments hung together for seventeen minutes with no container
started, so no log line, no CrashLoopBackOff, and nothing naming TLS.

**Recommendation.** An `action installClusterOperators` should install all six
from pinned refs, and `repairInstance` should VERIFY them by existence
(IngressClass present, CRD `externalsecrets.external-secrets.io` served,
ClusterIssuer Ready, `memql-ca` Secret present) rather than by health -- the
whole class is invisible to health checks.

---

## 4. `creationPolicy: Merge` needs a bootstrap shell

Both `memql-secrets` ExternalSecrets target the same Secret with
`creationPolicy: Merge`, which merges into an existing Secret and **does not
create one**. That is correct for the migration it was written for and wrong for
a fresh install, where nothing has created it. An empty
`kubectl create secret generic memql-secrets` is the bootstrap. Cheap to do,
impossible to guess from the symptom.

---

## 5. The substrate and the overlay are COUPLED, and nothing checks it

Two independent coupling failures, both between what `azure-provision.sh`
creates and what `cloud-entry` assumes:

- **VM size.** The script defaults to `Standard_D2as_v4`. The target
  subscription does not offer it at all -- newer subscriptions carry v5/v7 and
  ARM families only. `--dryRun` cannot catch this: it reports "would create AKS
  cluster" and the failure arrives four minutes later, **after** the resource
  group, registry, vault and storage account are already real. Resolved with
  `Standard_D2as_v7` (2 vCPU, x64, quota 10, $0.0908/hr -- marginally cheaper
  than the v4).
- **Availability zones, and this one is subtle.** `cloud-entry` pins
  `storageClass: managed-csi-premium-v2`. **Premium SSD v2 can only attach to a
  VM in an availability zone**, and `az aks create` without `--zones` produces a
  non-zonal cluster. So the script's own default substrate cannot run the
  overlay the engine ships. The error surfaces at the CNPG initdb pod, three
  layers from the cause:

  ```
  Managed disks with 'PremiumV2_LRS' storage account type can be used only
  with Virtual Machines in an Availability Zone.
  ```

  **A PVC-bind probe is NOT sufficient to detect this** -- I ran one, it
  reported `ProvisioningSucceeded`, and I wrongly cleared the concern. The PV
  object is created fine; the *attach* is what fails. Only a pod reaching
  `Running` proves it.

  Worse, it is sticky: PVs provisioned while non-zonal carry
  `topology.disk.csi.azure.com/zone=` (empty), which can never match a zonal
  node, so fixing the node pool is not enough -- the PVCs must be recycled and
  the CNPG `Cluster` recreated.

**Recommendations.**
- `azure-provision.sh` should take `--zones` and **default the database pool to
  a zone**, because the overlay it exists to serve requires one.
- Add a pre-flight to `validate_arguments` that resolves the VM size against
  `az vm list-skus --location <loc> --size <sku>` and checks
  `restrictions == []` and `CpuArchitectureType == x64`. This makes `--dryRun`
  actually load-bearing and turns a four-minute partial failure into an
  immediate exit 2.
- Longer term the storage class is the coupling point and should be a declared
  instance value checked against the pool's zonality, not two independent
  defaults that happen to disagree.

---

## 6. GitOps needs a credential step the model has no place for

`installInstance` calls `argoSync(app: "memql")` as though the Application were
already registered. Registering it needed three cluster writes and one GitHub
write, none of them modelled:

- the AppProject must exist AND list the instance repo in `sourceRepos`, or the
  Application is refused as "repo not permitted in project" -- which stops
  reconciliation while looking harmless;
- a repository Secret whose `url` matches the Application's `repoURL` **byte for
  byte**, or Argo reports a `ComparisonError` that reads like a manifest problem;
- **the engine's app-of-apps cannot be applied on an instance cluster.**
  `deploy/argocd/apps/root.yaml` has `automated: {prune: true, selfHeal: true}`
  and an include glob containing `memql.yaml` -- the engine's OWN Application,
  named `memql`, pointing at the engine repo. Applying root on an instance
  cluster would continuously revert the instance's Application to the engine's.
  This is memql#4463's exact failure with the polarity reversed, and it is armed
  by default.
- credential type is an ORG-POLICY question, not a preference: deploy keys were
  disabled org-wide, so the ssh path was unavailable and the Application's
  `repoURL` had to change to https with a fine-grained token. A lifecycle
  automation must treat the transport as an input, not a constant.

---

## 7. What the capability-script contract got right

Worth saying plainly, because it is the part to keep:

- **`--print-spec` told me the required params without running anything.** That
  is exactly the memql#3568 argument, and it held.
- **Idempotency was real.** After the AKS failure, re-running skipped the
  resource group, registry, vault and storage account with `already exists` and
  resumed at the cluster. No second anything was created. The secret seeder
  likewise: `created: 0, kept: 9, changed: false`.
- **Exit codes were honest.** The AKS failure exited 5 with
  `ok:false, changed:true` and a message naming the operation -- `changed:true`
  being correct and load-bearing, since four resources HAD been created.
- **One JSON envelope on stdout, logs on stderr** made `tee`-ing the result and
  reading `esoClientId` / `dbClientId` out of it trivial -- which is the entire
  handoff to the overlay step.

The contract is not the problem. The problem is that only one step of eleven is
behind it.

---

## 8. Concrete backlog for `dsl/deployment/`

1. **`action installClusterOperators`** -- cert-manager, CNPG+Barman, ESO CRDs,
   ESO, ingress-nginx, ClusterIssuer, from pinned refs. Idempotent, verify-first.
2. **`action seedInstanceSecrets`** -- generate-if-absent (§2), never migrate,
   never log a value, DSN and CNPG creds from one password.
3. **`action registerGitOpsRepo`** -- AppProject `sourceRepos`, repository
   Secret, transport as an input (§6).
4. **`action wireExternalSecrets`** -- SecretStore + ExternalSecrets patched with
   `esoClientId` from provisioning's envelope, plus the `memql-secrets` shell (§4).
5. **Widen `provisionAzureInfrastructure`** with `--zones` and a VM-size
   pre-flight (§5).
6. **Rewrite `installInstance`** to own steps 1-11 with `argoSync` last, so
   `bringUpInstance` becomes true end-to-end rather than substrate-plus-a-wish.
7. **Give `repairInstance` an existence checklist**, not a health check -- the
   entire §3 class is invisible to health.
8. **Model the post-sync settle.** The first sync legitimately fails while the
   database is still initialising; mesh pods CrashLoopBackOff until it is up and
   then need a restart to clear backoff. A lifecycle step that treats the first
   `Degraded` as failure will be wrong most of the time.

## 9. One engine-side defect found

`cloud-entry` at `v0.19.6` holds voice off by converting `livekit-rtc` and
`livekit-sip` to ClusterIP, but `livekit-sip` alone also carries
`loadBalancerSourceRanges` (an `0.0.0.0/0` placeholder in
`base/livekit-sip.yaml`). It is forbidden on a ClusterIP exactly as
`externalTrafficPolicy` is, so **the very first sync failed and applied nothing**
-- all 54 objects stayed behind one invalid Service. Fixed in the instance repo;
belongs upstream, since any instance composing `cloud-entry` with voice off hits
it. Refs memql#4225.

## 10. Mail does not fail silently -- it fails UPWARD

The sharpest finding of the bring-up, and a different failure *class* from
everything above. The install-dependency class (§3) fails **silently**: a pod
hangs and says nothing. This one reports **success at every layer**.

With no mail credentials, `integrations/email`'s `NewSenderFromEnv` selected a
`LogSender`, which wrote the message to the pod log and returned `nil`. So:

```
email: no sender configured, using LogSender
email (log-only mode, not delivered)  to=...  subject="Claim ownership of MemQL"
```

...the setup wizard said the link was sent, and the audit row recorded
`magic_link_issued` with `outcome=success`. The only evidence was a human not
receiving mail, which is indistinguishable from a spam filter. On this
instance the cluster owner could not claim their own cluster and **nothing
anywhere said why**.

A failure that hangs eventually gets investigated. A failure that reports
success does not. A no-delivery mode is right for local development; on a
cloud install it converts "mail is unconfigured" -- a loud, fixable condition
-- into "the owner is confused about their inbox".

**Fixed in memql#4477.** Log-only is now refused on any install whose
`MEMQL_DOMAIN` is not a local name, at boot and at send, with the audit row
and the portal's integration report changed to match. `MEMQL_DOMAIN` was the
right discriminator because it is already the one seeded value every node
derives from, so the gate introduces no new fact to keep in sync. Break-glass:
`MEMQL_EMAIL_ALLOW_LOG_ONLY=true`.

**For the lifecycle work (§8, memql#4472 / memql#4473):** mail belongs in
`installInstance` rather than a follow-up step, and `repairInstance` should
assert the **sender**, not the send -- the boot line `email: using Microsoft
Graph sender` with a non-empty sender is the check. A send needs a recipient,
and a health check that mails a real person every time it runs is a worse
failure than not knowing.

**The break-glass recovery, worth recording either way.** Because `LogSender`
writes the whole message, a magic link issued in log-only mode is recoverable:

```bash
kubectl logs deploy/identity -n memql | grep -A5 "log-only mode"
```

A cluster is therefore never actually locked out by this. Two constraints: the
link expires in 10 minutes, and links are device-bound (memql#4300) -- it must
be opened in the browser that requested it, which holds the `memql_ml` cookie.

## 11. The mail app registration could send as any mailbox in the tenant

The security half of the same area. The Graph sender needs `Mail.Send`
(Application), whose Entra display name is literally **"Send mail as any
user"** -- and that is precisely what it grants: the app can send as any
mailbox in the tenant, not only the configured sender.

Narrowing it to one mailbox requires an Exchange `ApplicationAccessPolicy`,
which is **Exchange Online PowerShell and not reachable from `az`**. Nothing
in the documented bring-up path applied one, so unless an operator knew to do
it by hand, every instance stood up this way had a tenant-wide send capability
sitting in a Key Vault.

A destructive adjacency found the same day: **`az ad app credential reset`
without `--append` deletes every existing secret** on the app registration.
Adding a secret for a new cluster to a shared registration is therefore a
one-flag difference between additive and destructive -- and here the secret it
destroyed was labelled `memql-keep-it`.

**Fixed in memql#4478**, as documentation, because both halves are operator
actions on a tenant this repo has no access to: the `New-ApplicationAccessPolicy`
invocation with both-directions verification, the `--append` requirement, and a
new app registration per instance rather than one shared across instances --
which bounds a leaked secret to one instance and makes the wrong reset flag
cost nothing. See
[azure-entry-install.md](../../public/operate/azure-entry-install.md#mailsend-is-tenant-wide-until-you-scope-it).

> Numbering note: memql#4477 and memql#4478 cite these as §10 and §12 of an
> earlier draft of this write-up. They are §10 and §11 here.
