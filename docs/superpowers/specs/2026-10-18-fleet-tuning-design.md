# Fleet tuning: keeping a model loaded, and taking requests in parallel -- Design

- **Date:** 2026-10-18
- **Status:** proposed. Sub-project S4 of the MemPals program (`memql-znas-mempals`,
  `docs/design/2026-10-18-mempals-program-design.md`, sections 4, 6 item 10 and 7 "VRAM").
  The model bake-off it links to is S1 Task 22 (`docs/design/2026-10-18-mempals-runtime-plan.md`,
  with section 13 of `2026-10-18-mempals-runtime-design.md`). D1-D8 are rulings recorded here for
  the owner to overturn, each with what it costs if wrong.
- **Repositories:** `memql-cockpit` only (this record and its plan,
  `docs/superpowers/plans/2026-10-18-fleet-tuning.md`). No engine change is needed or proposed.
- **Read at:** memql-cockpit origin/main `58ad263` (cited as `file:line`); memql origin/main
  `f0af5373d` for engine behaviour; every machine fact was measured on the owner's laptop on
  2026-10-18, and each figure says whether it was read, measured, derived or estimated.

---

## 1. Problem

The MemPals pals decide through the owner's fleet: one RTX 5090 Laptop (24 GB) running
`qwen3.8:27b` and `qwen3-embedding:0.6b` under the cockpit's own Ollama. A pal's decision has to
land in under three seconds at p95 and falls back to a canned line after eight. Three things in the
fleet path work against that, and all three are visible in the owner's own logs:

1. **A model is unloaded five minutes after its last call**, and reloaded by whoever asks next:
   2.9 s median, 41.7 s worst, for the 27B (section 3).
2. **A model is also reloaded whenever the requested window changes.** The engine rounds every call up
   to a power of two, so the same model is asked for 8192, 16384 and 32768 in turn, and Ollama reloads
   on each change. 107 of the 170 reloads in thirty days were this, not the idle timeout.
3. **One request at a time per model.** 16% of the 666 chat calls started within a second of the
   previous one ending, which under a ceiling of one is a call that waited; 23% ran ten seconds or
   longer. A call that waits the full 90 s is refused as `model_concurrency_exceeded`, which the
   engine treats as a failed call and does not retry elsewhere.

The program design lists the scope as "keep a model resident, parallel requests, per-lane slots,
model bake-off". This record settles the first two, decides the third (deferred, with the evidence
that would reverse it), and says how the bake-off's winner is made resident.

## 2. What the tree does today

All on origin/main `58ad263`.

| Fact | Where |
|---|---|
| Chat sends `model`, `messages`, `stream`, `truncate:false`, `shift:false`, `options`, `format` (the schema, verbatim), `tools`, and `think:false` for structured or `fast` calls. **No `keep_alive`.** | `internal/worker/modelcall/ollama.go:53-81`, `ollamaThink` at `:178-180` |
| Embeddings send `options.num_ctx` 8192 (`embedWorkingContext`) and `num_batch` 512 for the 0.6B. **No `keep_alive`.** | `ollama.go:201-238` |
| `num_ctx` is sent only when the engine sends `context_tokens`; vision is a chat turn; image generation posts `/api/generate`. | `ollama.go:362-364`, `modalities.go:205-216`, `:418-434` |
| The advertised ceiling per model is `OLLAMA_NUM_PARALLEL` **read from the worker's own environment**, else 1, and it is the same number on every model, embedder included. | `internal/worker/models/discover.go:339-355`, `models/ollama.go:69,85` |
| The machine ceiling is the sum of the per-model ceilings, in the registration and in the enforcement alike. | `internal/worker/modelinventory.go:171-188`, `modelcall/session.go:498-504` |
| A call that finds its model full waits up to the call's idle ceiling (90 s by default), keeping the stream alive, then ends `model_concurrency_exceeded`. The Limiter is a semaphore shared by every home. | `modelcall/session.go:440-472`, `runtime.go:108`, `limiter.go:29-62` |
| The managed Ollama unit sets `OLLAMA_HOST`, `OLLAMA_MODELS` and `OLLAMA_KV_CACHE_TYPE=q8_0`. **Not `OLLAMA_NUM_PARALLEL` or `OLLAMA_KEEP_ALIVE`.** It is written once, and never when a runtime is already serving. | `internal/worker/inference/stage.go:222-250`, `:298-317`, `plan.go:467-471` |
| `policy.yaml models` has `allow`, `runtimes`, `pull`. `allow` merges on SIGHUP; the others replace. | `internal/worker/tools/policy.go:203-215`, `:494-510` |
| SIGHUP reloads the policy and asks every home to re-advertise; a re-advertise reconnects only when labels changed and nothing is in flight. | `internal/worker/cli.go:488-496`, `CLAUDE.md` "Local models on the fleet", rule 4 |
| Seven places build a `models.Discoverer`; three build the discovery request from the policy by hand. | `cli.go:372`, `pair.go:235`, `probe_cmd.go:59`, `runtime_cmd.go:45`, `inference_cmd.go:142`, `models_cmd.go:77`, `modelinventory.go:81` |

