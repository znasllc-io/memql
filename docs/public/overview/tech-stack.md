---
title: MemQL tech stack
audience: public
status: stable
area: overview
sinceVersion: 0.20.0
owner: znas
---

# MemQL tech stack

MemQL runs as specialized services in one cluster. Clients connect to the
engine; they do not connect directly to its storage.

| Layer | Implementation | What it means for you |
|---|---|---|
| Engine | Go; module minimum and selected toolchain are declared in `go.mod` | Build with the repository's toolchain rather than a guessed version |
| Data | PostgreSQL 16, TimescaleDB, pgvector, managed by CloudNativePG | Versioned records, time-oriented retrieval, and vector support underneath the platform |
| Engine API | gRPC `MemqlService.Stream`; WebSocket bridge for browsers | Use the Go or TypeScript SDK for typed operations |
| HTTP edge | Hostname-based static site serving, runtime configuration, health and identity endpoints | HTTP complements the engine API; there is no general REST CRUD API to substitute for it |
| Language | `.memql` declarations, compiler, registries, and MemQL Sense | The editor and engine share language intelligence |
| Identity | Cluster-owned identity service, OAuth/PKCE, passkeys, magic links, JWT/JWKS | Sign in to the cluster that owns the resource |
| AI | Level/policy/rule routing across configured sources | Local resources and vendor integrations have explicit readiness and permission requirements |
| Browser client | MemQL OS: React, TypeScript, Vite | A static SPA served by the edge as an ordinary site |
| Editor | TypeScript VS Code extension and bundled Go language server | Offline authoring; optional authenticated runtime connection |
| Deployment | Docker images, Kubernetes/Kustomize, ArgoCD | k3d locally; the supported cloud overlay targets AKS |

## Local and cloud operation

Local Kubernetes runs inside Docker through k3d. ArgoCD reconciles the cluster's
manifests; the front door uses HTTPS hostnames such as `os.memql.localhost` and
`api.memql.localhost`. Debug port-forwards are optional, not the regular client
connection. The initial local build uses `make up`; the engine development loop
uses `make dev`.

Cloud installations use the same base manifests with environment configuration
and pinned images. GitOps changes update the deployed revision; do not replace
this workflow with an ad hoc application server or direct production rollout.
The same topology does not imply identical hardware capacity or performance.

- [Getting started](quickstart.md)
- [Environment parity](../operate/environment-parity.md)
- [Database platform](../operate/database-platform.md)
- [Minimum requirements](../operate/minimum-requirements.md)
- [Build tags and node types](../build/build-tags.md)

## Build a client

Use [the SDKs and client model](../concepts/clients.md). A site build emits a
folder of static files with `index.html` at its root; the edge serves it without
a Node.js runtime. MemQL OS is a worked example. A product's client can also be
hosted elsewhere and connect through the supported API.

[Site hosting](../operate/site-hosting.md) ·
[TypeScript SDK](../../../sdk/ts/README.md) · [Go SDK](../../../sdk/go) ·
[Contributing](../../../CONTRIBUTING.md).
