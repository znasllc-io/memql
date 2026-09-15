---
title: Getting started with MemQL
audience: public
status: stable
area: overview
sinceVersion: 0.20.0
owner: znas
---

# Getting started with MemQL

Start by editing a small program, connect to an existing cluster, or install a
local cluster. These are separate steps: the editor works offline; executing
constructs needs the engine.

## Try the language first

Install [MemQL for VS Code](../language/vscode.md#get-the-extension) and open the
[reading-list example](../../../examples/reading-list/reading.memql). Completion,
hover, and diagnostics work without credentials or a cluster. Follow
[Your first MemQL program](../language/first-program.md) to understand the code
and validate it with the command-line linter.

## Connect to an existing cluster

1. Open a trusted workspace in VS Code.
2. Run **MemQL: Add Cluster** and enter the domain supplied by your operator.
3. Select the cluster and use **Sign In**. The extension discovers the endpoints
   and opens the cluster's sign-in flow.
4. Browse **Constructs** for definitions and **Data** for rows your account may
   read. Use **Open Console** to reach that cluster's MemQL OS.

You do not need Docker to connect to someone else's cluster. A personal access
token is not a substitute for the identity-issued access token used by the
editor's engine connection. See [editor authentication](../operate/auth/connecting-editors.md).

## Install a local cluster

The supported local runtime is **Docker + k3d + ArgoCD**. It includes the
engine services, storage, identity, and MemQL OS. Allow time for image downloads,
compilation, and readiness checks. Setup time depends on your host and network.

### Through the extension

Use the local-install option in **MemQL: Add Cluster**. The installer currently
supports **Linux x64 and Apple Silicon macOS**. Docker must already be installed
and running. Review the install plan and follow the ownership/passkey handoff.
See [install prerequisites](../operate/install-prerequisites.md) for what it
places on your machine.

Installing the extension and installing a cluster are different operations.
The extension's language server can run on additional packaged platforms; that
does not make those platforms supported by the local cluster installer.

### From a repository checkout

Use this route when developing the engine. Install Git, Make, Docker, k3d,
kubectl, and mkcert. Docker must be running. The local TLS setup uses mkcert's
trusted certificate authority and local hostname mappings; follow the
[local runtime guide](../operate/reproduce-the-cloud-locally.md) if these are not
already configured. CLI language tools use the Go toolchain declared in
`go.mod` (including its `toolchain` directive).

```bash
git clone https://github.com/znasllc-io/memql.git
cd memql
make up REVISION=main
make status
```

`make up` builds/imports local engine images, registers the local overlay with
ArgoCD, runs migrations, and waits for healthy deployments. `REVISION=main`
gives ArgoCD a revision it can fetch. If using a feature branch, push that
branch before asking ArgoCD to track it.

Already have a local cluster? Inspect it with `make status` and use its existing
sign-in. Do not recreate it to follow this tutorial.

## Open MemQL OS

For the default local domain, open **[os.memql.localhost](https://os.memql.localhost/)**.
Identity lives at `identity.memql.localhost`; the engine API lives at
`api.memql.localhost`. The normal connection uses these HTTPS front doors.
Port-forwards are for debugging.

A fresh installation needs its first owner. The installer provides an ownership
handoff; a checkout-based installation can use identity's `/setup` flow. Local
mail may be log-only, so use the supported
[sign-in and enrolment paths](../operate/auth/sign-in-paths.md) rather than
assuming an email has been delivered.

The OS can show the core setup gate when inference is explicitly unconfigured.
Configure a compatible source using the options offered, then follow the Set up
card. A local install does not require adding a paid-provider API key.
[First-run setup](../operate/first-run.md) explains the exact readiness states.

## Get your first result

Use the [reading-list tutorial](../language/first-program.md) to register a small
concept, add one row, and query it in VS Code. Then open **Concepts** in MemQL OS
to inspect the schema and rows. This exercises the engine without an AI call.
Other apps have their own setup requirements: Fleet for execution resources,
Files for artifacts, Deployables for sites, and Nexus for goals and approvals.

## Check a problem

From the checkout, these inspect the default cluster without changing it:

```bash
make status
kubectl get pods -n memql
kubectl logs -n memql deploy/identity --tail=100
kubectl get applications -n argocd
```

- **Browser certificate error:** inspect local DNS/mkcert setup in the
  [front-door guide](../operate/front-door.md). Do not make TLS bypass the normal path.
- **Sign-in fails:** check the domain and use the identity service for that
  cluster. [Sign-in paths](../operate/auth/sign-in-paths.md).
- **Pods are not ready:** check their events and the ArgoCD application's
  conditions. [Local runtime](../operate/reproduce-the-cloud-locally.md).
- **Model-dependent work cannot run:** inspect
  [configuration readiness](../operate/configuration-readiness.md) and
  [AI routing](../operate/ai-routing.md).

## Develop and continue

After engine changes, `make dev` rebuilds and rolls the local services;
`make dev NODE=bff` targets one node type. These change the running cluster.
Run `make test` for the workspace test suite. See
[CONTRIBUTING.md](../../../CONTRIBUTING.md) for focused checks and DB requirements.

For teardown or reset, read the [local runtime runbook](../operate/reproduce-the-cloud-locally.md)
first: removing the cluster can destroy its stored data.

[Documentation home](index.md) · [OS app guide](../operate/memql-os.md) ·
[Language reference](../language/memql.md).
