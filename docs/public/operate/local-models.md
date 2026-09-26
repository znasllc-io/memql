---
title: Local models on the fleet
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Local models on the fleet

Running the platform's own operations — planning, suggestions, embeddings —
on models the machines in your fleet already host, instead of on a metered API.

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

**How the runtime gets there.** `memql worker setup --inference` installs it
without anything from root on either platform. On macOS that is Homebrew's
Ollama. On Linux it is Ollama **as the person's own user**: the vendor's
release archive, checked against that release's `sha256sum.txt`, unpacked
into `~/.memql/ollama/runtime` and kept running by a user systemd unit on
the loopback, with models under `~/.memql/ollama/models` — reaching the GPU
through the same device nodes `nvidia-smi` used to pass the floor, so no
container toolkit, no Docker and no daemon restart are involved. The
`ollama/ollama` container is still there as `--runtime docker`, and it does
need the NVIDIA container toolkit (a root install). The Linux uninstaller
removes the runtime's unit with the worker's; its `--purge` removes the
runtime and the models. Design record:
[2026-09-08-linux-native-runtime-and-class-defaults](../../superpowers/specs/2026-09-08-linux-native-runtime-and-class-defaults-design.md).

**"My laptop does not appear in the model list."** In order of likelihood: it
is below the floor above; it has no model runtime installed; its
`policy.yaml` `models.allow` does not list the model; it is not signed in; or
it is simply asleep. The Providers page distinguishes the last one — an offline
machine is **listed and marked offline**, not hidden, precisely so this
question has a visible answer.

