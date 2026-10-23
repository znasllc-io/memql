# Runtime install and model pull on the cockpit, and the guided inference machine -- Design

- **Date:** 2026-09-06
- **Status:** approved in the 2026-09-06 brainstorm as epic 4 of the inference and setup
  program (`memql` repository, `docs/superpowers/specs/2026-09-06-inference-and-setup-program.md`).
  The direction was the owner's; D1-D3 are rulings recorded here for the owner to
  overturn, each with what it costs if wrong.
- **Repositories:** the cockpit half in `memql-cockpit` (this record;
  `internal/worker/cli.go`, `models_cmd.go`, a new `internal/worker/inference/` package,
  `internal/worker/models`, `internal/worker/appsession`'s process supervisor reused,
  `scripts/install/*`, `docs/local-models.md`); the engine half in `memql` (the same record,
  copied; `clients/os/src/apps/fleet`, `component/grpc/worker.proto`,
  `dsl/platform/concepts.memql` `fleetModel`, `docs/public/operate/local-models.md`).

---

## 1. Problem

The fleet can serve local models, and nothing installs one. A person who wants their
machine to be the cluster's inference does five things by hand today: install Ollama,
start it, pull a model, hand-edit `~/.memql/policy.yaml` to allow it, and `kill -HUP` the
worker. No installer writes `policy.yaml`, so `models.allow` is empty on every fresh
machine, which is default-deny by construction; `memql worker setup` does nothing but the
computer-use permission pre-flight; `memql worker models` names the fix as a sentence
("Install Ollama, or declare an OpenAI-compatible endpoint"). The OS's Fleet app mints a
token and shows an install line that never mentions models, and the Readiness signpost
sends people to it.

The owner wants the cockpit to set a machine up as an inference machine, models pulled
from the Ollama library and from Hugging Face, and the whole thing reachable from MemQL OS.

## 2. What the tree already has

- **Discovery, gating and serving are done.** `internal/worker/models` probes Ollama at
  the three `OLLAMA_HOST` spellings and any declared OpenAI-compatible endpoint, applies the
  hardware floor (Apple Silicon with 16 GB on macOS 13+, or a discrete GPU with 8 GB VRAM
  on Linux), reads `/api/show` per model, and advertises `model:<id>` labels; `modelcall`
  serves the calls. `/api/show`'s `model_info` already carries `general.parameter_count`,
  `general.file_type` and `general.size_label`; today only the context length is read.
- **A streaming subprocess supervisor exists once:** `internal/worker/appsession/process.go`
  (own process group, partial-line flush every 2 s, SIGTERM then SIGKILL). `ollama pull`
  prints a newline-less progress bar, which is exactly what that supervisor was written
  for. The engine-driven `workerHost.exec` path is capped at 600 s with buffered output
  and cannot carry a pull.
- **The shell allow-list admits `docker` and refuses `curl`, `wget` and `sudo`**, so a
  Docker-based runtime is reachable through existing policy and a `curl | sh` install is
  not.
- **Re-advertising costs a reconnect:** the engine binds labels at the handshake, so a
  new model becomes visible only after the next fingerprint change, outside the busy
  window and at least two minutes after the last re-advertise.
- **The OS Fleet app** has "Add machine" with one checkbox that changes the install line
  (the computer-use build), an install line that MUST stay one physical line (three
  surfaces print it and a bracketed paste once split it), a machine detail with facts,
  labels and apps groups and no models group, and no consumer of `fleetModels` anywhere.
- **The VS Code installer's offer** already distinguishes "runtime present, zero models"
  and labels its accept "Show me how", which today leads to prose.

## 3. Decisions

### D1 -- Docker is the runtime on Linux; native Ollama is the runtime on Apple Silicon

