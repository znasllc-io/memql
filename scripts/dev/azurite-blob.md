# Local Azure Blob Storage via Azurite (memql#806)

The local k3d cluster runs [Azurite](https://learn.microsoft.com/azure/storage/common/storage-use-azurite) -- the official Azure Storage emulator -- as the in-cluster `azurite` Deployment so attachment uploads produce real, downloadable blob URLs instead of `local://` placeholders.

## How it works

### Azurite Deployment (`deploy/k8s/overlays/local/azurite.yaml`)

The local overlay adds an `azurite` Deployment + Service running the `mcr.microsoft.com/azure-storage/azurite` image in blob-only mode (`azurite-blob --blobHost 0.0.0.0 --blobPort 10000`). It is reachable in-cluster at `http://azurite:10000` and on the host via the k3d port-forward (`localhost:10000`).

### Connection string wiring (`scripts/k3d/seed-secrets.sh`)

`make up` (via `seed-secrets.sh`) seeds `AZURE_BLOB_CONNECTION_STRING` into the `memql-secrets` k8s Secret using the well-known Azurite dev constants:

```
DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://azurite:10000/devstoreaccount1;
```

The connection string uses the in-cluster hostname `azurite` (not `127.0.0.1`) so pod-to-pod traffic stays inside the cluster. From the host, the endpoint is `http://127.0.0.1:10000/devstoreaccount1` (via the port-forward).

The account name (`devstoreaccount1`) and account key are the [well-known Azurite dev constants](https://learn.microsoft.com/azure/storage/common/storage-use-azurite#well-known-storage-account-and-key). They are not secrets.

### Blob URL format

Uploaded blobs get URLs of the form:

```
http://azurite:10000/devstoreaccount1/attachments/<objectName>
```

The attachment download endpoint (`GET /spaces/:id/attachments/:attachmentId`) proxies the download via `azureblob.DownloadURL`, which parses the container and object name from this URL and fetches the bytes. The `ParseBlobObject` helper in `integrations/azureblob/uploader.go` only accepts `https://` scheme URLs for production; for the local Azurite path the download goes through the client's `DownloadStream` against the same connection string.

## Inspecting blobs

Use the [Azure Storage Explorer](https://azure.microsoft.com/products/storage/storage-explorer) or `az storage` CLI pointed at the Azurite endpoint (via the host port-forward):

```bash
az storage container list \
  --connection-string "DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;"
```

## Cloud / staging deploys

Cloud environments override the connection string via the genesis envelope (sealed in `~/.memql/genesis.znas`, seeded into k8s Secrets). The local Azurite constants only apply to the local overlay, so there is no risk of the dev constants leaking to staging or production.
