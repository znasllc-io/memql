---
title: Component vs integration vs pack
audience: public
status: stable
area: concepts
sinceVersion: 0.15.0
owner: znas
---

# Component vs integration vs pack

MemQL has **exactly three** extension words. Do not invent a fourth.

| Word | Means | Lives |
|---|---|---|
| **component** | Engine internals — DSL lexer/AST, HTTP/gRPC servers, bus, identity, env registry | `component/` |
| **integration** | Talk to other databases or services. Exposes DSL-callable capabilities | `integrations/` |
| **pack** | Client-agnostic product feature: a Go integration plus a `.memql` DSL bundle | Plugin SDK v1 / `examples/referencepack` |

Intake **"plugin"** means **pack**. `memql.RegisterPlugin` is the Go registration primitive a pack (or a core integration) calls at `init()`. It is not a fourth runtime, and there is no runtime (non-compiled) loading of Go. The VS Code tooling (`editors/vscode/`) is **the extension** — editor tooling, not a fourth extension kind, and never "the plugin".

`dsl/todos`, `dsl/calendar`, and `dsl/campaigns` are **core**. Packs cannot shadow them.

The worked example is Shopify, which exists as TWO artifacts on either side of the line:

- `integrations/shopify` is the **integration** — it speaks the Storefront/Admin APIs and receives inbound webhooks. It talks to somebody else's system; that is the whole test.
- `examples/shopifypack` is the **pack** — the client-agnostic product feature layered on that integration (portal views for shop, secrets, sync). The thin product index stays core (`dsl/shopify`).

Reviews is a **pack** only (`examples/reviewspack`): a product feature with no external system behind it, so it has no integration half. The thin product index and checkout URL live on the engine side of that line — merchandising stays on Shopify; checkout stays `cart.checkoutUrl`.

See [Plugin SDK](../build/plugin-sdk.md) for the Go contract and [Building a pack](../build/building-a-pack.md) for the walkthrough.
