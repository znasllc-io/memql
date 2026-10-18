---
title: Model catalog baseline and first research run
audience: internal
status: stable
area: ops
sinceVersion: 0.28.12
owner: znas
---

# Model catalog baseline

Manually reviewed on 2026-10-15. This is a source review, not a measured
quality or throughput benchmark. Existing catalog entries without a review
date retain their earlier provenance. No local-model research run was started.

| Task | Baseline | Evidence and limits |
|---|---|---|
| Conversation, structured output, tools, vision | Qwen 3.5 4B/9B; Qwen 3.8 27B where it fits | [4B artifact](https://ollama.com/library/qwen3.5:4b), [family and variants](https://ollama.com/library/qwen3.5), [27B artifact](https://ollama.com/library/qwen3.8:27b). Tags identify exact runtime offerings; the 27B tag reports Qwen 3.5 architecture. A family name alone does not establish capability or residency. |
| Embeddings | Qwen3 Embedding 0.6B, 4B, 8B | [Publisher's runtime catalog](https://ollama.com/library/qwen3-embedding). Keep the active corpus binding; a different model requires reindexing even if dimensions match. |
| Image generation | FLUX.2 klein 4B | [Publisher release](https://bfl.ai/blog/flux2-klein-towards-interactive-visual-intelligence), [Ollama offering](https://ollama.com/x/flux2-klein). Cockpit advertises `flux2-klein-4b`, distinct from the Ollama pull id. The Cockpit entry has unknown download size, so it can be selected when already installed but cannot be downloaded from this page. |
| Transcription | Whisper large-v3-turbo; Parakeet TDT 0.6B v3 | [Whisper model card](https://huggingface.co/openai/whisper-large-v3-turbo), [NVIDIA artifacts](https://huggingface.co/nvidia/parakeet-tdt-0.6b-v3/tree/main). Runtime and memory support must still be established on the machine. |
| Speech | Kokoro 82M | [Publisher repository](https://github.com/hexgrad/kokoro). Model weights and runtime compatibility are separate checks. |
| Cloud reference | text-embedding-3-large | [Official model documentation](https://developers.openai.com/api/docs/models/text-embedding-3-large). Catalog visibility only; never a local recommendation or automatic replacement. |

The larger local alternatives reviewed include [Qwen 3.6 35B](https://ollama.com/library/qwen3.6:35b),
[Gemma 4](https://ollama.com/library/gemma4) and [gpt-oss](https://ollama.com/library/gpt-oss).
Their presence does not imply that every machine can run them.

## Capacity contract

`memoryNeedBytes` is a conservative planning allowance, not live free memory.
Saved setups sum distinct selected models once and reserve operating-system
headroom. Unified, discrete GPU and CPU memory use the existing hardware
projection. There is no upper machine-class ceiling in setup admission.
Unknown residency, missing runtimes, unsupported platforms and insufficient
capacity are refusals. Downloads additionally require a known artifact size,
5 GiB of free-storage headroom, and no pending machine download.

The Qwen 3.5 4B baseline uses an 8 GB planning allowance; the Cockpit FLUX entry
uses 13 GB from the publisher's release guidance. These are not measured
claims. Context length and image inputs affect consumption, as documented in
[Ollama context guidance](https://docs.ollama.com/context-length).
Saved setups cap subsequent calls at 32K context and serialize model calls on
the replica holding the machine stream. Existing work finishes. Other apps
and runtime-retained allocations remain outside this admission mechanism.

## Deferred weekly research

`weeklyModelCatalogResearch` is scheduled for Monday 09:00 in the scheduler's
timezone, with a cluster lease. The seeded `catalogResearchSettings/weekly`
row is **disabled**. No local research should run until the follow-up issue's
acceptance checks are complete and the owner explicitly enables it.

The owner enables it with `configureModelCatalogResearch(enabled: true)`;
that records the owner of future work. `openModelCatalogResearch()` starts a
bounded, journaled work run only while enabled. The work route is
`fleet:strongest`, paid fallback is prohibited, and ceilings are 120,000 tokens,
24 model calls, one retry and 90 minutes. No model is downloaded or tested by
research. The knowledge lesson `training:model-catalog` contains the research
method; the prompt supplies the current catalog and requests a bounded report.
Only `webSearch` and `fetchUrl` are exposed. Search may return no results;
the lesson requires direct primary-source checks and honest gaps.

Findings are stored separately as review candidates. They cannot grant
installation eligibility, become recommendations, alter setup choices, or
change embedding bindings. [Follow-up issue #6121](https://github.com/znasllc-io/memql/issues/6121) tracks the first supervised run. It must verify real source
retrieval, harness journaling and budget enforcement before weekly operation.
