---
title: MemQL status and direction
audience: public
status: stable
area: overview
sinceVersion: 0.20.0
owner: znas
---

# MemQL status and direction

MemQL is alpha and pre-1.0. This page separates implemented surfaces from design
work; it does not promise release dates. Status recorded **2026-09-15**.

## Implemented in the repository

- Typed concepts, queries, mutations, logic, tools, prompts, and automations.
- Durable agent work and a [scorecard](proving-scorecard.md) that states its measurement limits.
- [MemQL OS](../operate/memql-os.md), with apps for data, artifacts, sites, Fleet, work, identity, and operations.
- [VS Code or Cursor authoring](../language/vscode.md), offline language intelligence, and authenticated runtime tools.
- [Pack enablement](../concepts/modules.md), [site hosting](../operate/site-hosting.md), and [client repositories](../concepts/clients.md).

Availability on a particular cluster depends on its release, configuration,
permissions, and healthy dependencies. Repository presence is not a production
readiness certification.

## Design rollout

[Supervised Visual Composition](../operate/supervised-visual-composition.md) is
the approved OS design direction. Fleet provides visual composition, persistent
policy editing, and typed Ask proposal review. Ask generation requires configured
compatible inference. The Deployables redesign is approved and being implemented;
it has not yet deployed. New Settings/Logs layouts await prototype approval.

## Future work

General autonomous driving of OS interfaces and coordinated voice operation of
those interfaces remain future work. Native mobile application delivery is also
outside the currently served static-site contract; see
[Deployables](../operate/deployables.md) for its exact boundaries.

Follow [GitHub issues](https://github.com/znasllc-io/memql/issues) and the release
notes for changes. Historical walkthroughs remain in the [proving log](proving.md);
they describe what was observed at the recorded commit, not the current interface.
