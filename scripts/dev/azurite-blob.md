# Local Azure Blob Storage via Azurite (memql#806)

`make dev-refresh` now starts [Azurite](https://learn.microsoft.com/azure/storage/common/storage-use-azurite) — the official Azure Storage emulator — as a compose service (`memql-azurite`) and automatically creates the `attachments` container. The agent and workbench nodes use the Azurite endpoint instead of the `local://` placeholder, so attachment uploads produce real, downloadable blob URLs.

## How it works

### Compose service (`docker/docker-compose.cluster.yml`)

The `azurite` service runs the `mcr.microsoft.com/azure-storage/azurite` image in blob-only mode:

```
azurite-blob --blobHost 0.0.0.0 --blobPort 10000
```

Blob data persists in the `cluster_azurite_data` named volume across `dev-refresh` runs (only `down -v` clears it).

### Agent and workbench env wiring

Both the `agent` and `workbench` compose services have these env defaults baked in (they can be overridden by the genesis env file for cloud/staging deploys):

```
MEMQL_AZURE_STORAGE_CONNECTION_STRING=DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://azurite:10000/devstoreaccount1;
MEMQL_AZURE_BLOB_CONTAINER=attachments
```

The connection string uses the in-network hostname `azurite` (not `127.0.0.1`) so container-to-container traffic stays on the Docker bridge. From the host, the endpoint is `http://127.0.0.1:10000/devstoreaccount1`.

The account name (`devstoreaccount1`) and account key are the [well-known Azurite dev constants](https://learn.microsoft.com/azure/storage/common/storage-use-azurite#well-known-storage-account-and-key). They are not secrets.

### dev-refresh step 4d

After the compose stack is up (step 3) and node tokens are minted (steps 4, 4b, 4c), step 4d calls `lib_setup_blob` from `scripts/dev/lib.sh`:

1. Polls `http://127.0.0.1:10000/devstoreaccount1?comp=list` until Azurite responds (up to 60 s).
2. Issues `PUT /devstoreaccount1/attachments?restype=container` via `curl`.
3. HTTP 201 = created; HTTP 409 = already exists (idempotent, OK); anything else = non-fatal warning.

Re-running `make dev-refresh` is safe — the 409 path handles the "already exists" case.

### Blob URL format

Uploaded blobs get URLs of the form:

```
http://azurite:10000/devstoreaccount1/attachments/<objectName>
```

The attachment download endpoint (`GET /spaces/:id/attachments/:attachmentId`) proxies the download via `azureblob.DownloadURL`, which parses the container and object name from this URL and fetches the bytes. The `ParseBlobObject` helper in `integrations/azureblob/uploader.go` only accepts `https://` scheme URLs for production; for the local Azurite path the download goes through the client's `DownloadStream` against the same connection string.

## Changing the container name

Override `MEMQL_AZURE_BLOB_CONTAINER` before running `dev-refresh` (or add it to `~/Downloads/local.genesis.env` and reseal):

```bash
export MEMQL_AZURE_BLOB_CONTAINER=my-container
make dev-refresh
```

## Inspecting blobs

Use the [Azure Storage Explorer](https://azure.microsoft.com/products/storage/storage-explorer) or `az storage` CLI pointed at the Azurite endpoint:

```bash
az storage container list \
  --connection-string "DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;"
```

## Cloud / staging deploys

Cloud environments override both env vars via the genesis envelope (sealed in `~/.memql/genesis.znas`). The compose defaults only apply when the variable is not already set by the genesis env file, so there is no risk of the dev constants leaking to staging or production.
