---
title: What Is MemQL
audience: public
status: stable
area: overview
sinceVersion: 0.20.0
owner: znas
---

# What Is MemQL

MemQL is an **open-source AI platform**: agents, automations, voice,
campaigns, and hosted sites, running as one deployable system on a
**time-series memory graph**. You declare behavior in the MemQL DSL --
concepts, queries, mutations, tools, prompts, automations -- and a mesh of
specialized nodes executes it, remembers it, and lets you inspect it.

It is a platform in the operational sense, not the marketing one: it is
the thing you install, and everything else is a module of it or a client
on it.

## The three-layer mental model

```
  clients          what you BUILD ON the platform
                   SPAs, websites, consoles, apps -- one repo per client
                   (the MemQL Portal is the worked example)
  ------------------------------------------------------------------
  modules          what the platform RUNS
                   components (engine internals) . integrations (talk to
                   other systems) . packs (product features, per-instance
                   enable/disable) . node-type modules (bff, voice,
                   cognition, agent, planner, workbench, mcp, edge)
  ------------------------------------------------------------------
  memory graph     what the platform REMEMBERS ON
                   PostgreSQL + TimescaleDB; append-only, versioned,
                   time-series nodes -- provenance and replay built in
```

- **The memory graph** is the substrate. MemQL is *built on* a
  time-series memory graph: every record carries its own history, a
  write adds a version rather than overwriting, and retrieval can blend
  semantic similarity with recency. (MemQL is not a database, and does
  not position itself as one -- the graph is the substrate the platform
  runs on, embedded and managed for you.)
- **Modules** are the platform's own capabilities. The
  [harness](why-memql-harness.md) -- the work spine that executes agent
  turns, enforces budgets, and consolidates memory -- is one module.
  Voice is another. Product features ship as packs, and an operator can
  enable or disable a pack per instance. See
  [Modules](../concepts/modules.md) for the full mental model.
- **Clients** are what you build: a SPA, a website, a mobile app
  (Android/iOS are planned, not shipped), a console. One repo per
  client, stamped from the `memql-project` template; the engine stays
  product-agnostic. See [Clients](../concepts/clients.md).

## What running it looks like

One installation shape everywhere: a k3d + ArgoCD cluster locally, the
same manifests reconciled on a cloud cluster. gRPC-first API on a single
multiplexed stream, with a WebSocket bridge for browsers. An in-house
identity service (magic link, passkeys, JWT/JWKS). Per-row authorization
classified and test-enforced. Cost and safety guardrails on by default.
The [Quickstart](quickstart.md) gets a local cluster up in five minutes;
the [Tech Stack](tech-stack.md) page states the opinionated choices.

## Where it stands

MemQL is Apache-2.0 licensed and developed in the open. It is pre-1.0:
the DSL, engine API, and wire surface still evolve, and the README's
status banner is the honest statement of maturity at any moment. A full
production product runs on MemQL today -- the platform is extracted from
real operation, not designed on a whiteboard -- but you should expect
breaking changes between releases until 1.0.

> Next: [Why MemQL ships a harness](why-memql-harness.md) -- the
> proof-driven tour of the module that runs agents, or the
> [Quickstart](quickstart.md) if you would rather run it first.