Since epic [memql#5146](https://github.com/znasllc-io/memql/issues/5146) the
machine's own page in Fleet answers this directly rather than by elimination:
it reports what the machine said about itself, what class the cluster
concluded from that, and — when the answer is no — which of the three
absences it is. See [The scanner](#the-scanner) below.

---

## The scanner

Everything above is a floor somebody has to check by hand. The scanner is the
cluster asking the machine, and then saying what it heard.

A cockpit reports a **hardware inventory** on register and on heartbeat: chip,
memory, accelerator and its backend, cores, OS version, free disk, and the
model runtimes it found with their versions. From that the cluster computes
one thing the machine did not report — the **machine class** — and the page
keeps the two apart, because everything else on it is something the machine
SAID and the class is something the cluster CONCLUDED:

> **A 32 GB machine, with 48.0 GiB usable.**
>
> | | |
> |---|---|
> | Chip | Apple M4 Max |
> | Memory | 64.0 GiB |
> | Accelerator | Apple M4 Max, sharing 64.0 GiB of unified memory |

The working is shown with the verdict on purpose. A class with no figure
behind it is an opinion.

### Three absences, three sentences

A blank cell would collapse these into one, and they lead to three different
actions — do nothing, buy hardware, fix a driver:

| What is true | What the page says |
|---|---|
| The cockpit predates the field | "This machine's cockpit has not reported what hardware it has. An older cockpit does not send it, and the machine is working normally -- updating the cockpit is what fills this in." |
| The machine reported and the answer is no | "Under the floor for local models, with 6.0 GiB usable.", then "Local models need Apple Silicon with 16 GB, or a discrete GPU with 8 GB." — the floor in the words the wizard uses, with the facts left on screen so the figure it was judged on stays visible |
| There is no usable accelerator | "No accelerator the runtimes can reach. Models run on the processor, which works and is slow." |

The third is deliberately **not** "no GPU". The machine may have a card whose
driver does not work, and that repair lives on the machine.

### The recommended set

Given the class, the page says what the catalog recommends — one model per
level — and offers to pull the set as **one act**:

> For a 24 GB machine, the catalog recommends:
>
> | | |
> |---|---|
> | Fast | `qwen3.8:27b` · 16.5 GiB · the curated text model for all three levels |
> | Strong | `qwen3.8:27b` · the same model; a second one would not stay resident beside it |
> | Reasoning | `qwen3.8:27b` · thinking is in the record |
> | Embeddings | `qwen3-embedding:0.6b` · 610 MiB · the cluster's active embedder |
>
> A level whose pick this machine cannot serve is shown with the reason, for
> example **Needs the Kokoro runtime, which this machine has not reported.**

**A blocked entry is shown, not filtered.** Hiding it would answer "there is
nothing for reasoning on this machine", which is false — the answer is "there
is, and here is what is in the way". Blocked entries are ordered by how
fixable they are: pull it, install a runtime, the class, the platform.

Installing a runtime is the cockpit's job and is said so plainly: *the cluster
never puts software on somebody's machine.*

### Measured capability

A machine can tell you it has a model. It cannot tell you the model is any
good on that hardware. **Probe this model** runs a pinned suite — structured
output, tool calling, and throughput at two context sizes — and records what
it measured:

> `qwen3.5:9b` — 9B · Q4_K_M · 125k context · structured output · tool calling
> · Valid 80% · Tools 67% · Speed 42 tok/s · suite 1

Four things about these figures are load-bearing:

- **"Not measured" is a value, not a zero.** A model nobody has probed says
  "Not measured on this machine yet." A figure and its absence are different
  answers, all the way to the pixel.
- **The suite version is on every reading.** A number measured by a different
  suite is not comparable to one measured by this suite, and the reading says
  which it was rather than leaving that to be assumed.
- **Measurement ranks; it does not gate.** A model that scored badly is still
  offered — it is ordered below one that scored well. A gate would make a bad
  afternoon on somebody's laptop permanent.
- **The probe runs on the machine that holds the stream.** The row names the
  replica that claimed it, exactly one replica acts on it, and a stale sweep
  closes a probe whose machine went away mid-suite.

---

## The model floor

The default set is **whatever the machine's class recommends** — one text
model that carries tools, thinking, structured output and vision in a single
record, sized to the memory the machine actually has, plus a **small
embeddings model** where embeddings route locally. The per-class table is
[below](#what-to-pull-by-machine-class); the standalone setup also has a smaller 4B fallback below class 16.

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
deliberate: a model that quietly answers prose to a structured-output turn
produces a parse failure three layers away, naming nothing.

---

## The four inference doors

Using the console's AI surfaces requires configured inference. **Starting MemQL does not** —
the engine boots, serves and migrates with no provider configured anywhere,
and the installer asks for no key. The requirement lives at the console, and
after sign-in the first-run gate runs in order:

1. **Passkey**, when you have none enrolled. It is what gets you back in
   without a link in your inbox.
2. **Inference**, when the cluster has no eligible source. Four doors, local
   first -- and none of them is an API key, because the product has none.
3. The console.

### Door 1 — run a local model (the default)

Pair a machine through Fleet → Machines → Add machine with **"This machine
will run local models"** ticked, and the one-line install command grows a
`--inference` flag: the installer checks the hardware floor, sets up a runtime
when it can do so without asking, and pulls a starting model in the same
terminal. On a fresh machine the runtime install needs a person, so the guided
install states the second command up front -- `memql worker setup --inference`,
run once the installer prints SUCCESS -- and its Checks stop offers **Pull the
recommended models** the moment the machine reports a runtime, then **Ask it
something** the moment a model is served (the guided install is described in
the [workers runbook](workers-runbook.md#55-pairing-a-machine-from-memql-os-the-guided-install)).
MemQL then uses it for planning, routing, suggestions and embeddings. Nothing is billed
per token and no prompt leaves your hardware.

A machine already paired is set up the same way from its own terminal:

```bash
memql worker setup --inference
memql worker setup --inference --model qwen3.5:9b --model qwen3-embedding:0.6b
```

Supported runtimes: **Ollama**, discovered natively at its default endpoint;
and **any OpenAI-compatible endpoint** declared in the machine's `policy.yaml`
— which covers LM Studio, vLLM and llamafile. There are no other per-vendor
integrations.

#### Pulling a model from MemQL OS

A machine's page in Fleet → Machines carries a **Models** group: what the
machine runs, with each model's parameter count, quantization and context
window, and a **Pull** control for the machine's owner.

```
Fleet → Machines → (a machine) → Models
```

Four things about that surface are deliberate, and each of them is the answer
to a question the naive version gets wrong.

**The act belongs to one machine.** A pull writes gigabytes to one disk and
edits one `policy.yaml`, so it is offered on that machine's page and nowhere
else — there is no fleet-wide "pull a model" that would have to ask which
machine. It is offered to the machine's **owner only**, and to anybody else the
control is absent rather than disabled: `fleetModelPull` refuses a machine that
is not the caller's, and a greyed button would advertise an act nobody on that
page can reach.

**Every refusal happens before anything is written.** A machine that is not
yours, is revoked, or is not connected to the cluster right now is refused at
the press, with a sentence naming which. There is no second machine to fall
through to, so a refusal you can act on beats a progress bar that fails five
minutes later for a reason that was knowable at the start.

**The progress numbers describe one STEP, never the whole download.** A
runtime fetches a model as a set of blobs and counts each from zero, and states
no whole-pull total anywhere — so the bar is labelled "in this step", the
counters legitimately jump backwards at each layer boundary, and there is no
overall percentage. When the runtime states no size for a step at all, the bar
is **absent** rather than empty: an empty track claims the download has moved
nothing, when what happened is that nobody said how far there is to go. The
runtime's own status line ("pulling 8eeb52dfb3bb", "verifying sha256 digest")
carries the rest, verbatim.

**Leaving the page does not stop the pull.** The download runs on the agent
replica holding the machine's connection, and its record is a
`v1:worker:modelPull` row — so it survives the tab closing, and the same rows
answer "why is this model here" long afterwards. A failed pull is the reason
that history is shown at all: a successful one is already visible as a model,
while a failed one leaves nothing behind and the machine's model list simply
does not change.

**A pulled model may not be visible to the cluster yet.** The cockpit
re-advertises its label set at once after a pull it drove, so the model is
normally routable immediately; when it could not, the record says
`sees it when this machine next reconnects` rather than reporting plain
success. Those are different facts and the surface keeps them apart.

#### When a pull does not finish

| What you see | What happened | What to do |
|---|---|---|
| "…is not connected to the cluster right now" | No agent replica holds that machine's stream. The pull was refused and nothing was written. | Wake the machine, or check its cockpit is running, and try again. |
| "…was revoked" | The registration survives revocation as audit history, so it still appears. | Pair the machine again; its old worker token can never be used. |
| "No agent replica is driving this pull" | The replica that claimed it is gone — scaled down or killed. `workerModelPullStaleSweep` closes the record within two minutes. | Start the pull again. Whatever was fetched stays on the machine, so it resumes rather than restarting. |
| A runtime error ("no space left on device") | A pull can fail inside a successful HTTP response; the runtime's own words are carried through. | Fix what the message names, then pull again. |
| The model does not appear after a success | The machine could not re-advertise, so the cluster has not seen the new label set. | It appears when the machine next reconnects; nothing needs doing. |

### Door 2 — a signed-in Claude Code or Codex on one of your machines

A subscription you already pay for, used as an inference door (epic
memql#5096). A machine that has the app **allowed** in its own `policy.yaml`
and **signed in** advertises it, and a policy naming `app:claude-code` — or
`app:*` for any of them — reaches it. MemQL hands over a prompt and takes back
an answer; nothing is billed to MemQL, and the ledger records the call as
`subscription` rather than as free.

**It does not serve MemQL's own tool-calling turns**, and that is the design
rather than a gap. On a tool turn MemQL is driving, and an app is an agent
that drives itself: it reaches MemQL's tools through MCP, in the other
direction. A tool turn therefore walks past every app door and lands on the
next entry in the chain.

A machine whose stream a **sibling replica** holds leaves the door shut on
this one — the app-session envelope has no cross-node forward yet.

### Door 3 — Anthropic workload identity federation

No key at rest anywhere: each pod exchanges its own projected Kubernetes token
for a bearer that lives at most an hour. Configured outside the console; once
it is complete this step passes silently. See
[anthropic-federation.md](auth/anthropic-federation.md).

### Door 4 — OpenAI workload identity federation

The same shape for the other vendor, on the same `memql-engine` ServiceAccount
with its own projected token and audience (epic memql#5088). Calls are billed
to the Platform service account the mapping targets. See
[openai-federation.md](auth/openai-federation.md).

### There is no fourth door, and no API-key door

A manually entered vendor API key is not a way to configure inference on this
cluster. There is no field for one in Settings → AI providers, no env name the
engine reads, and nothing seeds one into a Secret. Federation and the fleet are
the whole list.

**A local cluster therefore reaches cloud models through door 1 only.** k3d's
OIDC issuer is private, so neither vendor can federate with it, and doors 2 and
3 are unavailable there by construction rather than by omission. Streaming
transcription and Whisper are OpenAI calls and are off on a local cluster for
the same reason; there is no local substitute today.

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

## The chain: local, then an app, then anybody's money

Every shipped policy tries the doors in **cost order** (epic memql#5096):

```
@primary("fleet:strongest")      a model on hardware you already own. Electricity.
@fallback("app:*")       a signed-in Claude Code or Codex on one of your
                         machines. A subscription you already pay for.
@fallback("streamClaudeSonnet")   … and then the vendor entries, which cost
@fallback("stream54Pro")          money and are gated by the cost ceiling.
```

`fleet:strongest`, `fleet:fastest` and `app:*` are **selectors**: they
resolve among what the fleet offers for each call. The shipped fast rule uses
`fleet:fastest` with its quality floor; the stronger text lanes use
`fleet:strongest`. For strongest selection:

- **strongest first** — active parameters for mixtures when known, otherwise total
  parameters, then context window, quantization precision and model id;
- **your explicit preference wins**, if you set one (Fleet → Routing,
  `modelPreference`);
- **a model that does not say how big it is sorts LAST**, never first. It stays
  eligible for everything it advertised; it simply does not win by silence.

### The ceiling gates the federation hop

Falling back to a **paid** provider consults the cost ceiling first
(`MEMQL_LLM_MAX_TOTAL_COST_USD`, `MEMQL_LLM_MAX_TOTAL_CALLS`, and their
per-scope siblings). When it is reached, the call is refused with
`ceiling_reached` rather than spending past it.

A chain an operator wrote that **starts** at a vendor is unaffected: the
ceiling governs falling back to paid inference, not choosing it.

---

## Park, never fall back

This is the decision the whole feature rests on, so it is stated plainly:

**Work parks when EVERY door is shut. It does not quietly run on a paid API
that no policy named.**

The refusal is typed and names every door it tried and why each did not open —
and, for the local doors, every machine considered: offline, revoked, does not
offer the model, missing a capability, busy. That machine-level detail is the
half you can act on.

The run **parks** rather than failing: it becomes a `v1:work:approval` of kind
`inferenceUnavailable` on the run, and it resumes when

- **a door opens** — the sweep re-checks a parked run every five minutes, so a
  laptop you open or an app you sign into releases it with nobody deciding
  anything; or
- **you decide the approval** — "use a paid provider for this run", or stop
  the run.

A **ceiling** park carries no re-check: only a person changes a ceiling, so
polling would burn a dispatch every five minutes to rediscover a number nobody
touched.

Nothing about the work was wrong, which is why it parks: failing it would throw
away a compiled template and a journal because somebody closed a lid.

### A pinned policy still refuses

Park-not-fallback is unchanged for a policy that names ONE fleet model and
authors no fallback: it refuses exactly as it did, and a person's one-shot
consent is still the only way past it. What changed is the **default**, not the
rule.

### The four seeded local-only policies are gone

`localPlanner`, `localConductor`, `localSuggest` and `localEmbeddings` were
seeded local-first with no fallback and **named by nothing**. They are deleted:
the purposes they stood for are retired (the planner loop, memql#5052; there is
no conductor in the engine) or covered by the chain above. A policy nothing
names is not a default; it is a decoration that reads like one.

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

- **A model call carries your prompts, so it routes only to YOUR machines
  and to machines LENT TO YOU.** Another person's machine reaches your call
  only when its owner shared it with everyone, with you, or with a group you
  are in, AND the machine itself agreed (`inference.serve: cluster` in its
  `policy.yaml`). A machine nobody lent you is never a candidate.
- **System work** — automations and cluster maintenance, with no acting user
  — reaches only machines **lent to everyone**, with both consents. A machine
  shared with specific people never serves it. [Sharing a
  machine](shared-machines.md) is the whole story, including who you can lend
  a machine to and what the lender is told afterwards.

  > This replaced the `sharedInference=true` **operator label**, pre-release
  > and with no shim. A machine that reports `sharedInference` in its own
  > labels is not selected, and a test asserts it. The label could say only
  > one thing, and it could not say *which* consent was missing — so a
  > machine that had been lent and then carried onto a train looked identical
  > to one whose owner had never offered it.

- **A caller's own machines come first**, as a stable partition rather than a
  tie-break, so a fleet that starts sharing does not silently reroute work
  that was already working.

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

- **Transcription stays on its cloud path.** No local STT.
- **No models on workbenches.** Machines only.
- Machine-side discovery, serving and usage reporting live in the
  `memql-cockpit` repo. This repo fixes the wire contract and the engine side,
  so a cockpit that advertises no models changes nothing.

---

## Related

- [Workers runbook](workers-runbook.md) — pairing a machine, tokens, scope
- [Sharing a machine](shared-machines.md) — with people, groups or everyone;
  the two consents, and what the lender is told afterwards
- [Local apps as execution surfaces](local-apps.md) — the sibling delegation surface
- [LLM cost control](../ai/llm-cost-control.md) — the guard layers
- [Anthropic federation](auth/anthropic-federation.md) — door 2

---

## The curated catalog

Epic [memql#5137](https://github.com/znasllc-io/memql/issues/5137) added a
**catalog**: a short list of open-weight models worth running, organised by what
you would use them for, seeded with the cluster and readable at **Fleet →
Models**.

It is a recommendation and it **gates nothing**. A model your machines already
serve is used whether or not the catalog lists it; a model the catalog lists and
nobody has pulled is not available to anything. What the catalog knows is what a
machine class *should* pull, which is a different question from what the fleet
*can* serve — and the Fleet page shows the two beside each other, so the gap is
the thing you read rather than something you work out.

### Nine categories

| Category | What it is for |
|---|---|
| `text` | Everyday work: chat, tools, structured output |
| `reasoning` | Problems worth spending thinking on |
| `omni` | Every modality from one set of weights |
| `vision` | Deliberately empty — the text entries see |
| `audioIn` | Transcription |
| `audioOut` | Speech |
| `imageGen` | Making images |
| `videoGen` | Listed, recommended nowhere |
| `embeddings` | Search and memory |

Two of those rows are decisions rather than gaps.

**`vision` has no entries by design.** Every text model in the catalog carries
vision, so a separate vision pull would be a second copy of weights the fleet
already holds.

**`videoGen` is recommended nowhere.** The entries are listed so you can see
they exist and were considered; every one is a Linux GPU job measured in
minutes, and putting one behind a level a chat turn resolves would be a
multi-minute job answering a question somebody asked in a sentence.

### Runtimes

A model needs its runtime installed on a machine before that machine can serve
it. `ollama` covers most of the catalog; the others are small separate installs
the cockpit performs on consent.

| Runtime | Serves |
|---|---|
| `ollama` | Text, reasoning, embeddings, and (macOS, experimental) image generation |
| `mlx` | Apple Silicon models outside the Ollama library |
| `whispercpp` | Whisper transcription |
| `nemo` | NVIDIA Parakeet and Canary transcription |
| `kokoro` | Speech synthesis |
| `mflux` | Image generation on Apple Silicon |
| `comfyui` | Video generation, Linux and a GPU |

### What to pull, by machine class

`minMachineClass` on each entry is a **floor** in gigabytes of unified memory or
VRAM. A machine can manually pull eligible models at or below its class; its
automatic recommended set is the row below and does not accumulate smaller sets.

| Class | Fast / strong / reasoning | Embeddings | Estimated resident set |
|---|---|---|---|
| Below 16 GB, above the setup hardware floor | `qwen3.5:4b` | `qwen3-embedding:0.6b` | Depends on available memory and context; simultaneous residency is not guaranteed |
| 16 GB | `qwen3.5:9b` | `qwen3-embedding:0.6b` | 11.4 GB at 32K chat / 8K embedding context |
| 24 GB | `qwen3.8:27b` | `qwen3-embedding:0.6b` | 22.7 GB at 32K / 8K |
| 32 GB | `qwen3.8:27b` | `qwen3-embedding:0.6b` | 22.7 GB at 32K / 8K |
| 64 GB | `qwen3.8:27b-q8_0` | `qwen3-embedding:0.6b` | 41.3 GB at 256K / 8K |
| 128 GB | `qwen3.8:27b-q8_0` | `qwen3-embedding:0.6b` | 41.3 GB at 256K / 8K |

These are one text model and one embedder per class. The larger classes use
higher precision rather than installing a second text model by default.
Both 27B variants have **27.3 billion parameters**: quantization changes their
weight precision and download size, not their parameter count. The engine
uses precision to break a recommendation tie after checking the hardware
floor. Tags and download sizes were checked on 2026-09-08 against the Ollama
pages for [4B](https://ollama.com/library/qwen3.5:4b),
[9B](https://ollama.com/library/qwen3.5:9b),
[27B Q4](https://ollama.com/library/qwen3.8:27b) and
[27B Q8](https://ollama.com/library/qwen3.8:27b-q8_0).

The below-16 row is the standalone Cockpit setup fallback; the engine's
catalog class ladder still starts at 16. On smaller machines, reduce context
or explicitly choose models that fit the available memory.

`memoryNeedBytes` estimates residency at the stated working context. The
embedder's 2.6 GB allowance includes runtime compute buffers: on 2026-09-08,
Ollama 0.33.3 on an RTX 4090 at 8K with `q8_0` KV allocated
2,416,873,308 GPU bytes, plus about 205 MiB of host buffers.
The conformance gate runs the engine's actual recommendation function over
the embedded seeds, checks every class against this table, and requires the
unique recommended models to fit within 90% of the class. This is a curation
budget, not a runtime reservation: larger prompts, concurrent requests and
other GPU applications can require more memory. Embedding calls use an 8K
working context; chat calls carry their context requirement to the runtime.
In that local run, chat at 32K and a 1024-dimensional embedding each
succeeded, but another application using about 6.5 GiB of VRAM caused
Ollama to evict the chat model before loading the embedder. The run verified
both calls individually; simultaneous residency requires sufficient free
memory and was not demonstrated under that contention.
The native Linux service requests `q8_0` KV cache; Ollama enables Flash
Attention automatically on supported devices, where this reduces cache memory. See [Ollama context settings](https://docs.ollama.com/context-length)
and [cache quantization](https://docs.ollama.com/faq).

Installing and routing answer different questions. The shipped `fast` rule
uses `fleet:fastest` with a 3B effective-parameter floor; strong and reasoning
use `fleet:strongest`. If a fleet offers both a 9B and a 27B dense model, fast
calls can use the smaller model while strong calls use the larger one.
Strongest ordering uses advertised active parameters for mixtures when known,
otherwise total parameters; higher precision breaks equal-size/context ties
for strongest selection. This is a routing heuristic; explicit model
preferences and model pins remain available. See [AI routing](ai-routing.md).

The catalog keeps other models available for manual pulls, including
`gemma4:26b`, `qwen3.6:35b`, `qwen3.5:122b` and `gpt-oss:120b`.
The defaults are a curated starting point, not a claim that one benchmark
predicts every task. `qwen3-embedding:0.6b` remains the cluster's active
embedding binding at every class; change that binding before choosing a
different embedder for cluster work.

A machine that has not reported its memory blocks nothing: unknown is not small,
and telling somebody with an unreported 64 GB laptop that they have no machine
of the class would be confidently wrong with no way for them to tell.

### Adding one yourself

`modelProfileAdd` takes a model id and marks the entry `curated: false`. It
gates nothing either — a machine still has to advertise the model before
anything routes to it — so the blast radius of a wrong entry is a recommendation
nobody can act on.

Removing a **curated** entry is refused rather than performed. Curated rows are
re-seeded on every boot, so a removal would succeed, look correct, and be undone
at the next restart with nothing anywhere to explain it. Retiring one is a
release.

---

## The embedder is a binding, not a setting

The embedding model used to be a string in five files and the vector column was
declared at that model's width. Changing it meant editing five files **and** a
migration, and doing either without the other produced a table of vectors at the
wrong width — which is not an error anywhere. It is a search space that quietly
returns the wrong neighbours.

One row now says which embedder is active
(`v1:platform:embedderBinding` at the id `active`), and the width belongs to the
provider: a provider record declares it, and a fleet model's comes from its
catalog row. Vectors live in one table per width, `node_vectors_<dims>`.

**Switching is not an edit.** It creates the new width's table, records the
binding as a plan, re-embeds the corpus, and flips `active` only when the counts
match. Until then every read follows the binding that is still active — so a
switch interrupted half way leaves a cluster that still works, rather than one
whose vectors half mean one thing and half another.

Two consequences worth knowing:

- **Same width is not same meaning.** `bge-m3` and `qwen3-embedding:0.6b` are
  both 1024 dimensions and share nothing else. Switching between them still
  rebuilds the whole corpus.
- **A model the catalog does not know cannot be bound**, even when a machine
  offers it. This is a real state rather than a hypothetical: a cockpit
  advertises the embedding capability for any model whose runtime reports it. The
  model still serves embedding calls that name it; what it cannot be is the
  cluster-wide binding, because a binding creates its table before the first
  vector exists and a table at a guessed width returns wrong neighbours without
  ever erroring. The refusal says so, and says the machine is fine.

---

## What this does and does not yet prove

The proving suite
([overview/proving](../overview/proving.md)) carries a scenario in which a goal
whose steps are all deterministic is served end to end with **no provider call at
all**, against a control — a goal with a step that must reason — which makes
some. A call never made is never paid for, which is the load-bearing half of
running locally.

<!-- proving-pending: metric=amortizedCost.federationCalls reason=the CI tier replays from a cassette through a fake step registry, so no call reaches the router and no decision record names a door -->

What is **not** proven yet is the other half: that of the calls a cluster *does*
make, none went to a paid vendor. That needs a decision record naming the door
each call took, and the replay tier has no door — both arms play recorded
responses, so "no call went to a paid vendor" and "no call went anywhere" are the
same zero. Reporting it would be a number that reads as the headline result and
measures something else. The live tier can answer it and ships disarmed.
