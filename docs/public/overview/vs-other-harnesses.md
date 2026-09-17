---
title: MemQL vs. Agent Libraries and Frameworks
audience: public
status: stable
area: overview
sinceVersion: 0.9.0
owner: znas
---

# MemQL and agent libraries

Choose based on the system you want to operate. An agent library is a dependency
inside an application you build. MemQL is an installed platform with its own
language, data model, identity, execution services, and clients.

This page compares deployment approaches, not feature checklists for specific
third-party products. Libraries differ and evolve; evaluate their current
capabilities against your requirements.

## The tradeoff

| Decision | An application built with a library | MemQL |
|---|---|---|
| Where behavior lives | Your application's code and chosen runtime | `.memql` constructs plus Go components and integrations |
| State and identity | Your application's architecture | Shared engine records and cluster identity |
| Operation | Deploy your application and its dependencies | Operate the MemQL cluster and its configured resources |
| Authoring | Your language and development tools | MemQL DSL, VS Code support, and engine APIs |
| Inspection | The tooling you select or build | MemQL OS, runtime views, logs, and work records |
| Adoption cost | Depends on the selected library and services | Learn the DSL and run a multi-service cluster |

## When a library may fit better

If you need one stateless model call inside an existing application, adding an
entire platform can be unnecessary. A library may also fit when you want to keep
your current data and identity architecture or need a particular provider that
MemQL does not integrate with.

## When to evaluate MemQL

Consider it when data, history, agent work, tools, automations, and hosted client
surfaces need to share a runtime and access model. Start with a small prototype
that tests the operations you actually need, including a refused operation and
a recovery case.

MemQL is alpha and pre-1.0. Its [proving scorecard](proving-scorecard.md) states
what has been measured and what has not; it is not a general guarantee about
your workload. [What is MemQL?](what-is-memql.md) covers the broader product,
and [the harness guide](why-memql-harness.md) covers its work execution system.
