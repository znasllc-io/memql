---
title: Infrastructure Management Guide
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Infrastructure Management Guide

MemQL and its downstream product stack run on **Azure Kubernetes Service**
(cluster `aks-<install>`, namespace `memql`), with a self-hosted
**CloudNativePG** database in-cluster, images in **ACR** (`acrmemql.azurecr.io`), bootstrap secrets arriving
as keys on the **memql-secrets** Secret (a plain Kubernetes Secret every node
`envFrom`s -- the earlier genesis envelope, sealed and decrypted in-process
at boot, is retired, memql#3963), and per-env config in **Key Vault**
(`kv-memql-<env>`).

The former Google Cloud Run / Cloud Build / Artifact Registry / Secret Manager
infrastructure is retired. To avoid the doc drift that retirement caused, this
guide is intentionally a pointer rather than a duplicate:

- **[Scaling and upgrading an instance](instance-scale-and-upgrade.md)** and
  the **[deployment console](deployment-console.md)** — the deploy and
  operations reference: scaling, version rolls, the upgrade checks, and the
  role-gated deploy-control actions.
- **[../../../deploy/k8s/base/README.md](../../../deploy/k8s/base/README.md)** — manifest-level reference
  (per-node Deployments, HA, migrations-run-once, apply order, validation).
- **[The cluster front door](front-door.md)** — ingress-nginx +
  cert-manager + public entry; internal TLS is the
  `deploy/k8s/components/internal-tls` component.
