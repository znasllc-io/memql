---
title: Module taxonomy: component, integration, pack, said correctly everywhere
audience: internal
status: draft
area: design
sinceVersion: 0.19.6
owner: znas
---

# Module taxonomy: component, integration, pack, said correctly everywhere

> **Status.** This is the design record epic memql#4276 asked for, written
> because the epic was 27 individual kind decisions and could not be worked
> until they were made. **The decisions below are made.** One of the epic's
> four agenda items — the guard — is implemented (`module_taxonomy_test.go`).
> The other three move Go packages, DSL domains and two portal pages, and are
> **not done**; each carries an explicit "what it would take" section so the
> next person does not have to re-derive it.

## The defect, precisely

`docs/public/concepts/component-integration-pack.md` gives the test verbatim:
an integration *"talks to somebody else's system; **that is the whole test**"*.

The portal's `/integrations` page does not apply it. `integrationStatus`
(`dsl/integrations/builtins.memql:28`) builds its list from `registryRollCall`
(`integrations/email/status.go:545-566`), which is `memql.RegisteredPlugins()`
**raw** — no kind, no pack filtering.

`RegisterPlugin` is the DSL→Go capability seam that **every** Go-backed builtin
uses, which is what makes the page wrong rather than merely incomplete: it is
showing the seam and calling it the taxonomy. Sixteen of the twenty-seven names
on it talk to no external system at all.

`/modules` already models this correctly — `component/memql/module_registry.go`
declares `component | integration | pack | node-type` and enumerates each from
a different source. But its integration bucket is "every registered plugin not
bound to a pack", and **only two bindings exist in the engine build**
(`harnessrecall`, `harnesstrace`). So the correct model is fed the wrong data.

## The 27 decisions

Verdicts are in `pluginKinds` (`module_taxonomy_test.go`), which is the
enforced copy; this table is the reasoning.

| Kind | Names | Why |
|---|---|---|
| **integration** (7) | `avatardirect`, `email`, `openairealtime`, `shopify`, `storage`, `telephony`, `voice` | each makes an outbound call to a named third party's API |
| **component** (8) | `auth`, `rbac`, `router`, `database`, `identity`, `timeutil`, `deployversion`, `embedding` | there is no MemQL with these switched off; turning one off does not remove a feature, it breaks the engine |
| **pack** (12) | `chat`, `dailyspace`, `agents`, `library`, `knowledge`, `liveknowledge`, `actionSearch`, `similarity`, `files`, `workbench`, `harnessRecall`, `harnessTrace` | a product feature with a coherent "off" |

### The four the epic left open

Each was decided by reading the source, not by preference.

- **`embedding` → component.** It makes no outbound call. `plugin.go:20-42`
  resolves an `EmbeddingAIProvider` from the engine's own provider registry and
  the **provider** makes the vendor call. The vendor lane it rides is already
  switchable through a provider's `@disabled`, so a second switch here would be
  a second answer to one question.

- **`dailyspace` → pack.** No HTTP anywhere in the package, and a user
  preference for it already exists.

- **`workbench` → pack.** It holds an `http.Client`, and that is *not* the
  doc's test being met. `handleHTTPFetch` fetches a URL the model chooses at
  call time; the system on the other end is not integrated with, it is browsed.
  **A connector knows whose API it is speaking.** This one does not — which is
  exactly why it is a sandboxed capability slug rather than a vendor lane.

- **`knowledge` → pack, containing a real integration.** The genuinely mixed
  case. `integrations/knowledge/seed_wikipedia.go` calls Wikipedia's REST API
  with its own `User-Agent` and `http.Client` — the doc's test met exactly. The
  package as a whole is a product feature (knowledge domains); the seeder is a
  connector living inside it. That is the Shopify shape the epic names, and
  splitting it is work item 2 below.

### The named cases from the epic

- **Campaigns** is `component/campaigns` with no plugin registration at all —
  a product feature over an email connector, so **pack**. It does not appear in
  `pluginKinds` because nothing registers it; that is why the guard's
  stale-row check exists rather than a fixed count.
- **Voice and telephony** genuinely do talk to LiveKit and Telnyx. They are the
  Shopify shape: **an integration (the transport) plus a pack (the product
  feature)**. That split is what makes "we don't want audio" a toggle rather
  than a rebuild.
- **Harness** is already a pack and proves the mechanism works.

## What is done

**Agenda item 4, the guard.** `module_taxonomy_test.go` fails the build when a
`memql.RegisterPlugin` name is not classified, and when a classified name is no
longer registered. Both directions are proven to fire.

It is a **static scan** of the tree rather than a call to
`RegisteredPlugins()`, and that is deliberate: the registry is populated by
`init()` in each integration package, so a test only sees what its own binary
imports — and node-type-scoped plugins are behind build tags. The scan finds
every registration regardless. (Demonstrated: the probe that proved the guard
bites used a file excluded by `//go:build never_built`, which never linked and
was still caught.)

The guard carries two coverage floors — files scanned, and registrations found
— because a walk that matched nothing would otherwise report a clean tree.

## What is not done, and what each would take

**1. Retire `/integrations` as a taxonomy page.** Its one genuinely useful
part is the email configuration report — slot-by-slot credential presence and
the live probe — which becomes the `email` module's detail page under
`/modules`. Campaigns moves out from under `/integrations` to the pack it
becomes. `integrationStatus` either grows a `kind` field or is replaced by the
modules read. Touches: `dsl/integrations/builtins.memql`,
`integrations/email/status.go`, two portal routes.

**2. Make the classifications real.** Today `pluginKinds` is a table a test
enforces; it does not change what `/modules` enumerates. For a pack row to
behave like a pack — to get the cluster-wide `v1:platform:packState` toggle for
free — it needs a pack domain plus a `BindPluginToPack` call. Twelve of those,
plus the `knowledge` and `voice`/`telephony` splits above. This is the bulk of
the epic and the reason it was filed as a spike.

**3. Move what is misplaced.** Sixteen packages under `integrations/` are not
integrations. Whether they move is a separate question from what they are
called: a directory move is a large diff with no behavioural change, and the
kind is now recorded either way. Recommended order: classify (done), bind
(item 2), move last or never.

## Constraints a future change must respect

- **Three words, no fourth.** `TestModuleKindsAreTheThreeExtensionWords`
  enforces it. The failure mode is not someone declaring a fourth const — it is
  someone reaching for a plausible new word (`service`, `connector`,
  `provider`) for a case that did not fit, and the taxonomy becoming four words
  nobody can define.
- `nodeType` in `component/memql/module_registry.go` is a fourth
  **enumeration**, not a fourth extension word. Nothing registers a plugin as
  one.

Design record for the module registry itself:
`docs/superpowers/specs/2026-08-20-module-registry-design.md`. Sibling epic:
memql#4261.
