---
title: MemQL documentation
audience: public
status: stable
area: overview
sinceVersion: 0.20.0
owner: znas
---

# MemQL documentation

Build with the engine, manage it in MemQL OS, and author `.memql` in VS Code or Cursor.
MemQL is alpha; use the docs for the same release as your cluster and extension.

## Start here

| Your goal | Read |
|---|---|
| Understand the product | [What is MemQL?](what-is-memql.md) |
| Get an achievable first result | [Getting started](quickstart.md) |
| Write and run a `.memql` file | [Your first MemQL program](../language/first-program.md) |
| Compose retrieval, AI, caching, and automation | [Research-brief workflow](../language/research-workflow.md) |
| Install the editor extension | [MemQL in VS Code or Cursor](../language/vscode.md) |
| Find the right cluster app | [MemQL OS](../operate/memql-os.md#the-apps) |

## Build with MemQL

Start with [concepts and versioning](../concepts/concept-versioning.md), then
use the [language reference](../language/memql.md) as you write. The
[authoring rules](../language/authoring-rules.md) explain common errors;
[training](../language/training.md) explains when a definition becomes live.

- **Data:** [validation](../concepts/data-validation.md), [identifiers](../concepts/identifiers.md), [events](../concepts/events.md), [data origins](../concepts/data-origins.md).
- **AI and work:** [routing](../operate/ai-routing.md), [cost control](../ai/llm-cost-control.md), [agent harness](why-memql-harness.md), [audio streaming](../build/audio-streaming.md).
- **Applications:** [clients](../concepts/clients.md), [site hosting](../operate/site-hosting.md), [packs](../build/building-a-pack.md), [Go SDK](../../../sdk/go), [TypeScript SDK](../../../sdk/ts/README.md).

## Operate a cluster

Follow [first-run setup](../operate/first-run.md) and
[configuration readiness](../operate/configuration-readiness.md). Then choose
the guide for the job:

- [Fleet and local models](../operate/local-models.md), [shared machines](../operate/shared-machines.md), [workbenches](../operate/workbench-runbook.md).
- [Files and Library](../operate/library.md), [Deployables](../operate/deployables.md), [campaign sending](../operate/campaign-sending.md).
- [Identity and access](../operate/auth/access-model.md), [logs](../operate/logs.md), [local runtime](../operate/reproduce-the-cloud-locally.md).
- [Installation requirements](../operate/minimum-requirements.md), [database platform](../operate/database-platform.md), [upgrade barriers](../operate/upgrade-barriers.md).

## Reference and project status

[Full index](../../../GLOSSARY.md) · [Tech stack](tech-stack.md) ·
[Architecture](../concepts/architecture.md) · [Measured results](proving-scorecard.md) ·
[Status and direction](roadmap.md) ·
[Supervised Visual Composition](../operate/supervised-visual-composition.md).

`docs/public/` is the release-versioned user documentation. Design records under
`docs/internal/` and `docs/superpowers/` explain decisions; they are not a promise
that a proposed feature is available. Contributor workflow lives in
[CONTRIBUTING.md](../../../CONTRIBUTING.md).
