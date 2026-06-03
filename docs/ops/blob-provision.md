# Azure Blob Storage: staging provision + genesis wiring runbook

Runbook for provisioning the dedicated Azure Blob Storage account for the
**staging** environment and wiring the connection string to the agent node
via the genesis envelope. Part of [#807](https://github.com/znasllc-io/memql/issues/807),
epic [#805](https://github.com/znasllc-io/memql/issues/805).

**Decision (locked, #805):** separate storage account per environment. Staging
and production each own one account; local dev uses Azurite. The staging
connection string lives in `~/Downloads/staging.genesis.env` ONLY and must
never be added to `local.genesis.env` or any shared-dev file.

---

## Why this matters

Without a real Azure Blob backend the agent node degrades to writing
deliverables to the `emptyDir`-backed workbench directory. When the pod
reschedules (rolling deploy, node drain, HPA scale-down) that directory is
gone. This runbook wires `MEMQL_AZURE_STORAGE_CONNECTION_STRING` +
`MEMQL_AZURE_BLOB_CONTAINER` into the agent via the genesis envelope so every
`fs_write` / workbench deliverable produces a durable blob URL that survives a
pod restart.

---

## Prerequisites

- `az login` session with `Contributor` on `rg-memql-staging` (or at minimum
  `Storage Account Contributor` on the account + `Blob Data Contributor` on
  the container).
- `make genesis-seal` is available (the `memql` binary or `go run ./cmd/genesis-seal`).
- `MEMQL_MASTER_KEY` set in your shell (needed to reseal the envelope).
- `kubectl` context pointing at `aks-memql-staging` with `kubectl` access to
  the `memql` namespace.

---

## Step 1 -- Run the provision script

```bash
make blob-provision ENV=staging
# or directly:
bash scripts/deploy/blob-provision.sh --env=staging
```

Dry-run first if you want to preview without touching Azure:

```bash
make blob-provision ENV=staging DRY_RUN=1
```

The script creates (idempotently):

| Resource | Name | Notes |
|----------|------|-------|
| Storage account | `stmemqlstaging` | Standard\_LRS, StorageV2, East US, no public blob access, TLS 1.2+ |
| Blob container | `attachments` | Private (no anonymous reads) |

On success the script prints the full connection string to the terminal
(labeled "STAGING BLOB CONNECTION STRING"). Treat it as a secret -- it
contains the storage account key.

---

## Step 2 -- Add vars to staging.genesis.env

Append to `~/Downloads/staging.genesis.env`:

```
MEMQL_AZURE_STORAGE_CONNECTION_STRING=<paste connection string from step 1>
MEMQL_AZURE_BLOB_CONTAINER=attachments
```

Rules:
- These vars go into `staging.genesis.env`, NOT `local.genesis.env`.
  Local dev uses Azurite (`MEMQL_AZURE_STORAGE_CONNECTION_STRING=UseDevelopmentStorage=true`
  is set inside the Docker stack and is NOT stored in the genesis envelope).
- If you need the connection string again without re-provisioning, run:
  `az storage account show-connection-string --name stmemqlstaging --resource-group rg-memql-staging --query connectionString -o tsv`

---

## Step 3 -- Reseal the genesis envelope

```bash
make genesis-seal ENV_FILE=~/Downloads/staging.genesis.env
```

This reads the plaintext env file, validates it against the manifest,
encrypts it under `MEMQL_MASTER_KEY`, and writes `~/.memql/genesis.znas`.

Verify the seal completed:

```bash
# Should print the key list without any error; value is the encrypted blob.
ls -la ~/.memql/genesis.znas
```

---

## Step 4 -- Store genesis-b64 in Key Vault + k8s secret

The sealed envelope (`~/.memql/genesis.znas`) is safe to store (it is
encrypted). It must be updated in two places so the cluster picks it up:

### 4a. Key Vault (ESO reconciliation path)

```bash
az keyvault secret set \
  --vault-name kv-memql-staging \
  --name memql-genesis-b64 \
  --value "$(base64 < ~/.memql/genesis.znas)" \
  --only-show-errors \
  --output none
```

ESO refreshes the `memql-secrets` k8s Secret from Key Vault on its 1h
interval (see `deploy/external-secrets/externalsecret-memql.yaml`). You can
force a faster reconciliation in the next step.

### 4b. k8s Secret (direct update for immediate effect)

```bash
kubectl create secret generic memql-secrets \
  -n memql \
  --from-literal=MEMQL_GENESIS_B64="$(base64 < ~/.memql/genesis.znas)" \
  --dry-run=client -o yaml | kubectl apply -f -
```

Verify the secret was updated:

```bash
kubectl get secret memql-secrets -n memql -o jsonpath='{.data.MEMQL_GENESIS_B64}' \
  | base64 -d | wc -c
# Should print a non-zero byte count matching the sealed envelope size.
```

---

## Step 5 -- Roll agent pods

The agent node reads the genesis envelope at startup (`MEMQL_GENESIS_AUTOLOAD=true`
is set in `deploy/k8s/base/agent.yaml`). A rolling restart picks up the new
envelope:

```bash
kubectl rollout restart deployment/agent -n memql
kubectl rollout status  deployment/agent -n memql --timeout=120s
```

Other node types do NOT need a restart for this change -- `MEMQL_AZURE_STORAGE_CONNECTION_STRING`
and `MEMQL_AZURE_BLOB_CONTAINER` are consumed only by the agent binary
(`app/transport_agent.go`).

---

## Step 6 -- Verify durable uploads

After the rollout completes, trigger an attachment upload through the staging
frontend (or via the API) and confirm the blob URL is returned.

### 6a. Confirm the agent sees the vars

```bash
kubectl exec -n memql \
  "$(kubectl get pod -n memql -l app.kubernetes.io/name=agent -o jsonpath='{.items[0].metadata.name}')" \
  -- env | grep MEMQL_AZURE
```

Expected output (values redacted):
```
MEMQL_AZURE_STORAGE_CONNECTION_STRING=DefaultEndpointsProtocol=https;AccountName=stmemqlstaging;...
MEMQL_AZURE_BLOB_CONTAINER=attachments
```

### 6b. Trigger an upload and check the blob URL

Upload a file via the staging frontend (attach a file in a space), then check
the returned attachment URL. It should be of the form:
```
https://stmemqlstaging.blob.core.windows.net/attachments/<planId>/<objectName>
```

### 6c. Verify durability across a pod restart

```bash
# 1. Trigger an upload; note the blob URL.
# 2. Delete the agent pod (simulates a reschedule):
kubectl delete pod -n memql -l app.kubernetes.io/name=agent
# 3. Wait for the replacement pod to be ready:
kubectl rollout status deployment/agent -n memql --timeout=60s
# 4. Download the attachment again through the staging API.
#    The file must still be accessible (not 404 / degraded to local://).
```

A successful test here confirms durability vs the old `emptyDir`-only loss.

---

## Troubleshooting

### "MEMQL_AZURE_STORAGE_CONNECTION_STRING is not set"

The agent pod did not pick up the new genesis envelope. Verify:
- Step 4b updated `memql-secrets` (check with `kubectl get secret`).
- Step 5 completed the rolling restart.
- The pod logs show `[genesis] autoload: applied N vars` with N > 0 at startup.

### "az: not logged in"

Run `az login` before executing the provision script or the Key Vault commands.

### Connection string key rotation

If the storage account key is rotated (security incident, periodic rotation),
re-run the provision script to get the new connection string:

```bash
bash scripts/deploy/blob-provision.sh --env=staging
```

Then repeat Steps 2-5 with the new connection string. The script is idempotent --
it will find the existing account and container and only print the current key.

---

## Env-specificity guard

| Environment | Storage backend | Where the conn string lives |
|-------------|----------------|-----------------------------|
| Local dev | Azurite (`UseDevelopmentStorage=true`) | Docker compose env var; NOT in any genesis envelope |
| Staging | `stmemqlstaging.blob.core.windows.net` | `~/Downloads/staging.genesis.env` only |
| Production | `stmemqlproduction.blob.core.windows.net` (when provisioned) | `~/Downloads/production.genesis.env` only |

`load-staging-secrets.sh` (root of the projects tree) reads from `~/Downloads/local.genesis.env`
for the shared API keys. It does NOT reference `MEMQL_AZURE_STORAGE_CONNECTION_STRING` --
that var is intentionally absent from `local.genesis.env` so it cannot accidentally
reach the local Docker stack or be forwarded to the cluster via `make deploy-setup`.

The provision script (`scripts/deploy/blob-provision.sh`) enforces this by printing
instructions that always reference `~/Downloads/${ENV}.genesis.env`, never the shared
local file.

---

## Production (deferred)

The production account (`stmemqlproduction` in `rg-memql-production`) is
provisioned by the same script with `--env=production`. That work is deferred
until the production cutover. When it runs, replace every `staging` with
`production` in this runbook, use `~/Downloads/production.genesis.env`, and
roll `deployment/agent -n memql` in the production AKS cluster.
