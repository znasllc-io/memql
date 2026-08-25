# External Secrets ← Key Vault (deployment-v2 Phase 5, #703)

Retires the hand-edited / `kubectl patch secret` flow for the cluster-facing
secret material. **External Secrets Operator** reconciles `memql-secrets` from
`kv-<install>`, so the cluster's secret state is declarative + reconciled
(no operator drift) — the secrets analogue of what Phase 1/2 did for images.

```
   kv-<install> (Azure Key Vault)
      memql-master-key / memql-operator-key / memory-nodes-database-dsn
              │   (WorkloadIdentity: federated managed identity -> ESO SA)
              ▼
   ExternalSecret (external-secrets controller, refresh 1h)
              ▼
   Secret/memql-secrets (memql ns)  ── envFrom ──►  every pod
```

## What ESO delivers

ESO owns the **k8s Secret** every pod `envFrom`s. Two `ExternalSecret` objects
contribute to it, both `creationPolicy: Merge`, and consumers cannot tell —
a node reads `memql-secrets` and neither knows nor cares how many objects
assembled it.

| Object | Keys |
|---|---|
| `memql-secrets` | `MEMQL_MASTER_KEY`, `MEMQL_OPERATOR_KEY`, `MEMQL_DATABASE_DSN` |
| `memql-secrets-identity` (memql#3960) | `MEMQL_IDENTITY_SIGNING_KEY_B64`, `MEMQL_IDENTITY_SIGNING_KEY_CREATED_AT`, `MEMQL_NODE_BOOTSTRAP_TOKEN`, `MEMORY_NODES_DATABASE_DIRECT_DSN` |

**The split is load-bearing, not tidiness.** ESO fails a whole `ExternalSecret`
when any single `remoteRef` cannot be resolved. Adding the second group's keys
to the first object would mean one missing Key Vault entry stalls
`MEMQL_MASTER_KEY` and the DSN along with them; split, a missing entry stalls
only the keys that are actually missing.

**Why the second group exists at all.** `MEMQL_IDENTITY_SIGNING_KEY_B64` must
be byte-identical on every identity replica or JWKS diverges and roughly half
of all authentication fails (memql#3400). It was reaching the cloud *only* by
riding inside the sealed envelope, or by an operator hand-adding a key outside
ESO — which `creationPolicy: Merge` permits and nothing records. Declaring it
here is what makes deleting the envelope safe.

## There is no second delivery path

ESO owns the `memql-secrets` Secret, and that Secret is **the** way config
reaches a pod (epic memql#3958). Rotating a key is a Key Vault write; ESO
reconciles it to the cluster on the next refresh.

> There used to be a second path: `MEMQL_GENESIS_B64` carried a sealed A2
> envelope each pod decrypted in-process at boot under `MEMQL_MASTER_KEY`,
> applying ~150 vars set-if-absent. It is gone, along with `component/genesis/`,
> its sealing CLI and its `.znas` format. If a runbook tells you to re-seal an
> envelope and push the blob to Key Vault, it is describing a mechanism that no
> longer exists. What the envelope was genuinely load-bearing for -- getting a
> locked-out owner back in -- is now the recovery key (memql#3964).

## Every ExternalSecret needs `IgnoreExtraneous` (memql#4489)

ESO copies an ExternalSecret's **labels and annotations onto the target
Secret it writes**. ArgoCD identifies its own resources by the
`app.kubernetes.io/instance` label. Put those two facts together and an
Argo-tracked ExternalSecret hands its tracking label to a Secret that exists
in **no repository** — so Argo reports that Secret `OutOfSync`, forever, with
nothing to sync and nothing to fix. Observed live as `Secret/memql-secrets`
carrying `app.kubernetes.io/instance: memql`.

The fix turns the same inheritance against itself. Put the annotation on the
**ExternalSecret**:

```yaml
metadata:
  annotations:
    argocd.argoproj.io/compare-options: IgnoreExtraneous
```

Inheritance delivers it onto the generated Secret, where it tells Argo the
omission is deliberate. On the ExternalSecret itself it is a no-op — that
object *is* in the repository.

Three things to know before adding or editing one:

- **It goes on every ExternalSecret, not just the ones that have caused
  trouble.** Any one without it re-claims its Secret the next time it
  reconciles.
- **Where several ExternalSecrets merge into one Secret** — `memql-secrets`
  has two here and an instance repo may add a third — *all* of them need it.
  One without is enough to lose the annotation for the Secret they share.
- **The symptom is a permanently `OutOfSync` Application**, which is the same
  class of defect as memql#4487: health that is always wrong stops being read,
  and an operator who learns the red is normal will not see a real one.

`deploy/k8s/overlays/externalsecrets_test.go` walks every ExternalSecret under
`deploy/` and fails the build on one without the annotation. It is text-level,
so it cannot skip for want of a renderer.

**Confirm the inheritance on a live cluster** — the gate proves the annotation
is on the ExternalSecret; only the cluster proves ESO carried it across:

```bash
# 1. the annotation is where we put it
kubectl -n memql get externalsecret memql-secrets \
  -o jsonpath='{.metadata.annotations.argocd\.argoproj\.io/compare-options}{"\n"}'

# 2. force a reconcile, then check it landed on the SECRET
kubectl -n memql annotate externalsecret memql-secrets force-sync=$(date +%s) --overwrite
kubectl -n memql get secret memql-secrets \
  -o jsonpath='{.metadata.annotations.argocd\.argoproj\.io/compare-options}{"\n"}'

# 3. and that Argo has stopped claiming it
argocd app get memql --refresh          # no OutOfSync entry for Secret/memql-secrets
```

Step 2 returning empty while step 1 returns `IgnoreExtraneous` is the one
outcome that would mean this mechanism does not hold on your ESO version. The
repair is then explicit rather than inherited — declare the annotation under
`spec.target.template.metadata.annotations`, which ESO applies to the generated
Secret directly. Do not reach for that first: a `template` block interacts with
`creationPolicy: Merge`, and the inherited form is what is verified here.

## Files

| Path | What |
|---|---|
| `install/` | ESO v2.5.0 (Helm-rendered, pinned) controller + webhook + the cert-manager Issuer/Certificate for the webhook cert, in `external-secrets` ns. CRDs are managed out-of-band (already installed, serving `external-secrets.io/v1`). Additive. See `install/eso-values.yaml` for the render recipe. |
| `secretstore.yaml` | `SecretStore` → `kv-<install>` via AKS **Workload Identity** (secret-less auth). |
| `externalsecret-memql.yaml` | The `external-secrets-kv` ServiceAccount + the `ExternalSecret` mapping the 3 Key Vault entries → `memql-secrets`. |

## Key Vault auth (Workload Identity — one-time)

```bash
# 1. user-assigned managed identity + federated credential for the ESO SA:
az identity create -g rg-<install> -n id-eso-memql
CLIENT_ID="$(az identity show -g rg-<install> -n id-eso-memql --query clientId -o tsv)"
OIDC="$(az aks show -g rg-<install> -n aks-<install> --query oidcIssuerProfile.issuerUrl -o tsv)"
az identity federated-credential create -g rg-<install> \
  --identity-name id-eso-memql --name eso-memql \
  --issuer "$OIDC" --subject system:serviceaccount:memql:external-secrets-kv \
  --audiences api://AzureADTokenExchange
# 2. grant it read on the vault's secrets:
az role assignment create --assignee "$CLIENT_ID" --role "Key Vault Secrets User" \
  --scope "$(az keyvault show -n kv-<install> --query id -o tsv)"
# 3. put the client id on the SA annotation (externalsecret-memql.yaml).
```

> **RESOLVED — webhook cert on Kubernetes 1.34 (memql#738):** ESO's *bundled*
> cert-controller never populated the `external-secrets-webhook` TLS cert on k8s
> 1.34 (the upstream release bundle is also templated for the `default` namespace,
> so its `--service-namespace=default` / `--dns-name=…default.svc` args pointed the
> cert plumbing at the wrong namespace). Fixed by rendering ESO from the **Helm
> chart** with `certController.create=false` + `webhook.certManager.enabled=true`,
> so **cert-manager** (already installed here) issues + rotates the webhook serving
> cert and its CA injector wires the `caBundle` onto the webhook configs. The
> render + a self-signed Issuer live in `install/` (see `install/eso-values.yaml`).
> The SecretStore also needs an explicit `tenantId` for the Azure provider under
> WorkloadIdentity (added to `secretstore.yaml`). With the webhook healthy the
> ExternalSecret reconciles as a **verified no-op** — Key Vault already equals the
> live Secret (#734), confirmed by sha256 before/after.

## Install + migrate (SUPERVISED — touches the live Secret)

```bash
# 0. Ensure the 3 Key Vault entries exist:
az keyvault secret set --vault-name kv-<install> --name memql-master-key --value "$MEMQL_MASTER_KEY"
az keyvault secret set --vault-name kv-<install> --name memql-operator-key --value "$(openssl rand -hex 32)"  # memql#3519: NOT the master key
az keyvault secret set --vault-name kv-<install> --name memory-nodes-database-dsn --value "$DSN"

# 1. Install ESO (additive — doesn't touch memql-secrets yet). cert-manager
#    issues the webhook cert, so the webhook is the readiness signal:
kubectl apply -k deploy/external-secrets/install
kubectl -n external-secrets rollout status deploy/external-secrets-webhook
kubectl -n external-secrets get certificate external-secrets-webhook   # -> READY True

# 2. Apply the SecretStore + ExternalSecret (creationPolicy: Merge -> it manages
#    the 3 keys without clobbering the existing Secret):
kubectl apply -f deploy/external-secrets/secretstore.yaml
kubectl apply -f deploy/external-secrets/externalsecret-memql.yaml
kubectl -n memql get externalsecret memql-secrets -w   # -> SecretSynced

# 3. Verify the synced values match (sha256 before/after; KV == live, so a no-op).
#    Pods pick up changes on their next restart (envFrom is injected at pod start);
#    do NOT roll if the values were unchanged. Going forward, rotating a shared
#    secret is a Key Vault write (as in step 0) followed by an ESO refresh
#    (reconciles within the hour, or force it with `kubectl annotate
#    externalsecret memql-secrets force-sync=$(date +%s) --overwrite`), then a
#    manual rollout restart of every pod that envFroms memql-secrets so the new
#    value actually lands in a running process (#734).
```

Manage these manifests via the Argo CD app-of-apps (Phase 2) so the secret
*wiring* is reconciled too (the secret *values* never enter git — only the
Key-Vault references do).

## DR

Secret recovery + DB point-in-time recovery + the rollback rehearsal are in
[../../docs/internal/ops/dr-runbook.md](../../docs/internal/ops/dr-runbook.md).
