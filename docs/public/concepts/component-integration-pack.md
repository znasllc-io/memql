---
title: Component vs integration vs pack
audience: public
status: stable
area: concepts
sinceVersion: 0.15.0
owner: znas
---

# Component vs integration vs pack

memQL has **exactly three** extension words. Do not invent a fourth.

| Word | Means | Lives |
|---|---|---|
| **component** | Engine internals — DSL lexer/AST, HTTP/gRPC servers, bus, identity, env registry | `component/` |
| **integration** | Talk to other databases or services. Exposes DSL-callable capabilities | `integrations/` |
| **pack** | Client-agnostic product feature: a Go integration plus a `.memql` DSL bundle | Plugin SDK v1 / `examples/referencepack` |

Intake **"plugin"** means **pack**. `memql.RegisterPlugin` is the Go registration primitive a pack (or a core integration) calls at `init()`. It is not a fourth runtime, and there is no runtime (non-compiled) loading of Go.

`dsl/todos`, `dsl/calendar`, and `dsl/campaigns` are **core**. Packs cannot shadow them.

Shopify is an **integration** (`integrations/shopify`). Reviews is a **pack**. The thin product index and checkout URL live on the engine side of that line — merchandising stays on Shopify; checkout stays `cart.checkoutUrl`.

See [Plugin SDK](../build/plugin-sdk.md) for the Go contract and [Building a pack](../build/building-a-pack.md) for the walkthrough.