Ruling. The owner asked for Docker always. On macOS a container has no access to the
GPU, so Ollama in Docker on an M-series machine serves on the CPU, which the hardware
floor exists to prevent; on Linux the Docker image with the NVIDIA container toolkit (or
ROCm) is the reproducible path and `docker` is already on the cockpit's allow-list. So:
Linux installs `ollama/ollama` in Docker with GPU passthrough and refuses when the
toolkit is absent, naming it; macOS installs Ollama natively through Homebrew or points at
the app, and says why Docker is not offered there. Cost if wrong: an owner who wants
Docker on a Mac gets a CPU-only runtime the cockpit will not advertise, and would have to
override the floor; overturning this ruling means adding a "CPU-only, not advertised"
mode.

### D2 -- The cockpit sets the machine up locally, consented, idempotent, without sudo

`memql worker setup --inference` on the machine: the floor verdict, the runtime (detect,
else install per D1 after printing the exact commands and asking), the models (a default
pair, `llama3.1:8b` and `nomic-embed-text`, or `--model <id>` any number of times, Hugging
Face repositories as `hf.co/<owner>/<repo>:<quant>` through Ollama's own pull), the pull
streamed to the terminal through the appsession supervisor, `models.allow` written to
`policy.yaml` with the same mode care `worker.yaml` gets, a SIGHUP to a running worker,
and a closing line that prints what the cluster will see. `--non-interactive` refuses to
install anything it would have asked about, with exit code 3, so an automation never
installs software nobody approved. Nothing here needs sudo; the installers' rule that a
background service is never started unasked stands.

### D3 -- A machine can be driven from MemQL OS, in a second step, over a new message pair

The remote path is `ModelPullStart / ModelPullProgress / ModelPullEnd` on
`WorkerService.Stream`, the `AppSession` family's shape: the engine names the model, the
cockpit runs the pull under the same supervisor, streams structured progress (bytes,
total, status line), writes `models.allow` on success, and re-advertises AT ONCE, once,
bypassing the two-minute guard for this case only, because a person who just pressed
"Pull" is watching. Runtime install stays local (D2): installing software on a person's
machine from a web page is a line the cockpit does not cross. Cost if wrong: a pull the
person did not start; mitigated by the owner-only gate on the OS act and the cockpit's
`models.pull` policy switch (default on for the machine's own owner).

### D4 -- Size and quantization ride the labels; epic 3 owns the attribute

`params=<count>` and `quant=<level>` join the `ctx=…,structured=1,max=N` value of each
`model:<id>` label (epic 3, decision D5, owns both parsers and the wire-contract test);
this epic reads them from `/api/show` on the cockpit and renders them in the OS. No proto
field, because labels are `map[string]string` end to end and the flat-list rationale in
`models.go` holds. The engine's `fleetModel` projection gains `params` and `quant` with a
stated reduction: the MAXIMUM across machines for `params` (a model id names one size, so
disagreement is a labelling error the row reports) and the set of quantizations.

### D5 -- The Fleet app grows a models surface, and the install line grows one flag

"Add machine" gains a second checkbox, "This machine will run local models", which appends
`--inference` to the one-line install command; the installers pass it through to `memql
worker setup --inference` after the token is written, in the same terminal. The machine
detail gains a Models group (runtime, each model with params, quant, context window,
capabilities, allowed or blocked, and the Pull act for owners), fed by the registration's
labels live and by `fleetModels` for the fleet-wide view. The Readiness signpost's
"pair a machine that runs a local model" links to this flow.

### D6 -- What this epic does not decide

Which models are "good" (the model floor is a class, the operator picks), CPU-only
machines (never advertised, as today), models on workbenches (never), and the quality
ranking used for routing (epic 3).

## 4. The change

### 4.1 Cockpit

- `internal/worker/inference/`: `Plan(ctx, host) (Plan, error)` (floor, platform, runtime
  presence, Docker presence and GPU toolkit on Linux), `InstallRuntime(ctx, plan, consent)`,
  `Pull(ctx, model, progress func(Progress)) error` over the appsession supervisor,
  `Allow(policyPath, ids...)` (atomic write, 0600, merge into `models.allow`),
  `Readvertise()` (SIGHUP self or the running worker).
- `cli.go`: `setup --inference [--model <id>]... [--non-interactive] [--runtime docker|native]`;
  `models --pull <id>`; `models --allow <id>`.
- `models/ollama.go`: read `general.parameter_count`, `general.file_type`,
  `general.size_label` into the inventory (epic 3 renders them into labels).
- `loop.go`: a `ModelPull*` handler beside `AppSessionStart`, and the one-shot immediate
  re-advertise after a pull.
- `scripts/install/install-{mac,linux}.sh` and `lib.sh`: `--inference` pass-through;
  `lib_test.sh` covers it.
- `docs/local-models.md`: the five manual steps become one command; the failure table
  gains rows for "Docker present, no GPU toolkit", "pull refused by policy", "model pulled
  and not yet visible (reconnect latency)".

### 4.2 Engine and OS

- `component/grpc/worker.proto`: `ModelPullStart{registration_id, model, request_id}`,
  `ModelPullProgress{request_id, completed_bytes, total_bytes, status}`,
  `ModelPullEnd{request_id, ok, error, model}`; the agent node forwards to the replica
  holding the stream (the `WorkerForward*` precedent).
- `dsl/worker/builtins.memql`: `fleetModelPull(registrationId, model)`, owner of the
  machine only; `dsl/platform/concepts.memql` `fleetModel` gains `params`, `quant`.
- Fleet app: the checkbox and the flag on the install line (three surfaces, one function),
  the Models group, the Pull act with live progress from `ModelPullProgress` bridged onto
  the machine row, the Readiness link.
- `docs/public/operate/local-models.md`: the one-command story and the OS path.

## 5. Failure modes

- No GPU toolkit on Linux with Docker present: refused before any pull, naming the package.
- A pull interrupted (network, disk full): the supervisor ends the child, the model is
  not allowed, the OS shows the runtime's own last line; a retry is a new pull.
- Disk space: `Plan` reports free space on the runtime's volume and the model's size when
  Ollama can say it; a pull that would exceed it is refused with both numbers.
- The reconnect tax: the OS says "visible to the cluster within a minute" after a local
  pull and shows the model the moment the re-advertise lands after a remote pull.

## 6. Testing

- `inference.Plan` on fixture hosts: Apple Silicon with and without Ollama, Linux with
  Docker and toolkit, Linux with Docker and no toolkit, below-floor machines; every path
  asserted on the sentence it prints.
- `Pull` over a fake `ollama` binary that prints a progress bar without newlines and exits
  0, then one that exits 1; progress callbacks observed; the allow-list written only on 0.
- `Allow`: an existing `policy.yaml` with other keys survives byte-for-byte outside
  `models.allow`; mode 0600; a missing file is created.
- `--non-interactive` refuses an install with exit 3 and installs nothing.
- The `ModelPull*` round trip against the engine's in-process forward hop test pattern.
- OS: the checkbox changes the install line and nothing else on it; the Models group
  renders params and quant; the Pull act is absent for a non-owner; screenshots in both
  modes.

## 7. Delivery

Cockpit: two PRs. PR 1: `inference` package, `setup --inference`, `models --pull/--allow`,
the `/api/show` fields, installers, docs. PR 2: the `ModelPull*` handler and the
immediate re-advertise, against the proto from the engine's PR 1 (pin bump). Engine: two
PRs. PR 1: proto, forward hop, `fleetModelPull`, `fleetModel` fields. PR 2: the Fleet app
surfaces and docs. Order: cockpit PR 1, engine PR 1, cockpit PR 2, engine PR 2.

## 8. Facts to re-verify before starting

Ollama's `/api/show` detail keys; the Docker image's GPU flags and the toolkit package
names on the supported distributions; Homebrew's Ollama formula versus the app; whether
`ollama pull hf.co/<repo>` accepts a quantization suffix in the current release. All were
read on 2026-09-06 from the two trees; the vendor facts are the executing session's to
confirm.