Engine side, read at memql `f0af5373d`: the context a call needs is `ceil(chars / 4) + 4096`
(`core/airoute/estimate.go:36-49`), rounded up to the next power of two for the chosen machine
(`integrations/agent/worker/fleet_context_allocation.go`, whose comment states the reason: "Ollama
reloads a runner when explicit num_ctx changes"), and the closed set of levels is `fast`, `strong`,
`reasoning`, `embeddings` (`core/airoute/level.go:31-44`). The wire (`ModelCallStart`) carries
`level`, `effort`, `purpose`, `run_id` and `step_id`; **it has no lane or priority field.**

A word that means two things here: `keepalive` in the cockpit is the liveness delta a running call
sends the cluster. Ollama's `keep_alive` is the residency parameter this record is about. They share
nothing.

## 3. Evidence

### 3.1 The owner's fleet Ollama, thirty days (read from `~/.memql/state/ollama.log`)

The log covers 2026-09-18 to 2026-10-18 and the configuration printed at the latest start (2026-10-12) was
`OLLAMA_KEEP_ALIVE:5m0s`, `OLLAMA_NUM_PARALLEL:1`, `OLLAMA_MAX_LOADED_MODELS:0` (auto),
`OLLAMA_KV_CACHE_TYPE:q8_0`, `OLLAMA_CONTEXT_LENGTH:0`.

| Measure | Value |
|---|---|
| `/api/chat` requests | 666 (no `/api/embed` at all) |
| Loads of `qwen3.8:27b` | 171 (5.7 a day; 51 on 2026-10-13) |
| Window at each load (`num_ctx` x slots) | 8192 x1: 78, 16384 x1: 59, 32768 x1: 34 |
| Reload cause, after the first load | window changed: 107; same window: 63 |
| Load time | median 2.9 s, p95 3.8 s, max 41.7 s; 5 over 10 s |
| Call duration | median 1.3 s, p90 16.9 s, p95 22.2 s, max 90 s |
| Calls of 10 s or longer / 30 s or longer | 153 / 20 |
| Calls that began within 1 s of the previous one ending | 106 (16%) |

The census script is in the plan (Task 12); it reads these from the log and can be run again after
a change with a start date.

### 3.2 The memory the 27B takes (read from llama-server's own report in the same log)

At one slot: 15,339 MiB of weights on the GPU (+682 MiB mapped on the CPU), a vision projector of up
to 1,161 MiB, 748 MiB of recurrent state per sequence, a cache of 34 KiB per token at `q8_0` plus
4 KiB per token for the draft model, and 160-260 MiB of compute buffers. The runtime's own
"projected to use" figure for one slot was 16,911 MiB at an 8K window, 16,969 at 16K and 17,673 at
32K, against 22,966 MiB free, and it states that it wants at least 1,936 MiB left ("will leave 6054
>= 1936 MiB of free device memory, no changes needed"). Every load offloaded 66 of 66 layers.

The embedder was never loaded in the thirty days, so its footprint is **estimated** from its
architecture (28 layers, 8 KV heads of 128): about 1.4 GB at one slot and 0.5 GB for each further
one.

### 3.3 What Ollama 0.34.2 does (measured on 2026-10-18 on throwaway instances)

Two Ollama 0.34.2 processes were started on other ports with private homes, once with
`qwen3-embedding:0.6b` from the owner's model directory (shared, pruning off; nothing in it changed) and once with a
pulled `gemma3:270m`, `OLLAMA_NUM_PARALLEL=2`. The owner's fleet Ollama was not touched, and was
checked afterwards (nothing loaded, same GPU memory). Observations:

1. A request that **states** `keep_alive` sets the expiry from that moment, in either direction:
   `forever -> 90s -> 10m -> 30s`.
2. A request that **omits** it leaves the expiry alone (`forever` stayed forever, `30s` stayed `29s`).
   A model whose first request omitted it loads with the five-minute default.
3. JSON number `-1` is forever (`expires_at` 2319-01-28). The string `"-1"` is refused
   (`time: missing unit in duration "-1"`); the string `"-1m"` works.
4. `keep_alive: 0` on `/api/generate` unloads (`done_reason: "unload"`).
5. An empty `/api/generate` with `keep_alive` and `options.num_ctx` loads the model
   (`done_reason: "load"`); the runner starts with `-c <num_ctx x slots> -np <slots>`, and
   `/api/ps` reports `context_length` per slot.
6. A request with a **different** `num_ctx` reloads the runner, **smaller as well as larger**
   (4096 -> 8192 -> 2048: three starts for three calls).
7. A request at the **same** `num_ctx` does not reload, and does not move the expiry if it omits
   `keep_alive`.

The cockpit's own client, built from this plan, was then driven against the real `gemma3:270m` with
`working_context: 4096` and `keep_alive: 2h`: six calls whose engine windows were 2048, 4096, 1024,
4096, 8192 and 2048 produced three loads (the first, the 8192 call that needed more than the window,
and the return), every one left the model expiring in 2 h, and the four calls that fit ran at 4096.
The same six calls with nothing configured produced six loads and a five-minute expiry.

### 3.4 What shares the card right now (read at the time of writing)

The 24 GB card (24,463 MiB) held 17,486 MiB used and 6,496 MiB free, and **none of it was the fleet's
Ollama**, which had nothing loaded:

| Tenant | Holds | Configuration |
|---|---|---|
| `rmf-ollama` (Docker `ollama/ollama`, the RMF stack's brain today) | `qwen2.5:14b-instruct` 15.7 GB at a 32K window x 2 slots, plus `nomic-embed-text` | `OLLAMA_KEEP_ALIVE=-1`, `NUM_PARALLEL=2`, `MAX_LOADED_MODELS=3`, `CONTEXT_LENGTH=32768` |
| `rmf-whisper` (GPU speech recognition) | about 1.5 GB | |
| the owner's fleet Ollama | nothing | defaults (5 minutes, 1 slot) |

The 27B needs about 17.7 GB at one slot. It cannot be loaded fully on the GPU while RMF's 14B stays
resident, and section 6 says what that means for the order of work.

## 4. Decisions

### D1 -- `keep_alive` travels with the call, per model, from `policy.yaml`; the managed unit does not set `OLLAMA_KEEP_ALIVE`

**Ruling.** `models.resident[].keep_alive` is put on every Ollama route by the client (chat, vision,
embeddings, image generation), resolved for each call from the live inventory, so a SIGHUP reaches the
next discovery refresh (at most 90 s) and nothing restarts. The unit keeps no `OLLAMA_KEEP_ALIVE` by
default; an owner may add one as a safety net (section 8).

**Why.** It is per model: the 27B for two hours and the embedder for two hours are different asks
from "everything for two hours". It needs no restart, where the environment variable needs
`systemctl --user restart memql-ollama.service`, which cancels in-flight generations and unloads every
model. It reaches runtimes the cockpit does not manage (Docker, macOS). And every route carries it
because Ollama applies what a request states and treats an omission by release (3.3, items 1 and 2):
a residency that depends on which route touched the model first is not the one the owner asked for.

**Cost if wrong.** A different client of the same Ollama that states a shorter `keep_alive` shortens
the residency. The unit is loopback-only and written for the cockpit, so today there is none; the
remedy is one more line in the drop-in that already carries the slot count.

### D2 -- A resident model also gets a working window

**Ruling.** `models.resident[].working_context` is a floor on `num_ctx`: a call that needs no more runs
at exactly it, a call that needs more runs at what it asked for, and a call with no stated window runs
at it. It does not apply to embeddings, which always run at 8192.

**Why.** `keep_alive` alone leaves 107 of 170 measured reloads (3.1) and the experiment in 3.3
shows the cure end to end. Never clamping down is not caution: a truncated prompt is a wrong answer.

**Cost if wrong.** The window is per slot, so it is also the memory knob (section 5). A window larger
than the engine needs costs cache (272 MiB for 8K at `q8_0` on the 27B) and nothing else that was
measurable. A window smaller than the engine's biggest bucket costs a reload for those calls and one to
come back; in the thirty days 20% of the loads were at 32K.

### D3 -- Each runtime has one source for its slot count; the managed unit is the source, the policy overrides

**Ruling.** The per-model ceiling is, in order: `models.parallel` in `policy.yaml`; `OLLAMA_NUM_PARALLEL`
in the worker's environment (the only knob there was, so it keeps its place); the `OLLAMA_NUM_PARALLEL`
of the unit the cockpit wrote, drop-ins included; one. Never zero, which the engine reads as unlimited.
A fresh native install on a 24 or 32 GB machine writes `OLLAMA_NUM_PARALLEL=2`; every other class writes
what it always wrote. A runtime that is already serving is never rewritten.

**Why.** Ollama reads its slot count from the Ollama process's environment and the worker advertises
from its own, and the two are separate systemd services: setting the variable where Ollama reads it
changed nothing the engine heard, and setting it where the worker reads it changed nothing Ollama did.
Making the unit the source puts both on one line, so on the laptop one edit and one Ollama restart
change both, and the label follows within a refresh without a worker restart.

**Why two, and only there.** Section 5: two slots at a 16K window leave 2.7 GB on this card, two at 32K
leave 1.4 GB, under the 1.9 GB the runtime wants to leave. Only the 24 GB class was measured, so only the
24 and 32 classes move; one is Ollama's own default and costs nothing to keep.

**Cost if wrong.** The sources can disagree with the process that is actually running (the unit is edited,
Ollama has not restarted). That is benign both ways: a call admitted past the runtime's slots queues
inside Ollama, and a ceiling below them wastes a slot until the next refresh. `memql worker models`
prints which source answered.

### D4 -- No warm-up

**Ruling.** The cockpit never loads a model on its own initiative. The first call loads it, or the owner
warms it with one request after checking the card (section 8). S3's runtime may send a no-op decision
when a player joins.

**Why.** On this machine another process holds 17 GB of the card (3.4). A warm-up at worker start would
load the 27B into the 6.5 GB left, the runtime would offload most of it to the CPU, and `keep_alive`
would then make that placement permanent. Residency makes a bad load last, so the load is the owner's.

**Cost if wrong.** The first call after an idle expiry or an Ollama restart pays a load (2.9 s median).

### D5 -- The settings ride `models.Info`, never the label

**Ruling.** `keep_alive` and `working_context` are fields on `models.Info`, not attributes. The label
changes in `max=` and nowhere else, so `TestWireContract` and the engine are untouched.

**Why.** The engine parses `model:<id>` and skips a key it does not know, silently. A setting on the label
would be one the engine can neither read nor reject.

### D6 -- No per-level slot reservation now; the wait line is the evidence

**Ruling.** Not built. What is built is the evidence: a call that waits a second or more for a slot logs
`model call waited for a concurrency slot` at info with its model, `call_level`, purpose and wait, and a call
that is refused after waiting logs `model call refused after waiting for a concurrency slot` at warn.
Both were debug lines before. `call_level`, not `level`: slog's JSON handler already writes `level` for the
severity, and a second `level` key made parsers read the call's level as the severity.

**Why.** A reservation needs something to key on, and the wire has `level` and `purpose` but no lane.
Whether MemPals' decision prompts will arrive as `fast` is S3's choice, and S2 has an open seam for a
settable lane; a mechanism built before either is a guess at the other's shape. And with two slots the
case it protects against needs two long background calls at once, which the log cannot yet show.

**What would reverse it.** Both of: (a) the cockpit can recognise a MemPals decision (`level` `fast`, or a
`purpose` S3 names), and (b) over a week the new lines show fast-level calls waiting two seconds or more on
1% of fast calls, or any refusal for a fast call. **Sketch**, about 60 lines and their tests:
`Limiter.tryAcquire` takes a lane flag and a reserve; a non-fast call may hold at most `cap - reserve`
slots of a model; the reserve is `models.reserve_fast`, default 0, honoured only when `parallel >= 2`.

### D7 -- The laptop profile, and who owns the card

**Ruling.** On this machine: `parallel: 2`; `resident` the bake-off winner (default the 27B) with
`keep_alive: 2h` and `working_context: 16384`, and the embedder with `keep_alive: 2h`.

**Why 2h and not `-1`.** The card is shared. A model loaded while another process held memory stays
where it was put until it reloads; a finite residency bounds how long a bad placement lasts, and a
play session resets the clock on every decision. On a card with no other tenant, `-1` is right.

**Why 2 x 16K.** It is the largest profile whose projection leaves the runtime's margin (section 5).
The alternative, 1 x 32K, leaves more room and no concurrency; the owner's traffic is bursty (9 calls
began inside one minute at most) and 16% of calls began as the previous one ended.

**Preconditions (not code).** The card must be free of RMF's 14B and whisper before the 27B is made
resident, and the order depends on the phase: during the S1 live soak RMF's Ollama is the pals' brain and
owns the card; after S3 moves the brain to the fleet, RMF's models are unloaded and the fleet owns it.
Unloading RMF's model interrupts the pals that are running on it, so it is done when nobody is playing.

### D8 -- Two stages, ordered by what they disturb

**Ruling.** Stage A installs the branch build with nothing configured. It changes no behaviour and costs
one worker restart. Stage B applies the winner's profile: a SIGHUP, then an Ollama restart for the slots,
then a warm-up the owner runs. Every step is labelled with what it interrupts (section 8).

**Why.** The worker serves two clusters, runs CI pipeline steps and app sessions, and has no drain; its
restart cancels all of it. The tuning that matters most (SIGHUP) needs no restart at all, so the one
restart is spent on shipping the code, not on configuring it.

## 5. Configuration

```yaml
models:
  allow: [qwen3.8:27b, qwen3-embedding:0.6b]   # unchanged
  parallel: 2                                  # 1 to 64; replaces on reload
  resident:                                    # replaces on reload
    - id: qwen3.8:27b
      keep_alive: 2h          # 30m, 2h, 3600 (seconds), -1 or forever; never 0; at most 30 days
      working_context: 16384  # tokens, 1 to 1048576; per slot
    - id: qwen3-embedding:0.6b
      keep_alive: 2h
```

An entry that cannot stand is reported and ignored, never an error: no id; the same id twice (the first
stands); a `keep_alive` that is zero, unreadable or past 30 days (the window beside it still stands); a
window out of range; an entry that sets neither; a model that `allow` does not list (kept, reported). The
worker logs each at start and at SIGHUP, and `memql worker models` prints them. Discovery adds notes for an
entry whose model is not installed, is served by a runtime with no `keep_alive`, or is an embedder given a
window. A window is capped at the model's own context.

**The budget.** In MiB, for `qwen3.8:27b` (Q4_K_M, `q8_0` cache) with its embedder, on the 24 GB laptop,
22,966 free, 1,936 wanted left. The one-slot rows of the 27B are read from the report; every further slot
adds 748 MiB of recurrent state plus 38 KiB per token of window; the embedder is estimated.

| slots x window | 27B | with the embedder | left |
|---|---|---|---|
| 1 x 32K | 17,673 | 19,073 | 3,893 |
| 2 x 8K | 17,963 | 19,863 | 3,103 |
| **2 x 16K** | 18,325 | 20,225 | **2,741** |
| 2 x 32K | 19,637 | 21,537 | 1,429 |
| 3 x 16K | 19,681 | 22,081 | 885 |

A choice holds if the load's own report says `no changes needed` and `offloaded 66/66 layers to GPU`, and
`/api/ps` shows `size_vram` equal to `size`. Below the margin the runtime would start taking layers off the
GPU (derived from its stated target; not observed on this card), which is why 2 x 32K is not the profile.

## 6. The shared card, and the order of work

The residency settings are safe to ship before the card is free, because configuring nothing changes
nothing. Making the 27B resident is not safe until RMF's 14B and whisper are off the card.

| Phase | Who owns the card | Fleet Ollama |
|---|---|---|
| S1 live soak and the bake-off (now) | RMF's Ollama: pals decide on `qwen2.5:14b-instruct`, resident | Stage A only; the winner is measured against the fleet Ollama with RMF's model unloaded for the run |
| S3 cutover | The fleet: RMF's 14B is unloaded or its Ollama stopped | Stage B: winner and embedder resident |
| A dedicated small winner (`qwen3.5:9b`, about 9 GB) | The 9B and RMF's 14B (15.7 GB) do not fit together with the embedder and whisper, so the 14B still has to go | Stage B with the 9B as the only chat model on the card |

A resident model is not a pinned one. Ollama evicts an idle model when another must load and does not fit,
so a resident 9B is displaced by one call for the 27B. If the pals' model is the small one and must stay, the
27B comes out of `models.allow`, and the fleet then serves only the 9B and the embedder.

## 7. The bake-off and the winner

S1 Task 22 replays recorded decisions against `qwen2.5:14b-instruct`, `qwen3.5:9b` and `qwen3.8:27b` and
picks the best valid-plan rate among models with p95 under 3 s. Three adjustments make its numbers the
numbers production will see:

1. Run each candidate at the profile it would be made resident at: `numCtx` equal to the planned
   `working_context`, and the slots the profile gives. A call that reloads the model is a measurement of a
   reload.
2. Warm each model first and drop that call, and run the 27B only with the card free of RMF's model (S1's
   step already says to check `ollama ps`).
3. If the pals will share the model with the owner's other work, run once with a second long call in
   flight: that is what the slot count buys.

Then the winner is applied by the same four steps whatever it is:

| Winner | Pull and allow | `resident` | `parallel` |
|---|---|---|---|
| `qwen3.8:27b` (already on the fleet) | none | 27B and embedder, `2h`, window 16384 | 2 (the table) |
| `qwen3.5:9b` | `memql worker models --pull qwen3.5:9b --allow qwen3.5:9b`; take the 27B out of `allow` if the 9B must stay | the 9B and embedder | read the first slot and the per-slot cost from the load report, then the largest count whose "left" stays above the wanted margin |
| `qwen2.5:14b-instruct` | pull and allow it into the fleet Ollama (it is RMF's today) | that model and the embedder | as above |

The id in `resident[].id` is the id S3 pins in `@defaultProvider("fleet:<id>")`, byte for byte, and it has to
appear as a `model:<id>` label in `memql worker models`.

## 8. Local rollout

**How this laptop runs the fleet** (read on 2026-10-18, all read-only). Two systemd user units, both enabled:

- `memql-ollama.service`: `ExecStart=/usr/bin/env ~/.memql/ollama/runtime/bin/ollama serve`, with
  `OLLAMA_HOST=127.0.0.1:11434`, `OLLAMA_MODELS=~/.memql/ollama/models`, `OLLAMA_KV_CACHE_TYPE=q8_0`
  and nothing else; `Restart=on-failure`; output appended to `~/.memql/state/ollama.log`. Ollama
  0.34.2, up since 2026-10-12. It is the file `Stage.Unit()` writes, and it has no drop-in directory.
- `memql-worker.service`: `ExecStart=~/.memql/bin/memql worker run`, where `memql` is a symlink to
  `memql-computeruse-linux-amd64`, version `0.20.0+g58ad26323197 (computeruse)`; output in
  `~/.memql/state/worker.log`; `EnvironmentFile=-~/.memql/worker.env` (empty). It holds two homes,
  `api.memql.znas.io` and `api.memql.localhost`, and its environment has no `OLLAMA_*` variable, which is
  why it advertises `max=1` today. `~/.memql/policy.yaml` allows the two models, CI pipeline steps and
  workbenches. `memql-menu.service` is the tray companion on the same binary.

So the settings are applied in three different ways, and the plan keeps them apart: `policy.yaml` plus a
SIGHUP to the worker (no restart), a drop-in plus a restart of `memql-ollama.service` (Ollama only), and a
binary swap plus a restart of `memql-worker.service` (both homes). The drop-in that carries the slot
count may also carry `OLLAMA_KEEP_ALIVE=2h`, the safety net of D1 for any client that does not state a
`keep_alive`; the plan includes it commented out.

Detail and exact commands are in the plan (Tasks 12 and 13). What each step disturbs, on the owner's
laptop:

| Step | Disturbs |
|---|---|
| Build in a pinned layout; run the suite; `memql worker models` from the new binary | nothing (the sibling `../memql` is stale and cannot build `tools`, `inference` or `worker`; a pinned worktree and `-modfile` avoid touching it) |
| Install the computer-use build's dependencies with `sudo apt` (once) | nothing running |
| Swap the binary and restart `memql-worker.service` | **both clusters' connections, CI pipeline steps, app sessions, in-flight model calls; there is no drain.** Check the unit's cgroup shows only the main process first |
| Restart `memql-menu.service` | the tray icon |
| Edit `policy.yaml` and send SIGHUP | nothing; a changed `max=` re-registers every home when idle |
| Unit drop-in, `daemon-reload` | nothing until Ollama restarts |
| Restart `memql-ollama.service` | **generations in flight and every loaded model** (a few seconds) |
| Unload RMF's model, or stop `rmf-ollama` / `rmf-whisper` | **the RMF stack, which is the pals' brain today** |
| Warm-up request | puts 20 GB on the card |

Rollback is the reverse and costs the same: delete the `resident` block and SIGHUP (instant, nothing
restarts); remove the drop-in, `daemon-reload`, restart Ollama; `mv` the old binary back and restart the
worker. The old binary is copied to the fleet-tuning state directory before the swap.

**Acceptance**, each measurable on the laptop: after Stage B and a play session, `memql worker models`
shows the 27B and the embedder loaded, 100% on the GPU, expiring about two hours out; the census shows no
window-change reload at or below 16K (a call the engine sizes at 32K still loads at 2 x 32K; plan Task 13
Step 8 checks that load) and at most one or two a day for anything else (baseline 5.7 a day, 63% window
changes); no `model call refused after waiting` line for a fast-level call in a week; and the S1 telemetry's
addressed-reply p95 stays under three seconds.

## 9. Deferred and out of scope

- **Per-level slot reservation** (D6), until the lane exists and the log asks for it.
- **A warm-up**, by the cockpit (D4).
- **Rewriting an existing unit.** Setup never touches a serving runtime, and keeps that; the doc says how to
  edit one. The uninstaller removes the unit and leaves a drop-in directory behind, harmlessly.
- **Docker and macOS** runtimes: `models.parallel` and `models.resident` work there, and the slot count is set
  where that runtime reads it.
- **Engine changes**: a lane or priority on `ModelCallStart`, rerouting a refusal made before start (the
  `CLAUDE.md` note about the model ceiling waiting rather than refusing says when to revisit).
- **Multiple GPUs**, automatic profile selection from the fit report, and the vision projector's 1.1 GB
  (`--no-mmproj-offload`) are not touched.

## 10. Testing

TDD per task, with the focused package run first and the whole suite at the end. The pieces that cannot
fail visibly in production are pinned by a test each:

- every Ollama route carries `keep_alive`, forever is a JSON number, the window never clamps down;
- deleting the block and sending SIGHUP returns every model to defaults;
- the ceiling is never zero whichever source is garbage; the unit's drop-ins merge as systemd merges them;
- only one place builds a discoverer or a request (an AST walk, like the Runner and SIGHUP guards);
- an unreadable `/api/ps` is a line in the report, not an error;
- two fuzz targets assert properties, as the repository's other fuzz targets do: `FuzzParseKeepAlive`
  (a setting that parses is one Ollama reads back as the same duration; a refusal returns nothing) and
  `FuzzUnitParallel` (never negative; an empty `Environment=` always resets; a comment never changes the
  answer). CI discovers them without being told.

The plan was replayed on a pristine copy of origin/main before it was written: for each task the test failed
before the change and passed after it, `gofmt` and `go vet` were clean, the package suites passed, and the
resulting tree is identical to the one the plan describes.

## 11. Risks

- **The shared card.** The biggest, and operational: section 6. Mitigations are the bounded residency, the
  GPU-share line in the report, and doing Stage B only on a free card.
- **The worker restart** (D8): it interrupts everything on the machine. Stage A is a single, scheduled
  restart; check the cgroup first.
- **Two slots slow each stream when both are busy** (decode is memory-bound; expect 10-25% on the second
  stream, not measured here). The S1 target is p95 under three seconds; the bake-off is where to see it.
- **The computer-use build needs X11 development packages** this laptop does not have. The existing binary
  was built elsewhere. Installing them is a one-time `sudo` the owner runs; installing the headless build
  instead would drop the capability the machine advertises.
- **Engine behaviour is read from origin/main, not from what the cluster runs.** The window rule
  (`max(requested, window)`) holds whatever the engine rounds to.
- **Ollama versions differ on omitted `keep_alive`** (3.3). Every route states it, so none of them matters.
- **The unit and the process disagree** between an edit and the restart (D3); benign, and shown in the report.

## 12. Questions for the owner

1. Is the S1 live soak still on RMF's Ollama? Stage B waits for the answer, because it needs the card.
2. May the one-time `sudo apt-get install` of the X11 development packages be run, so the branch is installed
   as the same computer-use variant?
3. Is `2h` and 2 x 16K the right profile, or `-1` and 1 x 32K? The table above is the whole trade.
