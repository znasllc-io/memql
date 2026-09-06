---
title: Clients
audience: public
status: stable
area: concepts
sinceVersion: 0.20.0
owner: znas
---

# Clients

A **client** is a surface built ON the platform: an application a person
or another system points at a MemQL cluster. A single-page app, a
website, a landing page, a console, a kiosk -- and, on the roadmap,
mobile apps (Android and iOS are **planned, not shipped**; when that
changes, this page will say so).

Clients are the third layer of the platform's mental model -- above the
[modules](modules.md) the platform runs, which sit above the time-series
memory graph it remembers on. The two outward-facing directories in the
engine repository point in opposite directions, and a client is the
inward-pointing one:

| Direction | Category | Lives |
|---|---|---|
| MemQL calls out | **integration** | `integrations/` in the engine |
| The world connects in | **client** | its own repo, one per client |

## One repo per client, product-agnostic engine

The engine never carries product code. A client gets its **own
repository, stamped from the `memql-project` template**, which consumes
the engine as pinned images and never forks it. What a client repo
holds: the application itself, its build, its CI lane, and the deploy
wiring that publishes its bundle to a cluster.

The engine repository carries exactly **one** client as the worked
example -- **MemQL OS** (`clients/os/`), the platform's own
browser operations console. It exists in-tree so the question "where
does my app go, and how does it get served, built, tested, and deployed
alongside the engine?" has a running answer rather than a prose one, and
an allowlist test keeps `clients/` from quietly growing beyond it. The
conventions a client follows -- own package, view-kit for concept
rendering, dial the origin you were served from, one CI lane per client
-- are stated in `clients/README.md` next to the shell.

## How a client reaches the cluster

Clients speak the same surface everything else does: gRPC on
`MemqlService.Stream`, or the `/memql/ws` WebSocket bridge from a
browser, authenticated by the cluster's identity service. The typed
SDKs (`sdk/ts`, `sdk/go`) and the view-kit rendering library
(`sdk/ts-viewkit`) are the libraries clients are built with -- view-kit
turns rows plus their display hints into rendered views, so a concept
renders the day it is declared with no client change.

A website or SPA is **hosted by the cluster itself**: the edge node
resolves the request hostname to a `v1:platform:site` row and serves the
client's published bundle -- the shell is a site row, served by the same
mechanism as any customer site. A client can equally stay where it
already is and connect in over the SDK.

> Related: [Modules](modules.md) -- what the platform runs;
> [What is MemQL](../overview/what-is-memql.md) -- the whole picture;
> [Site hosting](../operate/site-hosting.md) -- the runbook for
> deploying a client onto a cluster.
