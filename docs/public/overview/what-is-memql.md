---
title: What is MemQL?
audience: public
status: stable
area: overview
sinceVersion: 0.20.0
owner: znas
---

# What is MemQL?

MemQL is an open-source AI platform for applications that need AI to act on
structured data and external systems. You declare data and behavior in `.memql`
files; the engine executes them on a cluster. You can inspect that work through
MemQL OS, VS Code, or your own client.

## The mental model

**The engine runs the system. MemQL OS is an application connected to it.**
Individual OS apps are views and controls for particular jobs, not separate
engines. The VS Code extension is another client, with offline language support
and an optional connection to one of your clusters.

```text
MemQL OS apps       VS Code        Your sites and applications
      \                |                 /
             Authenticated engine API
                        |
  Data and relationships · Tools and automations · Agent work
                        |
       Versioned memory graph and execution records
```

## What the engine supports

| Capability | What you can do | Start here |
|---|---|---|
| Typed data and history | Declare concepts, validate fields, query rows, and retain versions and relationships | [First program](../language/first-program.md), [versioning](../concepts/concept-versioning.md) |
| Reusable behavior | Declare queries, mutations, logic, tools, prompts, and event/schedule automations in the same language | [Language](../language/memql.md), [specifications](../language/specifications.md) |
| AI routing | Route a call's required level through policies and rules to compatible configured sources, including local models | [Routing](../operate/ai-routing.md), [local models](../operate/local-models.md) |
| Durable agent work | Inspect goals, runs, steps, approvals, budgets, and recovery records | [Harness](why-memql-harness.md), [measured results](proving-scorecard.md) |
| Files and knowledge | Store versioned artifacts, train from Library files, and retrieve knowledge | [Library](../operate/library.md), [document history](../concepts/document-version-history.md) |
| External systems | Connect integrations; distinguish mirrored, original, and native data; deliver changes through an outbox | [Data origins](../concepts/data-origins.md), [outbound delivery](../operate/outbound-delivery.md) |
| Sites and applications | Serve static bundles by hostname; manage sources, builds, and deployments | [Hosting](../operate/site-hosting.md), [Deployables](../operate/deployables.md) |
| Communication | Use the implemented audio stream and campaign sending surfaces when their providers and delivery configuration are ready | [Audio](../build/audio-streaming.md), [campaigns](../operate/campaign-sending.md) |
| Access and operations | Authenticate users, grant app access, inspect logs, and manage the cluster | [Access model](../operate/auth/access-model.md), [MemQL OS](../operate/memql-os.md) |

A capability being implemented does not mean a new cluster has it configured.
Model calls need a compatible inference source; mail delivery needs a sender;
external integrations need their own configuration and permissions. Use
[configuration readiness](../operate/configuration-readiness.md) to see what is
missing.

## The memory graph

MemQL is built on a time-series memory graph, backed by PostgreSQL, TimescaleDB,
and pgvector. Concepts describe record types. Queries select data; mutations
write versions. Relationships connect records. Authorization depends on the
declared concept tier and the operation: do not assume every concept is private
by default. [Learn the access model](../operate/auth/access-model.md).

The agent harness uses that same system for its work records. It is built into
the platform's work lifecycle, not a pack you can switch off. Its evidence is
scoped to the tests and runs in the [proving scorecard](proving-scorecard.md);
a replay result is not a guarantee about every external side effect.

## Modules, clients, and apps

- **Modules** describe what an operator can inspect in the engine: components,
  integrations, packs, and node types. Only packs have the pack enable/disable
  switch, and changing it takes effect at node restart. [Modules](../concepts/modules.md).
- **Clients** connect to the engine. MemQL OS is the browser client included in
  this repository; product-specific clients live in their own repositories.
  [Clients](../concepts/clients.md).
- **OS apps** organize a person's work inside that client. Fleet, Files,
  Deployables, Nexus, and Concepts share a session and cluster connection.
  [Find the right app](../operate/memql-os.md#the-apps).

The approved OS design direction is
[Supervised Visual Composition](../operate/supervised-visual-composition.md).
That page distinguishes the current interface, Fleet work under local validation,
and designs that have not shipped.

## What to expect today

MemQL is alpha and pre-1.0. APIs and language syntax can change between releases.
The local runtime is a Docker-backed k3d cluster managed through ArgoCD; a cloud
installation follows the same GitOps topology. It is a multi-service system,
not a single editor process. Offline editing is available before installing it.

[Get started](quickstart.md), or choose a route from the
[documentation home](index.md). See [status and direction](roadmap.md) for the
boundary between implemented capabilities and upcoming work.
