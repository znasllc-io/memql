---
title: Local models on the fleet
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Local models on the fleet

Running the platform's own operations — planning, conductor/routing,
suggestions, embeddings — on models the machines in your fleet already host,
instead of on a metered API.

The premise is simple: if a machine you own can run the model, there is no
reason to pay per token for work a 7–8B model handles well. Cloud providers
stay available, reserved for the work that genuinely needs a stronger model —
and reaching one is always a decision somebody made, never a fallback that
happens quietly. Epic
[memql#4676](https://github.com/znasllc-io/memql/issues/4676).

---

## The hardware floor

A machine below this floor is **not offered as an inference machine**. It
remains a full worker for everything else — shell, filesystem, HTTP fetch,
computer use, local apps — and nothing about it is degraded; it simply does not
appear in the model catalog, and the cockpit's discovery names the reason.

| Platform | Minimum (supported) | Recommended |
|---|---|---|
| macOS | Apple Silicon (M1+), 16 GB unified memory, macOS 13+ | M2 Pro+ / 32 GB for 8B-class at comfortable latency |
| Linux | x86_64 + discrete GPU with >= 8 GB VRAM (CUDA/ROCm) | 12–16 GB VRAM |
| CPU-only / Intel Mac | Not supported as an inference machine (still a full worker for everything else) | — |

The floor is checked by the cockpit's model discovery, on the machine itself.
That placement is deliberate: only the machine can see its own GPU, and a
central check would be guessing from a hostname.

**"My laptop does not appear in the model list."** In order of likelihood: it
is below the floor above; it has no model runtime installed; its
`policy.yaml` `models.allow` does not list the model; it is not signed in; or
it is simply asleep. The Providers page distinguishes the last one — an offline
machine is **listed and marked offline**, not hidden, precisely so this
question has a visible answer.

---

## The model floor

The default operational class is a **7–8B instruct model with structured
output** — `llama3.1:8b` or `qwen2.5:7b` class — plus a **small embeddings
model** where embeddings route locally.

Two different things enforce and recommend that, and it is worth keeping them
apart:

- **The floor is what onboarding recommends installing.** It is a sentence in
  a dialog and in this document.
- **The catalog's capability gating is what enforces it, per call.** A model
  is routed a structured-output prompt only if the machine hosting it
  advertised structured-output capability for that model; embeddings route
  only to models that advertised embeddings. A capability miss is treated as
  an **availability** miss — the provider is simply unavailable — because
  "no machine offers this model" and "none that offers it can do what this
  prompt needs" are the same answer to whoever is waiting.

Capabilities default to **absent**. A machine that says nothing about
structured output is not selected for a structured prompt. That direction is
deliberate: a model that quietly answers prose to a conductor turn produces a
parse failure three layers away, naming nothing.

---

## The three inference doors

Using the portal requires configured inference. **Starting MemQL does not** —
the engine boots, serves and migrates with no provider configured anywhere,
and the installer asks for no key. The requirement lives at the portal, and
after sign-in the first-run gate runs in order:

1. **Passkey**, when you have none enrolled. It is what gets you back in
   without a link in your inbox.
2. **Inference**, when the cluster has no eligible source. Three doors,
   local first.
3. The console.

### Door 1 — run a local model (the default)

Pair a machine through Fleet → Machines, install a runtime, and pull a model
meeting the floor above. MemQL uses it for planning, routing, suggestions and
embeddings. Nothing is billed per token and no prompt leaves your hardware.

Supported runtimes: **Ollama**, discovered natively at its default endpoint;
and **any OpenAI-compatible endpoint** declared in the machine's `policy.yaml`
— which covers LM Studio, vLLM and llamafile. There are no other per-vendor
integrations.

### Door 2 — the Anthropic workload-identity federation

No key at rest anywhere: each pod exchanges its own projected Kubernetes token
for a one-hour bearer. Configured outside the portal; once it is complete this
step passes silently. See
[anthropic-federation.md](auth/anthropic-federation.md).

### Door 3 — an API key

An Anthropic or OpenAI key, stored the way this cluster stores every provider
credential. Calls are billed to that account. Settings → AI providers.

### What the gate does and does not do

- It gates **the console**, never the cluster. Nothing here is enforced
  server-side, and features that need a model refuse or park with a typed
  reason regardless.
- **A machine that goes offline later produces a notice, not an eviction.**
  Work that needs a model pauses and says why; you are not thrown out of a
  session because a laptop closed.
- **Auth-disabled clusters** (`MEMQL_IDENTITY_ENABLED=false`, troubleshooting
  only) skip the passkey step — there is no identity — and the inference step
  is skippable. That is the only mode with a skip.

---

## Park, never fall back

This is the decision the whole feature rests on, so it is stated plainly:

**An unavailable local model parks the work. It does not quietly run on a paid
API.**

A call whose policy primary is a fleet model, with no eligible machine online
and no authored fallback, returns the typed refusal
`no_local_model_available`, naming every machine considered and why each was
ruled out — offline, revoked, does not offer the model, missing a capability,
busy. A plan parks at `awaitingFeedback` with
`feedbackReason=no_local_model_available` and offers two actions:

- **Wake a machine** — always offered.
- **Run this plan on the cloud instead** — offered **only when a cloud
  provider is actually configured.** A button that cannot work turns "your
  machines are asleep" into "you clicked the fix and it did not fix it", which
  is much harder to act on.

Cloud runs in exactly two ways, and both are on the record:

1. **An operator authored a fallback.** A policy naming
   `@fallback("streamClaudeSonnet")` gets exactly that, with no park and no
   prompt, because somebody wrote it down.
2. **A person consented**, for this one plan or this one request. Interactive
   surfaces surface the refusal with a one-shot "use cloud this once"
   affordance; declining leaves the request refused.

There is no third way, and that is structural rather than a rule to remember:
the only providers a policy chain can reach are the ones it names.

### The shipped defaults are local-first with no cloud fallback

`localPlanner`, `localConductor`, `localSuggest` and `localEmbeddings` each
name a fleet model as primary and author **no** cloud fallback. With an idle
fleet they park. The cloud-quality policies (`balancedChat`,
`strongReasoning`, `cheapestCapable`, …) are all still present and are wired
per purpose when a purpose genuinely needs one.

---

## Accounting

A local call costs no dollars, and the ledger says so honestly rather than
pretending it is free of consequence.

- `v1:router:call.billing` gains **`local`**, beside `metered`,
  `subscription` and `unknown`. It is stamped explicitly by the fleet path and
  is **never inferred** — inferring it would claim work ran on somebody's
  hardware when nobody established that. Absence still reads as `metered`.
- `executionSurface` names the machine, as `fleet:<registrationId>`, so
  "which of my machines did this" is answerable from the ledger alone.
- `plan.tokenSpentLocal` sits beside `tokenSpentSubscription`.

The two caps want opposite answers, exactly as they do for subscription spend:

- the **dollar ceiling excludes** local tokens — nobody was billed, and
  charging them would mean the more you used your own machine, the sooner your
  plans stopped;
- the **loop caps include** the calls — a runaway loop on a free model is
  still a runaway loop, burning a laptop's battery and occupying the machine
  its owner is trying to work on.

Unreported usage is **absent, not zero**: "the model ran and used nothing" and
"the model ran and nobody counted" are different facts, and only one of them is
ever true.

The same guards apply. The process-wide rate ceiling, the identical-request
breaker and the per-plan budgets sit at the provider seam, so a fleet call
passes the gates an HTTP provider call passes, sharing the same state. See
[LLM cost control](../ai/llm-cost-control.md).

---

## Routing, and who may use whose machine

Selection is the existing Fleet router asked for a `model:<id>` label, under
the strategies it already has. Two properties are security-load-bearing:

- **A model call carries your prompts, so it routes only to YOUR machines.**
  There is no cross-user routing path. The read that finds candidates is
  caller-scoped, so another user's machine is never in the result to begin
  with.
- **System work** — automations and cluster maintenance, with no acting user
  — reaches only machines whose **owner opted in**, by setting
  `sharedInference=true` in the machine's **operator labels** on
  Fleet → Machines. It must be the operator half: the labels a cockpit
  reports are overwritten on every reconnect, so an opt-in stored there would
  be granted by the machine rather than by its owner, and revoked roughly
  whenever the lid closed.

`leastLoaded` rations by the concurrency ceiling a machine declared **for that
model**, so a machine advertising one slot for a 70B and eight for a 1B is
described correctly for each.

---

## Install-time

Local installs probe for a model runtime and, when one is present, offer to
wire it up. **The probe is inference-optional in the strong sense**: a machine
with no runtime, no model and no key completes install, uninstall, repair and
update identically, and sees no prompt at all — an install is not the moment
to sell somebody a capability they did not ask for.

```bash
# The probe, runnable on its own. "Not found" is exit 0.
scripts/install/detect-ollama.sh
scripts/install/detect-ollama.sh --endpoint=http://127.0.0.1:11434 --timeout=3
```

The VS Code extension runs the same capability. That install, uninstall,
repair and update make **no** inference call is an assertion test, not a
review note: the flows run with every outbound network seam replaced by a
function that throws.

---

## What is not here

- **Realtime voice stays on its cloud path.** No local STT or TTS.
- **No models on workbenches.** Machines only.
- Machine-side discovery, serving and usage reporting live in the
  `memql-cockpit` repo. This repo fixes the wire contract and the engine side,
  so a cockpit that advertises no models changes nothing.

---

## Related

- [Workers runbook](workers-runbook.md) — pairing a machine, tokens, scope
- [Local apps as execution surfaces](local-apps.md) — the sibling delegation surface
- [LLM cost control](../ai/llm-cost-control.md) — the guard layers
- [Anthropic federation](auth/anthropic-federation.md) — door 2
