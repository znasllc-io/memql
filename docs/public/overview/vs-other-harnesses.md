---
title: memQL vs. Other Harnesses
audience: public
status: stable
area: overview
sinceVersion: 0.9.0
owner: znas
---

# memQL vs. Other Harnesses

This is meant to be fair. memQL is not the right tool for every job, and
the projects below are good at what they do. The point of this page is
to make the **category difference** clear so you can choose well.

## The category difference

Most "agent frameworks" are **libraries / SDKs**: you import them, wire
chains/agents/tools in your own code, bring your own persistence, and
add your own guardrails, multi-process coordination, and observability.

- **Python:** LangChain / LangGraph, LlamaIndex, and the surrounding
  glue. Huge ecosystems, every integration imaginable — and you assemble
  and operate the system.
- **Go:** the field is growing fast — Google ADK, Firebase **Genkit**,
  **LangChainGo**, ByteDance's **Eino**, and others. These are
  well-built Go-native libraries for composing model calls, tools, and
  flows.

memQL is a different category: a **harness + memory substrate that
runs**. The loop, the persistent memory graph, the cost/safety
enforcement, the multi-node mesh, the identity layer, and the
observability are the product — not things you assemble on top.

A useful one-liner: *the others give you the pieces to build an agent
runtime; memQL is the agent runtime.*

## Comparison

| Capability | Library/SDK (Genkit, LangChainGo, Eino, LangChain) | memQL |
|---|---|---|
| Shape | Library you import + wire | Runtime + DSL you declare against |
| Persistent memory | BYO (pick a store, wire it) | Built in: append-only time-series graph, provenance, replay |
| Memory consolidation | DIY | Episodic → semantic, recency+semantic scoring |
| Cost/loop guardrails | DIY | Built in: global rate ceiling, per-plan budgets, loop breakers, approval gate, model tiering |
| Behavior definition | Code (chains/flows) | Declarative DSL (concepts, automations, tools, prompts, specs) |
| Multi-node coordination | DIY | Built in: node mesh, event bridge w/ dedup+TTL |
| Identity / authz | BYO | Built in: identity service, JWT/JWKS, per-row authz (test-enforced) |
| Observability | BYO | Built in: per-invocation hypertable + Cockpit topology |
| Footprint | Small, no database required | A real system: PostgreSQL + TimescaleDB |
| Language | Go (or Python) | Go engine + the MemQL DSL |

## Where the others are a better fit (honestly)

- **You want a small dependency, no database.** A Go library (Genkit,
  Eino) drops into an existing service with a fraction of the surface.
  memQL is a system with a Postgres/TimescaleDB substrate — that is the
  point, and the cost.
- **You need the widest provider/integration catalog today.** The
  Python ecosystem still has the longest tail of connectors.
- **You're prototyping a single, stateless flow.** If there is no
  durable memory, no fleet, and no budget risk, a harness is overkill —
  reach for a library.
- **Maturity.** memQL is honestly pre-1.0 (versioning policy: git-tag
  semver, 1.0 at the beta); the established libraries have more miles on
  them.

## When to choose memQL

Choose memQL when the **hard parts are the point**:

- agents that must **remember** across sessions and restarts, with
  provenance;
- workloads where an unbounded loop or a stuck model is a **real cost or
  safety risk**;
- multi-agent / multi-node systems you'd otherwise have to coordinate by
  hand;
- a product you intend to **operate and inspect**, not just demo;
- a team that wants behavior expressed **declaratively** and versioned,
  not buried in glue.

And the standing proof that those are solved problems and not roadmap
items: memQL runs **CoPresent** today. See
[Why memQL Is a Harness, Not a Library](why-memql-harness.md) for the
code behind each claim.
