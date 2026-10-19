# Local models on the fleet — the machine side

The cockpit can serve MemQL's own operations — planning, conductor/routing,
suggestions, embeddings — from a model running on **this** machine, over the
worker stream it already holds open. Nothing is billed per token and no prompt
leaves the hardware.

The canonical record is the engine's
[`docs/public/operate/local-models.md`](https://github.com/znasllc-io/memql/blob/main/docs/public/operate/local-models.md)
(epic [memql#4676](https://github.com/znasllc-io/memql/issues/4676)). This page
is the half that happens on the machine: what has to be true before it offers
anything, what it tells the cluster, and what to check when it offers nothing.

Two commands are worth knowing first:

```bash
memql worker setup --inference
memql worker models
```

The first turns a bare machine into an inference machine: it checks the
hardware floor, installs the model runtime this platform can actually serve
from (after printing the exact commands and asking), pulls the default
models, writes `models.allow`, and signals the running worker. The second
prints exactly what this machine would advertise, or the reason it would
advertise nothing.

---

## Three things must be true

Nothing is on by default, and each of the three fails in a way that looks
identical from the portal — the machine is simply absent from the model list.
That is what `memql worker models` exists to disambiguate.

### 1. The hardware floor

| Platform | Minimum (supported) | Recommended |
|---|---|---|
| macOS | Apple Silicon (M1+), 16 GB unified memory, macOS 13+ | M2 Pro+ / 32 GB for 8B-class at comfortable latency |
| Linux | x86_64 + discrete GPU with ≥ 8 GB VRAM (CUDA/ROCm) | 12–16 GB VRAM |
| CPU-only / Intel Mac | Not an inference machine | — |

A machine below the floor stays a **full worker for everything else** — shell,
filesystem, HTTP, computer use, local apps — and nothing about it is degraded.
It simply does not appear in the model catalog.

The check runs here rather than in the cluster because only this machine can
see its own GPU; a central check would be guessing from a hostname. On macOS
it reads `hw.optional.arm64`, `hw.memsize` and `kern.osproductversion`; on
Linux it asks `nvidia-smi`, then the amdgpu driver's
`mem_info_vram_total`. **A fact that cannot be established is not a fact in
this machine's favour** — an unreadable probe means "not offered", with the
reason named.

### 2. A runtime with a model in it

**Ollama is discovered natively**, at `http://127.0.0.1:11434` or wherever
`OLLAMA_HOST` points (`host:port`, `http://host:port` and a bare host all
work). `/api/tags` says what is installed; `/api/show` says what each one can
do.

Getting it there used to be five manual steps — install, start, pull, pull,
edit `policy.yaml` — of which an operator following the old instructions
reliably did the first one and stopped. It is now one command:

```bash
memql worker setup --inference
```

It prints what it is about to do before it does it, asks before changing
anything on the machine, shows byte counts while it pulls, and ends by
printing what the cluster will see. Nothing in it runs `sudo`: where a fix
needs root, the command is printed for **you** to run and is labelled as
such.

| Flag | What it does |
|---|---|
| `--model <id>` | Pull this model instead of the defaults. Repeatable. Hugging Face ids work unchanged: `hf.co/<owner>/<repo>` and `hf.co/<owner>/<repo>:<quant>` are resolved by Ollama itself. |
| `--non-interactive` | Never ask. A runtime install it would have asked about is refused with **exit 3** and nothing is installed. |
| `--runtime docker\|native` | `native` is every platform's own default and changes nothing. `docker` on Linux chooses the `ollama/ollama` container instead of the user-space runtime; on macOS it is **refused**, not ignored, because a container has no access to the GPU there. |

The runtime it installs is fixed by the platform, and the reason is the
hardware floor:

- **macOS (Apple Silicon)** — Ollama natively, through Homebrew
  (`brew install ollama`, `brew services start ollama`). Docker is not
  offered: a container has no access to the GPU there, so Ollama in a
  container would serve from the CPU and this machine would not be
  advertised at all.
- **Linux (discrete GPU)** — Ollama **as your user, from your own home
  directory**: the vendor's release archive (`ollama-linux-<arch>.tar.zst`,
  plus the ROCm add-on on an AMD machine) is downloaded from the
  `ollama/ollama` GitHub release, checked against that release's
  `sha256sum.txt`, unpacked into `~/.memql/ollama/runtime`, and kept
  running by a user systemd unit, `memql-ollama.service`, that binds the
  **loopback only** and keeps its models in `~/.memql/ollama/models`
  (`OLLAMA_MODELS` in your environment wins). It reaches the GPU through the
  same device nodes `nvidia-smi` used to pass the floor, so **nothing here
  needs root**: no container toolkit, no Docker, no daemon restart, no
  package repository. Its log is `~/.memql/state/ollama.log`. The command
  says everything it will download and write before it asks, and a
  `--runtime docker` run takes the container instead -- which does need the
  NVIDIA container toolkit (a root install and a Docker restart), and refuses
  without it, naming the package and the way out.

The uninstaller is the other half: `uninstall-linux.sh` stops and removes
`memql-ollama.service` with the worker's own unit, and `--purge` removes
`~/.memql/ollama` -- the runtime and every model in it -- with the rest.
Without `--purge` the models stay, because they were pulled on purpose and
cost hours to pull again, and the closing block says so.

Afterwards, a single model at a time:

```bash
memql worker models --pull qwen2.5:7b     # fetch it; changes no policy
memql worker models --allow qwen2.5:7b    # offer it, and signal the worker
```

`--pull` deliberately does not write `models.allow`. Pulling spends
bandwidth and disk; allowing spends this machine's GPU on somebody else's
prompt, and a command that quietly granted the second would be a policy
change nobody typed.

The manual route still works and is still supported — `brew install ollama`,
`ollama serve`, `ollama pull llama3.1:8b` — the command above simply does
all of it, including the `policy.yaml` edit that the manual route leaves you
to remember.

**Any OpenAI-compatible endpoint** works too — LM Studio, vLLM, llamafile, and
Ollama's own `/v1` surface — but it has to be **declared**, because
`/v1/models` returns ids and nothing else. See below.

No runtime is not an error. Most machines in a fleet have none, and that means
"this machine offers no models" — a fact the worker reports, not a condition it
logs every heartbeat.

### 3. `models.allow` in `~/.memql/policy.yaml`

**Default-deny**, the same posture app consent (`apps.homes`) has and for the same reason:
serving a call spends this machine's own GPU on somebody else's prompt.
An empty allow list is the state of every machine upgrading into this feature,
and it does not mean "all".

```yaml
models:
  allow:
    - llama3.1:8b
    - nomic-embed-text

  # Optional. Default TRUE, unlike everything else here: a pull is the
  # machine's own owner acting on their own machine, and a fleet-wide
  # silent refusal is a worse failure than a pull somebody did not want.
  # Set it to false to refuse `--pull` and `setup --inference` outright.
  pull: true

  # Optional: OpenAI-compatible endpoints, and what they can do.
  runtimes:
    - name: lmstudio
      base_url: http://127.0.0.1:1234/v1
      api_key_env: LMSTUDIO_KEY      # optional; the VALUE is never logged
      models:
        - id: qwen2.5-7b-instruct
          context_window: 32768
          structured_output: true
          max_concurrent: 2
          # Ranked and routed on. UNDECLARED IS ABSENT for all three:
          # an unstated size sorts last rather than claiming zero, and
          # an unstated `tools` keeps the model out of tool turns.
          params: 7620000000     # the COUNT, not "7.6B" -- ranked numerically
          quant: Q4_K_M          # verbatim, as `ollama list` spells it
          tools: true            # this runtime can carry a tool-calling turn
```

`params`, `quant` and `tools` are spelled here exactly as the **label**
spells them, unlike `context_window` and `structured_output`: you compare
`quant=Q4_K_M` on the Fleet page against this file, whereas `ctx` and
`structured` are abbreviations nobody would guess from a policy.yaml.

`SIGHUP` reloads it — `kill -HUP $(pgrep -f 'memql worker run')` — so a newly
pulled model becomes offerable without a restart. `memql worker setup
--inference` and `memql worker models --allow` send it for you.

A model that is present but **not** listed is still reported, marked blocked.
That is what lets the portal say "present, blocked" instead of rendering it
identically to "not installed": one of those you can fix, the other sends you
looking for the wrong problem.

---

## What this machine tells the cluster

Models ride the **existing** registration mechanism — there is no second
channel — as labels the engine's fleet router selects on:

```
capability   MODEL
label        model:qwen3.5:9b       = ctx=262144,structured=1,max=2,params=9000000000,quant=Q4_K_M,tools=1,vision=1
label        model:qwen3-embedding:0.6b = ctx=2048,embeddings=1,max=4,params=600000000,quant=F16
label        runtime:ollama         = 0.13.0
concurrency  MODEL = 6
```

The `runtime:` label's **value is the runtime's version**. A runtime whose
version could not be read keeps the label with an empty value: the label is
the advertisement that the runtime is here, and dropping it over an unreadable
version would hide a runtime that is running.

`memql worker models` prints those exact strings, and beside each one the
same facts spelled for a person — `8B, Q4_K_M, 131072 context, tools,
structured output, max 2 concurrent` — so an operator comparing this machine
against the Fleet page is comparing one rendering, not two that can disagree.
The absences are named there too: a model with `tools: not advertised` is
skipped for every turn that carries tools, and that sentence is the whole
explanation for a machine that is in the catalog and never picked.

`params` is the parameter **count** rather than the string a model card
prints, because the engine ranks on it numerically and "70B" sorts before
"8.0B" as text. `quant` is free text and is the only attribute that could
forge another one, so a level carrying a `,` or an `=` is dropped whole
rather than escaped — it gates nothing, and losing it costs a tiebreak where
emitting it would cost the integrity of every attribute beside it.

The value is a flat `k=v` list rather than JSON because labels are
`map[string]string` end to end — the concept, the wire, the Fleet page — and a
JSON blob inside one would be unreadable in every surface that renders labels
as text.

**Every capability defaults to absent, and the direction is deliberate.** The
engine is fail-closed: a model that says nothing about structured output is
never selected for a structured prompt. A probe that cannot establish a
capability therefore claims nothing — because a model that quietly answers
prose to a conductor turn produces a parse failure three layers away, naming
nothing here.

That has one visible consequence worth knowing. Ollama has no "structured
output" capability of its own, so the cockpit claims it only for models that
report **`tools`** — which covers the operational class (`llama3.1:8b`,
`qwen2.5:7b`). If you disagree about a specific model, declare it under an
OpenAI-compatible runtime pointed at Ollama's own `/v1` surface with
`structured_output: true`. Your machine, your claim.

That escape hatch relies on one rule worth knowing: **a model id can be
advertised once**, because the id is the label key. When the native probe and
a declared runtime both offer the same id, the **declared** entry wins and
`memql worker models` says which one was shadowed. If it went the other way,
the file you just edited would change nothing.

`max_concurrent` is the one attribute that is **never** left absent: the engine
reads a missing ceiling as *unlimited*. It is the first of these that gives a
usable number: `models.parallel` in `policy.yaml`; `OLLAMA_NUM_PARALLEL` in the
worker's environment (on Linux, the `~/.memql/worker.env` its unit loads); the
`OLLAMA_NUM_PARALLEL` of the `memql-ollama.service` unit this cockpit manages,
drop-ins included; and otherwise 1. `memql worker models` names which one
answered. It runs in your shell and the worker does not, so it reads the
worker's environment file rather than its own environment; where there is no
such file (macOS, whose LaunchAgent loads none, or a machine the installer never
touched) it reports its own environment and says "this command's environment
(the worker's own may differ)". It reads the file as it is now, and the worker
took its environment when it started, so restart the worker after editing it.
The worker's log carries the number it really advertises: one `model
concurrency ceiling` line per model it serves, with `model_max` and `source`, at
start and after every `SIGHUP`.
See [Keeping a model loaded](#keeping-a-model-loaded).

### The four modalities

Beyond chat and embeddings, a machine can advertise four more things it can
do. Each is a key on the same label, present only when true — **there is no
`vision=0`**, because a missing key and a stated "no" have to be different
things on the wire:

| key | what it means | where the claim comes from |
|---|---|---|
| `vision=1` | the model accepts images | Ollama's own `/api/show` capability list |
| `imagegen=1` | the model generates images | the same capability list |
| `audioin=1` | the model transcribes audio | **declared only** |
| `audioout=1` | the model produces speech | **declared only** |

Two of the four are real probes and two are not, and the asymmetry is not an
oversight. Ollama reports a `vision` capability and reports **nothing** about
audio in either direction, so there is no honest probe for those two short of
sending real audio to every model on the machine. The alternative — reading a
modality out of a model's **name** — is exactly the failure this rule exists
to prevent: a "kokoro" in an id is not a runtime that answered, and a machine
that advertised speech on that basis takes a call it cannot serve, with the
failure landing on somebody else's prompt.

So a bare Ollama offers `vision` where its models report one, and nothing
else. To advertise transcription or speech, declare the endpoint that serves
them:

```yaml
models:
  allow: ["whisper-base.en", "kokoro-82m"]
  runtimes:
    - name: local-transcription
      base_url: http://127.0.0.1:8080
      transcription: whisper-cpp
      models:
        - id: whisper-base.en
          audio_in: true
    - name: local-speech
      base_url: http://127.0.0.1:8880/v1
      voices:
        male: am_michael
        female: af_heart
      models:
        - id: kokoro-82m
          audio_out: true
```

The keys are spelled `vision`, `audio_in`, `audio_out` and `image_gen` — as
the **label** spells them, so an operator comparing `audioout=1` on the Fleet
page against this file reads one word in both places.

### Serving a modality call

The four kinds — `vision`, `transcribe`, `speak`, `image` — ride the same
`ModelCall` envelope as chat, over the stream this worker already holds open.
The machine's own runtimes serve them. Vision uses OpenAI chat image parts;
transcription uses the declared protocol; speech uses an OpenAI-compatible
`/audio/speech` endpoint; image generation uses the runtime's image route.
Binary inputs, outputs and tool calls are carried on the authenticated worker
stream. An incomplete payload is refused instead of being treated as chat.

`transcription: openai` selects multipart `/audio/transcriptions` with a WAV
file. `transcription: whisper-cpp` selects `/inference` and requires `/health`
to report `status: ok`; use the model actually loaded by whisper-server in the
declaration. Omitting the option retains chat `input_audio` for runtimes that
support it. OpenAI-compatible runtimes must list the declared model in `/models`.
The `voices` map translates MemQL's male/female choices to runtime voice IDs.
Neither transcription nor speech runs in Ollama merely because a model name
resembles Whisper or Kokoro.

For a local Ask voice route, run whisper.cpp with a loaded model, start Kokoro,
and allow both models in policy. A 32 GB or 64 GB Apple Silicon machine can
reserve most memory for its chat model while these smaller audio models run
beside it; actual latency depends on the chat model and concurrent work. On a
24 GB machine, start with a smaller chat model to avoid swapping. Check GPU
VRAM independently of system RAM. NVIDIA Blackwell GPUs require a runtime
image whose CUDA/PyTorch build supports that GPU; do not assume an older
Kokoro GPU image supports an RTX 5090.

The worker only routes to runtimes that answer its probes. Model files and
runtime installation remain explicit operator actions; no credential or model
is downloaded as a side effect of starting a conversation.

### `sharedInference` is not the cockpit's to send

Offering this machine for the cluster's **system** work — the calls with no
acting user — is an **operator label**, set by the machine's owner in the
portal. It is deliberately not read from what the cockpit reports, because
`labels` is overwritten from `Register` on every reconnect: an opt-in stored
there would be granted by the machine rather than by its owner, and revoked
roughly whenever the lid closed. The cockpit never derives one.

### A changed model set costs a reconnect

Model labels are bound at `Register`, and `Heartbeat` carries apps but no
labels — so a model pulled, removed or newly allowed while the worker is
connected is invisible to the cluster until it registers again, and registering
again means reconnecting. A changed ceiling (`max=`) is a changed label too.

The worker re-checks its offered set every 60 s and reconnects only when the
**advertised labels actually changed**, only when no tool call, app session or
model call is in flight, and never twice inside two minutes. A model finishing
its pull will not kill an hour-long app session, and a runtime flapping up and
down will not turn this worker into one that reconnects forever.

A `SIGHUP` is the one thing that shortens the wait, and it shortens exactly
two parts of it. The handler reloads `policy.yaml` — so the machine's own
answer to "may I serve this model" changes at once — and then asks for an
**immediate re-advertise**, which drops the 90-second discovery cache (a
model pulled a moment ago is not in a probe taken a minute before it) and
waives the two-minute floor **once**, waking the loop instead of waiting for
the next 60-second tick. What it does **not** waive is the busy guard: a
reconnect that killed a running model call or an hour-old app session is a
worse outcome than a label that is a minute stale, and somebody watching a
pull is not a reason to throw away somebody else's work. The request survives
that wait.

So the honest sentence after a pull is "the cluster will see it shortly",
never "it is available now" — which is what `memql worker setup --inference`
prints. Without the re-advertise request the reload would change the file and
nothing else, and the model would stay invisible for up to the full two
minutes plus a tick.

### The cluster can ask for the pull

Since the install wizard (engine epic memql#5218, D13) the OS's **Pull the
recommended models** sends a `ModelPullStart` down the stream this worker
already holds open, and the worker runs exactly what `memql worker models
--pull` runs: the pull against the same Ollama the discoverer found, byte
counts forwarded as they arrive, `models.allow` written and reloaded on
success, and the same immediate re-advertise request — so the honest sentence
is still "the cluster will see it shortly". A refusal is a sentence shown in
the OS rather than a silent absence: a build without local-model support, or
`models.pull: false`, answers at once and says so. A cancel from the OS stops
the download; the bytes already fetched are resumed by the next pull of the
same model.

---

## Serving a call

`ModelCallStart` → deltas → `ModelCallEnd`, correlated by request id, on the
stream that is already open. Both kinds: `chat` and `embedding`.

- **Deltas carry a monotonic `seq`.** The engine drops out-of-order and
  duplicate deltas rather than repairing them.
- **The envelope owns the deadlines.** Timeout, idle ceiling and keepalive
  arrive on the call. A local 8B model on a cold GPU is twenty seconds from
  start to first token — indistinguishable from a wedged machine to anything
  holding only a wall clock — so a call with nothing to say still says it, on
  the keepalive cadence the caller set.
- **Cancel is a real cancel.** The in-flight request to the runtime is
  aborted; a call that merely stopped being read would leave the GPU busy for
  the length of a generation nobody will use. A lost stream and a drain do the
  same thing to every live call.
- **Usage is reported, never inferred.** What the runtime said, including which
  model it actually ran. Silence stays silence, which the cluster records as
  billing "unknown"; a count derived from string length would be stored as
  measured.
- **Both concurrency ceilings are enforced here** — per model and machine-wide
  — from the same numbers the registration advertises. The engine rations too,
  but the advertisement is a claim about this hardware and two replicas
  selecting at the same moment is an ordinary race.
- **The ceilings are the whole machine's, not each cluster's.** A machine
  paired with two clusters advertises the same numbers to both, and the calls
  of both count against one ceiling — two clusters cannot each drive the full
  cap at once and run the GPU at twice what it said it could. A call that
  finds the machine full **waits** for a slot, sending keepalives so the
  cluster knows it is alive, for up to its own idle ceiling (90 seconds by
  default) — as long as a call queued inside the runtime could have waited
  — and only then ends with `model_concurrency_exceeded`.

A refusal names its own fix: `model_not_offered`,
`model_concurrency_exceeded`, `schema_unsupported`, `unsupported_kind`,
`duplicate_request`, `runtime_error`, `cancelled`, `timeout`,
`worker_stopped`.

---

## When it does not work

**"My laptop is not in the model list."** Run `memql worker models`. It answers
in the order the causes actually occur:

| What it prints | What to do |
|---|---|
| `Hardware floor: NOT met — …` | Nothing, on this machine. It stays a full worker. |
| `Runtimes: none found.` | Run `memql worker setup --inference`, or declare an endpoint under `models.runtimes`. |
| `present, BLOCKED` | `memql worker models --allow <id>`, which writes `models.allow` and signals the worker. |
| `declared runtime … did not answer` | The endpoint is down. Its models are held back deliberately — advertising them would send prompts to a server that is not there. |
| `declared but not offered` | The endpoint is up but is not currently serving that model id. |
| labels printed, still absent in the portal | The machine is registered but offline, or the change has not cost a reconnect yet. Give it a minute; the worker will not interrupt work in flight to re-advertise. |

And what `memql worker setup --inference` prints when it stops:

| What it prints | What it means, and what to do |
|---|---|
| `Docker is installed but cannot pass this machine's NVIDIA GPU into a container` | Only under `--runtime docker`. Docker is there and the container toolkit is not, so a container would serve from the CPU — which is what the hardware floor exists to prevent, so the setup refuses rather than build one. Either drop the flag, which is the sentence's own last line — the default runs Ollama as your user and needs none of it — or install `nvidia-container-toolkit`, run `nvidia-ctk runtime configure --runtime=docker`, restart Docker (which restarts every container on the machine, a k3d cluster included), then run the setup again. Those three need root: **the cockpit prints them for you to run and never runs one itself.** The AMD case names `amdgpu-dkms` and `/dev/kfd` instead — there is no "ROCm container toolkit" to install. Exit **4**. |
| `The systemd user manager is not available on this machine` | No `systemctl` on PATH, so nothing would keep Ollama running across a login. Install Ollama yourself from ollama.com and start it, or use `--runtime docker`. Exit **4**. |
| `your user cannot open /dev/kfd and a /dev/dri render node` (AMD) or `cannot open /dev/nvidiactl` (NVIDIA) | The GPU is there and this user cannot reach it, so a runtime started now would serve from the CPU. The AMD fix is the `render` and `video` groups (`sudo usermod -aG render,video $USER`, then log back in); the NVIDIA one is the device's permissions. Printed for **you** to run. Exit **4**. |
| `OLLAMA_HOST selects …, but native setup installs a loopback service` | The chosen endpoint is not serving, and differs from the native installer's `127.0.0.1:11434`. Start that runtime yourself, or unset `OLLAMA_HOST` and retry. Setup refuses before downloading or writing anything. Exit **4**. |
| `the downloaded archive does not match the release's checksum` | The bytes fetched are not the ones the release's `sha256sum.txt` describes — a release being published mid-download, a proxy, or a tampered mirror. Nothing was unpacked and nothing was written; run the setup again. Exit **5**. |
| `the runtime was installed and started, but nothing answered at http://127.0.0.1:11434 within 30s` | The unit was enabled and Ollama did not come up. Read the log the sentence names, `~/.memql/state/ollama.log`; `systemctl --user status memql-ollama.service` shows the unit's own view. Exit **5**. |
| `This machine will not pull a model: models.pull is false in …/policy.yaml` | The pull switch is off in this machine's own policy. It defaults to **true** — a pull is the machine's owner acting on their own machine — so somebody set it deliberately. Remove the key or set it to `true`. Exit **4**. |
| `Nothing was installed: --non-interactive cannot answer that question.` | A runtime install was needed and a scripted run may not approve one. Nothing was changed. Run the same command **without** `--non-interactive`, in a terminal, or run the printed commands yourself. Exit **3** — which is what the installers' `--inference` watches for, so they can print the interactive command for you. |
| the pull finished, the closing block listed the model, and the portal still shows nothing | **Not a failure, and not due yet.** Model labels are bound at `Register`, so the cluster sees a newly allowed model only after the worker reconnects. The `SIGHUP` the setup sends arms that reconnect at once, but it still waits for any tool call, app session or model call in flight. A minute or two on an idle machine; longer on a busy one. Nothing is lost — the request survives the wait. |

**A model is offered but never picked.** Read its attribute line. A model with
`structured output: not advertised` is passed over for every planner,
conductor and suggest prompt; one with `tools: not advertised` is skipped for
every turn that carries tools; one with `context not advertised` meets no
context floor. All three are the fail-closed rule working as intended — the
fix is a runtime that reports the capability, or a declared runtime where you
state it. `size not advertised` is the mild one: `params` gates nothing, it
only ranks, and a model that states no size sorts **last** rather than being
excluded.

---

## From the install line

The portal's "Add machine" flow appends `--inference` to the one-line install
command when the machine is meant to run local models. Both installers pass
it through: after `worker.yaml` is written and the service is running, they
run `memql worker setup --inference --non-interactive`.

They **never fail the install over it.** A machine that paired fine and could
not set up local models is still a working worker — shell, filesystem, HTTP,
computer use, local apps, backup — and aborting over the one capability it
could not add would take away the eight it already has. So a non-zero exit is
reported and the install carries on, with one case answered specially: **exit
3** means a runtime install a scripted run may not approve, so the installer
prints the interactive command for the person who is standing at that
terminal right now.

---

## What this machine says it IS

`memql worker hardware` prints the inventory the cockpit reports on `Register`
and refreshes on every tenth heartbeat — chip, memory, GPU with its backend,
CPU cores, OS, free disk on the volume the runtime keeps models on, and every
model runtime it found with the version that runtime reported.

```
  Chip         Apple M3 Max
  Memory       64 GB unified
  GPU          Apple M3 Max, 64 GB, metal
  CPU cores    16
  OS           macOS 15.1
  Disk free    412 GB
  Runtimes     docker 27.3.1, ollama 0.13.0

  Class        32 -- 48.0 GB usable, 75% of 64 GB unified
```

**Presence facts only.** No serial numbers, no user names, no paths, no
hostname — and that is asserted by a test over the field set rather than
by review, because this payload lands on a registration row the owner's
whole cluster can read.

### The class, and what it is not

The class is the largest of 16, 24, 32, 64 and 128 not exceeding **usable**
memory: three quarters of the unified pool on Apple Silicon, because the OS
and everything else the person has open live in the same memory; the whole of
VRAM on a discrete card, because nothing else is in it.

**It is not the hardware floor.** The floor decides whether this machine
serves models at all. The class decides only which set is *recommended* — so a
Linux box with 8 GB of VRAM clears the floor, serves perfectly well, and
classes `unsupported`. That machine still gets a set; it gets the smallest
one.

Usable memory is rounded to the nearest whole gigabyte rather than floored,
and that matters more than it sounds: `nvidia-smi` reports total memory minus
what the driver has already reserved, so a card sold as 24 GB reports about
23.99 GB. Flooring puts every 24 GB machine in class 16 and classes every
16 GB card `unsupported` — systematic, always in the same direction, and
invisible except as a fleet that quietly under-recommends.

### The recommended set

`memql worker setup --inference` pulls the set for this machine's class, in
order, with the embedder last — a run interrupted halfway should leave a
machine with a general model rather than with only an embedder.

| class | set |
|---|---|
| below 16 | `qwen3.5:4b`, `qwen3-embedding:0.6b` |
| 16 | `qwen3.5:9b`, `qwen3-embedding:0.6b` |
| 24, 32 | `qwen3.8:27b`, `qwen3-embedding:0.6b` |
| 64, 128 | `qwen3.8:27b-q8_0`, `qwen3-embedding:0.6b` |

One text model per class plus the cluster's embedder keeps the working set
within the class budget. A bigger class buys a stronger model or the same one at
higher precision. Fast-level calls choose the fastest eligible fleet model after
a quality floor; strong and reasoning calls choose the strongest. The embedder
uses an 8K working context. Chat calls pass the engine's required context window
to Ollama instead of relying on its hardware-tier default. One slot of the 24 GB
pair is budgeted at 32K chat context, and larger contexts can require CPU
offload or model eviction. Setup gives a class 24 or 32 machine two slots, which
fit the pair at windows up to 16K; what a call the engine sizes at 32K costs
with two is under [Requests at once](#requests-at-once). The native Linux unit
uses `q8_0` KV cache, with Flash Attention selected automatically by Ollama on
supported devices.

Setup leaves an already-serving runtime in place. For an older
`memql-ollama.service`, add `Environment=OLLAMA_KV_CACHE_TYPE=q8_0` in its
`[Service]` section, then run `systemctl --user daemon-reload` and
`systemctl --user restart memql-ollama.service` between model calls to apply
the cache setting. A runtime managed separately keeps its own configuration.
The same goes for how many requests the unit lets Ollama take at once and for
keeping models loaded; both are below.

`--model` still overrides, and the closing block still reports what the
**cluster** will see rather than restating the command line.

---

## Keeping a model loaded

Ollama unloads a model five minutes after its last call and loads it again the
next time anybody asks: about three seconds from a warm disk cache and up to
forty from a cold one, for the 27B the 24 GB class serves. Two things cause a
reload, and a model that is meant to stay has to answer both.

- **The idle timeout.** `keep_alive` moves it.
- **A changed window.** Ollama also reloads a model whenever the `num_ctx` a
  call asks for differs from the one it loaded with, larger or smaller, by a
  single token. The engine rounds each call up to the next power of two, so one
  model is asked for 8192, 16384 and 32768 in turn. On the reference laptop,
  107 of the 170 reloads in thirty days were this and not the timeout.
  `working_context` pins it.

```yaml
models:
  allow:
    - qwen3.8:27b
    - qwen3-embedding:0.6b
  parallel: 2                    # Docker, macOS, system Ollama; outranks a unit
  resident:
    - id: qwen3.8:27b
      keep_alive: 2h             # 30m, 2h, 3600 (seconds), or -1 / forever
      working_context: 16384     # every call that fits runs at this window
    - id: qwen3-embedding:0.6b
      keep_alive: 2h
```

A `SIGHUP` applies it; nothing restarts. Delete the block, send another, and
every model is back on Ollama's defaults, though on current releases a model
already loaded keeps its last expiry until it is stopped, evicted, or reloaded.
Nothing here changes a machine until its owner writes it.

- **`keep_alive`** is sent on every call to that model: chat, vision, embedding
  and image alike. Ollama applies the value a request states, up or down, and
  what a request that leaves it out does varies by release, so no route leaves
  it out. `0` is refused (Ollama reads it as "unload after every call"), and so
  is anything past 30 days; `-1` keeps the model loaded until Ollama stops. A
  resident model is not pinned: Ollama still evicts an idle one when another must
  load and does not fit, so a resident 9B is displaced by a call for the 27B
  unless the 27B is not in `models.allow`.
- **`working_context`** is a floor on `num_ctx`: a call that needs no more runs
  at exactly it, and a call that needs more runs at what it asked for. It is the
  window per slot, and the model's cache is sized for the slots times it, so it is
  also the memory knob. Embeddings always run at 8192 and ignore it.
- Neither is a label attribute. The engine is told nothing about them, and the
  advertisement changes only in `max=`.

`memql worker models` prints what is configured, then asks Ollama what it holds:
the size, how much of it is on the GPU, the window it was loaded at, and when it
goes. A model that is partly on the CPU is called out, because residency makes
that permanent: a model loaded while another process held the card stays where
it was put until it reloads. On a card shared with another Ollama or a GPU
service, account for the other tenant's memory before making any model resident.

### Requests at once

`parallel` is the ceiling the worker advertises and enforces for each Ollama
model (a model on a runtime declared under `models.runtimes` takes the
`max_concurrent` its declaration states), and Ollama has to be given the same
number or the extra calls queue inside it. For the unit this cockpit installs,
the unit is the source: a fresh install on a 24 or 32 GB machine writes
`Environment=OLLAMA_NUM_PARALLEL=2`, and the worker reads it back, drop-ins
included. For an existing unit add `Environment=OLLAMA_NUM_PARALLEL=2` under
`[Service]` yourself (a drop-in, `systemctl --user edit memql-ollama.service`,
is the way systemd documents it, and its editor opens without the `[Service]`
header, so write that too), then `systemctl --user daemon-reload` and
`systemctl --user restart memql-ollama.service`. The restart cancels what Ollama
is generating and unloads its models, so do it between calls. `models.parallel`
overrides the unit for a runtime the cockpit cannot read: Docker, macOS, or a
system-wide Ollama (`/etc/systemd/system/ollama.service`), which the cockpit
does not manage either.

Every slot costs memory: a constant for the model's recurrent state plus its
cache for the window. For `qwen3.8:27b` (Q4_K_M, q8_0 cache) on a 24 GB RTX 5090
Laptop, llama-server's own report in `~/.memql/state/ollama.log` gives the first
slot (`projected to use`) and what each further slot adds: 748 MiB of recurrent
state plus 38 KiB of cache per token of window. In MiB, with the card's 22,966
free and the report wanting at least 1,936 left:

| slots x window | 27B | with the embedder | left |
|---|---|---|---|
| 1 x 32K | 17,673 | 19,073 | 3,893 |
| 2 x 8K | 17,963 | 19,863 | 3,103 |
| 2 x 16K | 18,325 | 20,225 | 2,741 |
| 2 x 32K | 19,637 | 21,537 | 1,429 |
| 3 x 16K | 19,681 | 22,081 | 885 |

The one-slot figures at 8K, 16K and 32K (16,911, 16,969 and 17,673 MiB) are read
directly; the other rows add the per-slot costs above to the one-slot figure at
the same window. The embedder column is estimated from its architecture (it was
never loaded in the thirty days measured). The report the runtime prints on
every load says whether a choice held: `no changes needed`, and
`offloaded N/N layers to GPU`.

One slot is budgeted at 32K. The two slots setup gives a class 24 or 32 machine
fit the 27B and its embedder at windows up to 16K, the `working_context: 16384`
above, and leave 2,741 MiB. A call the engine sizes at 32K loads the 27B at
2 x 32K, and with the embedder loaded that leaves 1,429 MiB, under the 1,936 MiB
the runtime wants. The runtime may then move layers to the CPU, or Ollama may
evict the embedder to make room; both follow from the runtime's own target, and
neither has been observed on this card. `working_context` cannot prevent it: it
is a floor, and a call that needs more runs at what it asked for.

If 32K calls are common on this machine, give Ollama one slot:
`Environment=OLLAMA_NUM_PARALLEL=1` under `[Service]` in the unit, then reload
and restart Ollama as above (the worker follows the unit unless
`models.parallel` or its own `OLLAMA_NUM_PARALLEL` is set). `models.parallel: 1`
alone only lowers what the worker advertises; it is for a runtime the cockpit
cannot read.

The ceiling the worker advertises can also differ from what Ollama was started
with: a stale `OLLAMA_NUM_PARALLEL` in the worker's environment outranks the
unit, a `worker.env` edited since the worker started shows in `memql worker
models` before it applies, a unit edited by hand takes effect only when Ollama
restarts, and another Ollama on the port reads none of this. Each is benign: a
call waits inside Ollama, or a slot sits idle until the next refresh.

---

## Measuring a model

`memql worker probe` runs a version-pinned suite against a model this machine
offers and reports what it measured: structured-output validity over five
schemas drawn from the platform's own prompts, tool-call correctness over
three tool definitions, and throughput plus time to first token at an 8K and a
32K prompt.

```
  structured validity      1.00         5 of 5 schemas held
  tool call correctness    0.67         2 of 3 definitions (missed: search)
  throughput 8K            49.8 tok/s   92 tokens in 1.848s
  time to first token 8K   1.04 s       first token after 1.041s
  throughput 32K           --
      the 32K throughput case did not answer within 2m0s and was ended.
```

**A figure and an absence are never the same shape.** A case that scored zero
and a case that could not run are opposite facts — the first says the model
failed, the second says nothing about the model at all — so a measured row
carries a number and an unmeasured one carries a dash with its reason on the
line below. Rendering both as `0` would rank a working model below a broken
one.

**These figures rank; they gate nothing.** A model that fails a case is still
offered for every call it advertises. Making a failed probe a hard eligibility
gate is a later decision, after a release of measurements — a probe that
refuses a working model on a bad run is worse than no probe.

The suite version is a **pin, not a floor**: measurements are filed by
`(machine, model, suiteVersion)`, so a version this cockpit does not know is
refused in both directions rather than run under the wrong number.

---

## Sharing models

In **Fleet → the machine → Sharing**, enable **Share models**, choose people
or groups, and save. Turn sharing off and save to keep inference private.
Only the enrolled owner can change this audience. The engine applies it to
new calls on every replica; Cockpit needs no additional setting or restart.

The former `inference.serve` setting is no longer used. Existing Fleet sharing
choices apply automatically. Model allowlists still decide which models the
machine offers. Sharing inference does not grant files, commands, local app
sessions or computer control. A machine must be online with an allowed model
to serve a call. Calls already admitted finish when sharing is taken back.

---

## The speech and image runtimes

Some modalities need a runtime Ollama does not provide.

```bash
memql worker setup --runtime kokoro    # text to speech
memql worker setup --runtime image     # image generation
```

Both print the exact commands and ask before running any of them; both refuse
under `--non-interactive` with exit 3 and install nothing; both are safe to
re-run, and a second run says the runtime is already there rather than
printing the commands again.

`--runtime` means two different things and the mix is **refused rather than
guessed**: alongside `--inference` it chooses how Ollama runs (`docker` or
`native`), and on its own it installs one of these. A person who typed the
wrong combination gets a sentence naming the command that works.

Two things are worth knowing before you run either:

- **Kokoro runs in Docker on macOS too**, which the model runtime deliberately
  does not. A container there cannot reach the Mac's GPU, so a *language*
  model in one would serve from the CPU — what the hardware floor exists to
  prevent. An 82M-parameter speech model is faster than real time on a CPU, so
  the reason does not apply.
- **`--runtime image` installs nothing.** Image generation is a capability of
  the model runtime this machine may already have, so the command's job is to
  tell you whether the runtime reports one and which pull would change that.
  Its refusal states what the runtime *reported*, never which platforms the
  vendor offers it on — a claim this cockpit cannot verify and would be stale
  within a release.

The `runtime:<name>` label appears **only once the runtime answers a probe**,
never because an install command exited zero: a container that started and
then died is a successful install and a runtime that is not there.

---

## Progress during reasoning

Model loading and prompt evaluation, before the first runtime output, use the
call’s overall deadline. They do not count as a stall between generated tokens.
For streaming calls, after the first answer, thinking or tool output, the shorter
idle ceiling applies; a runtime that subsequently stops is cancelled even while
worker keepalives flow. Native Ollama tool-enabled calls use the overall deadline
throughout: Ollama buffers complete tool arguments, so a quiet interval after
reasoning can still be active generation. Compatible runtimes stream partial tool
arguments and retain the shorter idle ceiling.
A runtime that never produces output still reaches the overall deadline and
releases its request and model slot.

The runtime idle watchdog counts answer text, thinking tokens and tool-call
fragments as progress. Ollama's `message.thinking` and compatible runtimes'
`reasoning` / `reasoning_content` fields update liveness without entering the
answer, logs or Fleet content deltas. A long reasoning phase can therefore
finish without being mistaken for a stalled runtime.

Worker keepalives and empty runtime frames do not reset that watchdog. The
request's cancellation and whole-call limit still apply; work goals retain
their separate durable recovery and budget controls.

## Related

- Engine + protocol: [memql#4676](https://github.com/znasllc-io/memql/issues/4676),
  design record `docs/superpowers/specs/2026-08-26-local-models-on-the-fleet-design.md`
- The cockpit half: [memql-cockpit#357](https://github.com/znasllc-io/memql-cockpit/issues/357)
- Open-weight defaults and the four modalities:
  [memql#5137](https://github.com/znasllc-io/memql/issues/5137), cockpit half
  [memql-cockpit#393](https://github.com/znasllc-io/memql-cockpit/issues/393)
- The scanner, the probe and shared machines:
  [memql#5146](https://github.com/znasllc-io/memql/issues/5146), cockpit half
  [memql-cockpit#396](https://github.com/znasllc-io/memql-cockpit/issues/396)
- [`local-apps.md`](local-apps.md) — the same shape, for Claude Code and Codex
