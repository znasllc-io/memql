# Infrastructure Management Guide

memQL + CoPresent run on **Azure Kubernetes Service** (cluster
`aks-memql-staging`, namespace `memql`), with a managed **Tiger Cloud**
database, images in **ACR** (`acrmemql.azurecr.io`), secrets in the **genesis
A2** sealed envelope, and per-env config in **Key Vault** (`kv-memql-<env>`).

The former Google Cloud Run / Cloud Build / Artifact Registry / Secret Manager
infrastructure is retired. To avoid the doc drift that retirement caused, this
guide is intentionally a pointer rather than a duplicate:

- **[DEPLOYMENT_STRATEGY.md](DEPLOYMENT_STRATEGY.md)** — authoritative deploy +
  operations reference: topology, deploy flow, config precedence, secrets/
  re-seal, identity HA, the promotion gate, deep smoke, zero-downtime, recovery,
  and capacity.
- **[deploy/k8s/README.md](deploy/k8s/README.md)** — manifest-level reference
  (per-node Deployments, HA, migrations-run-once, apply order, validation).
- **[deploy/k8s/README-public-entry.md](deploy/k8s/README-public-entry.md)** —
  ingress-nginx + cert-manager + internal TLS / public entry.
