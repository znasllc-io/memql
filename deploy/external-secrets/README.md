# External Secrets ← Key Vault (deployment-v2 Phase 5, #703)

Retires the hand-edited / `kubectl patch secret` flow for the cluster-facing
secret material. **External Secrets Operator** reconciles `memql-secrets` from
`kv-memql-staging`, so the cluster's secret state is declarative + reconciled
(no operator drift) — the secrets analogue of what Phase 1/2 did for images.

```
   kv-memql-staging (Azure Key Vault)
      memql-master-key / memql-genesis-b64 / memory-nodes-database-dsn
              │   (WorkloadIdentity: federated managed identity -> ESO SA)
              ▼
   ExternalSecret (external-secrets controller, refresh 1h)
              ▼
   Secret/memql-secrets (memql ns)  ── envFrom ──►  every pod
```

## The genesis-envelope boundary (unchanged)

ESO owns the **k8s Secret** that carries `MEMQL_MASTER_KEY`, `MEMQL_GENESIS_B64`
(the sealed A2 envelope), and `MEMORY_NODES_DATABASE_DSN`. It does **not** change
the genesis envelope itself: the A2 envelope stays the **app-internal**
shared-secret bootstrap (`component/secret/`, `component/genesis/`), autoloaded
at boot. To rotate a shared secret you still re-seal the envelope (DEPLOYMENT_STRATEGY
§4) and push the new blob to **Key Vault** — ESO then propagates it to the
cluster Secret on the next refresh, replacing the manual `kubectl patch` step.

## Files

| Path | What |
|---|---|
| `install/` | ESO v2.5.0 (Helm-rendered, pinned) controller + webhook + the cert-manager Issuer/Certificate for the webhook cert, in `external-secrets` ns. CRDs are managed out-of-band (already installed, serving `external-secrets.io/v1`). Additive. See `install/eso-values.yaml` for the render recipe. |
| `secretstore.yaml` | `SecretStore` → `kv-memql-staging` via AKS **Workload Identity** (secret-less auth). |
| `externalsecret-memql.yaml` | The `external-secrets-kv` ServiceAccount + the `ExternalSecret` mapping the 3 Key Vault entries → `memql-secrets`. |

## Key Vault auth (Workload Identity — one-time)

```bash
# 1. user-assigned managed identity + federated credential for the ESO SA:
az identity create -g rg-memql-staging -n id-eso-memql
CLIENT_ID="$(az identity show -g rg-memql-staging -n id-eso-memql --query clientId -o tsv)"
OIDC="$(az aks show -g rg-memql-staging -n aks-memql-staging --query oidcIssuerProfile.issuerUrl -o tsv)"
az identity federated-credential create -g rg-memql-staging \
  --identity-name id-eso-memql --name eso-memql \
  --issuer "$OIDC" --subject system:serviceaccount:memql:external-secrets-kv \
  --audiences api://AzureADTokenExchange
# 2. grant it read on the vault's secrets:
az role assignment create --assignee "$CLIENT_ID" --role "Key Vault Secrets User" \
  --scope "$(az keyvault show -n kv-memql-staging --query id -o tsv)"
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
# 0. Ensure the 3 Key Vault entries exist (memql-genesis-b64 already does, §4):
az keyvault secret set --vault-name kv-memql-staging --name memql-master-key --value "$MEMQL_MASTER_KEY"
az keyvault secret set --vault-name kv-memql-staging --name memory-nodes-database-dsn --value "$DSN"

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
#    do NOT roll if the values were unchanged. Going forward, shared-secret
#    rotation runs through scripts/secrets/reseal-genesis.sh (DEPLOYMENT_STRATEGY
#    §4), which writes Key Vault, drives the ESO refresh, verifies convergence, and
#    rolls — so Key Vault and the cluster Secret can never silently diverge (#734).
```

Manage these manifests via the Argo CD app-of-apps (Phase 2) so the secret
*wiring* is reconciled too (the secret *values* never enter git — only the
Key-Vault references do).

## DR

Secret recovery + DB point-in-time recovery + the rollback rehearsal are in
[../../docs/ops/dr-runbook.md](../../docs/ops/dr-runbook.md).
