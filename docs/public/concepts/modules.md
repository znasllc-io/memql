---
title: Modules
audience: public
status: stable
area: concepts
sinceVersion: 0.20.0
owner: znas
---

# Modules

**"Module" is the collective term for the things a MemQL cluster runs
and an operator manages.** It is not a fourth extension kind: the three
extension words stay exactly as
[Component vs integration vs pack](component-integration-pack.md) locks
them, and this page adds only the umbrella over them plus the one row
group that is not an extension kind at all -- node-type modules.

| Module kind | What it is | Enablement mechanism |
|---|---|---|
| **component** | Engine internals (identity, engine, ai, campaigns, ...) | None -- built in. The registry reports them for visibility and their environment-variable surface, never a switch. |
| **integration** | Talks to somebody else's system | **Derived from configuration.** An integration is active when its credentials/config are present, opted out when its factory declines (for example, no embedding provider), compiled out of a binary that never wires it. Nothing is stored; the state is read from what already decides it. |
| **pack** | Client-agnostic product feature (Go + DSL) | **A persisted per-instance toggle.** `v1:platform:packState` in the shared graph, flipped by the cluster owner, read by every node at boot. Absence of a row means enabled, so shipping a pack changes nothing for existing installs. |
| **node-type module** | A deployment unit of the mesh (bff, voice, cognition, agent, planner, workbench, mcp, edge, identity) | **Replica scale.** A node type at zero replicas is deliberately off. Voice is the worked example: without LiveKit credentials the deploy layer holds its lane at zero replicas, loudly -- the registry reports that as `credential_gated` rather than inventing a second switch. |

## What disabling a pack means

A disabled pack is **mounted-inert**, not removed:

- Its namespace stays owned -- another pack claiming the same domain is
  refused exactly as when it was enabled.
- Its **concepts still load**, so rows written before the flip stay
  browsable and cross-domain schema references keep resolving.
- Everything behavioral is absent: its tools, queries, mutations,
  builtins, prompts, and automations are not registered, and its Go
  side is not materialized. A model cannot call what is not there.
- **A flip takes effect as each node restarts** (stated honestly in the
  reply and in the portal). Between the flip and the restart, the
  registry shows both facts: the cluster-wide desired state and what
  each node actually loaded at its own boot.

Because the state lives in the shared graph rather than per-node
environment, every node resolves the same answer -- there is no way to
configure half a mesh.

The worked pack in this repository is **`examples/referencepack`**: a
`dsl/` tree of its own, a Go half that self-registers through
`memql.RegisterPlugin`, and a state row that decides whether the engine
loads it. Disabling it takes its constructs out of the tree at boot --
its queries stop resolving and its automations stop firing -- while every
other domain is untouched, which is what "a coherent off" means.

The harness used to be that proof, and is no longer a pack at all: it is
the platform's work spine ([why it exists](../overview/why-memql-harness.md)),
its state is ordinary `v1:work:*` rows, and there is nothing to switch
off.

## Where you manage modules

The **MemQL Portal** is the management surface: an owner/admin-gated
Modules browser showing every module's kind, state, and health, and a
detail view with the module's environment-variable surface (which
variables it reads, which are set -- secret values never leave the
engine, in any form) and the kind-appropriate control. The engine
reports all of it over gRPC message types on `MemqlService.Stream`; the
portal renders what the engine says and adds nothing.

The design record for the registry -- enumeration sources, the
per-node vs cluster-wide honesty split, and the enablement semantics --
is `docs/superpowers/specs/2026-08-20-module-registry-design.md` in the
repository.

> Related: [Component vs integration vs pack](component-integration-pack.md)
> -- the three locked extension words this page is the umbrella over;
> [Clients](clients.md) -- what you build ON the platform, which is a
> different axis from what the platform runs.
