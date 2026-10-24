# MemQL Cockpit

**Type:** Fleet worker runtime + cluster CLI for MemQL
**Binary:** `memql` (built from `cmd/memql/`)
**Display name:** "MemQL Cockpit"
**Language:** Go

The Cockpit is the machine-side half of the MemQL fleet: it enrolls a machine
against a cluster and runs as that machine's **worker** (headless shell / fs /
http tools everywhere; mouse + keyboard + screenshot on the computer-use
build). The TUI it once carried is gone (2026-08-25 slim-down — spec in
`docs/superpowers/specs/2026-08-25-cockpit-slim-rename-design.md`); the portal
and VS Code extension own cluster interaction, while the native macOS menu
companion owns local connection controls and troubleshooting logs. The engine lives
in a separate repo (`github.com/znasllc-io/memql`) and is consumed as a
**pinned sibling checkout** — see "The memql pin" below, which is the single
most surprising thing about this repository.

**The command name is `memql`, and the collision is deliberate:** the engine
repo also builds a `bin/memql`, but it ships only inside container images and
runs in pods. This CLI is what gets installed on operator machines — different
distribution channels, no PATH overlap. Do not "fix" this.

---

## Quick Start

```bash
make cockpit             # build the headless binary -> bin/memql
make cockpit-computeruse # the computer-use variant (CGO + RobotGo) -> bin/memql-computeruse
make worker              # build + run the worker (ARGS="--log-level=debug")
make clusters            # build + list the registered clusters
make test                # go test ./...
make lint                # fmt + vet
make tidy                # go mod tidy
make help                # every target, auto-generated from target comments

make dist                # versioned tar.gz archives + SHA256SUMS into dist/
make release             # recommend the next version; ARGS="--prepare" opens a version PR
```

`./...` really does mean everything here. **This is a single Go module** with no
`go.work` and no nested modules — the opposite of the memql repo, where a bare
`go test ./...` silently misses the engine. Do not import that repo's caution
into this one.

---

## The memql pin (read this before touching `go.mod`)

Cockpit consumes the engine's wire-tier packages through **`replace`
directives pointing at a sibling checkout at `../memql`**. Local builds
therefore resolve against whatever your memql working copy happens to be; CI
resolves against a **pinned commit**.

- **The pin lives in exactly one file: `.github/memql-pin`.** Native commands
  use `scripts/ci/prepare.py` to read it from the exact candidate commit and
  materialize two owned sibling checkouts. The preparation tests refuse
  ambiguous pins and prevent uncommitted operator changes from entering CI.
- **The `require` pseudo-version in `go.mod` is NOT the pin.** It is an
  epoch-zero placeholder that exists only because Go must validate a require
  against a real remote or a local `replace`. Reading it as the pin gives you
  the wrong commit and the wrong date.
- **Why pinned at all:** memql is being split into ~29 nested modules
  (memql#3228). No static cockpit `go.mod` is green both before and after a
  given module lands, and there is no atomic cross-repo merge. The pin buys the
  window.
- **To bump:** change the sha, add any newly-landed module's `require` **and**
  `replace`, and run `go mod tidy` — **all in one commit**. The truth table for
  which combinations fail (and how) is in `go.mod`'s own comment block; read it
  before guessing.
- **The 2026-08 blocker is gone, and the pin has moved past it.** The
  slim-down deleted every `component/genesis` importer (the TUI wizards and
  the embedded deploy runtime), and the local-apps bump then advanced the pin
  across the genesis removal (`36c19108`) and the engine / platform / server
  tiers in one commit. Two modules cockpit already imported —
  `component/identity` and `component/memql` — grew their own `go.mod` in that
  window and are now direct `require`s; the rest of the `replace` block is the
  transitive closure `go mod tidy` demanded, not a hand-picked list.

### Consequence: local build failures that are not your fault

If `go build ./...` here reports `updates to go.mod needed`, the usual cause is
that your `../memql` checkout has moved past (or lags) the pin. Check
`.github/memql-pin` against your memql sibling before debugging; a detached
checkout of the pinned sha is the reference state.

---

## Repository Structure

```
memql-cockpit/
├── cmd/memql/              Binary entry point: dispatch, cluster add/list/
│                           remove, login/logout, access, creds; variant consts
├── internal/
│   ├── access/             `memql access` — what the cluster says you are:
│   │                       role slug / name / rank, groups, account scope
│   ├── auth/               Identity-service auth: browser code grant with
│   │                       loopback callback; RFC 8628 device flow fallback
│   │                       (device.go) for SSH / headless machines
│   ├── config/             ~/.memql/clusters.yaml registry, credential
│   │                       stores (keyring / file), identity discovery
│   │                       (/.well-known/memql-config.json client)
│   ├── crash/              Panic recovery + redacted crash reports
│   ├── worker/             The worker: pair / run / setup / config /
│   │                       consent; LaunchAgent + service glue; tools/
│   │                       (shell, fs, http; computeruse adds screenshot /
│   │                       mouse / keyboard / window via RobotGo);
│   │                       apps/ (local-app detection) + appsession/
│   │                       (the app-session runner) + harness/ (the
│   │                       per-app protocol clients; record.go is the
│   │                       one shape every completed tool call leaves
│   │                       in) -- see Local apps, App-session recording;
│   │                       models/ (runtime discovery + the hardware
│   │                       floor) + modelcall/ (the ModelCall server)
│   │                       -- see Local models; inference/ (runtime
│   │                       install, model pull, models.allow) -- see
│   │                       Setting a machine up; backup/ (the
│   │                       watched-folder sweeper) -- see Watched-folder
│   │                       backup
│   ├── lint/               `memql lint` — author-facing DSL validator
│   └── setupproject/       `memql setup project` — stamps a memql-project
│                           workspace (stdin prompts when flags are absent)
├── scripts/install/        Worker-machine installers (mac / linux): binary +
│                           service + worker.yaml; the portal composes the
│                           one-liner (--version=X.Y.Z pins a release).
│                           uninstall-{mac,linux}.sh are their inverse, one
│                           line too: with no flags they decide the
│                           enrollment (one -> that cluster, none -> the
│                           runtime, several -> refuse with a command each)
│                           and the install shape from the machine;
│                           --cluster=URL / --all-homes say it; --dry-run
│                           prints the plan; --purge takes policy.yaml and
│                           the state dir as well. install.sh at the root
│                           is the plain binary installer from GitHub
│                           releases
├── deploy/systemd/         memql-worker.service template (user systemd)
├── docs/                   access.md, computer-use.md, local-apps.md,
│                           local-models.md, pipelines.md,
│                           watched-folders.md;
│                           docs/superpowers/specs/ designs (the plans
│                           beside them are deleted by the PR that
│                           finishes them; the specs are the record)
└── .github/memql-pin       THE pin. Single source of truth
```

## Build variants

Two builds, **one installed command name** (`memql`):

- **headless** (default, CGO off) — ships from GitHub releases; shell / fs /
  http worker tools.
- **computeruse** (`-tags computeruse`, CGO + RobotGo) — adds the
  workerComputer.* surface; built per-host (macOS Xcode CLT; Linux gcc +
  libxtst-dev / libxinerama-dev / libxkbcommon-dev / libpng-dev).

`memql --version` prints the variant; the worker's capability registration
carries it to the cluster. Dev cross-builds emit suffixed artifacts in `bin/`
(`memql-darwin-arm64`, `memql-computeruse`, ...) but an installed machine has
exactly one `memql`.

## Local apps as execution surfaces

The worker can delegate a planner Task to an app the user **already pays
for** -- Claude Code or Codex -- running on this machine, with MemQL's tools
reachable from inside it over MCP. The engine half is memql#4358; the
canonical record is the engine's
`docs/public/operate/local-apps.md`. `docs/local-apps.md` here covers the
machine side.

- **`internal/worker/apps`** detects `claude` / `codex` on PATH, their
  versions and their auth state, and the worker reports the inventory on
  `Register` and **every** `Heartbeat`. The engine derives `app:<id>` routing
  labels from it and has no other way to learn any of it. **Which PATH** is
  the worker's own, extended once at start by `ensureServicePath`
  (`servicepath.go`): a LaunchAgent gets launchd's four system directories
  and nothing else, which found no app an installer put in `~/.local/bin`
  or `/opt/homebrew/bin`. The app directories are APPENDED, never
  prepended (the worker runs `launchctl`/`plutil` by bare name), and every
  plist writer writes the same PATH. `ResolveSpec` returns the path it
  resolved, so a session starts the very binary the inventory reported.
- **`internal/worker/appsession`** runs the sessions:
  `AppSessionStart / Chunk / Control / End`, kinds `run` / `open` / `attach`,
  plus the MCP config writer, the Library pull/push, and the platform launch
  paths.
- **`internal/worker/harness`** drives each app through its OWN protocol
  (memql#5096's cockpit half): `claude-headless`, `codex-app-server`, and
  `codex-mcp` as the fallback. It replaced a parser for terminal output.

**A SESSION IS A SEQUENCE OF TURNS**, and that shape is forced rather than
chosen: neither app offers a process you can keep feeding. Claude Code is one
prompt per process resumed by `session_id`; Codex takes one turn per
app-server request. An `AppSessionControl` with `action="message"` starts the
next turn. It also makes credential renewal honest -- the replacement lands
when the next process starts, rather than pretending to reach a running one.

**THE HARNESS IS PER MACHINE, NOT PER APP ID.** Two harnesses answer to
`codex`, and only this machine can run `codex app-server --help` to find out
which. `apps.Specs()` carries the FLOOR (`codex-mcp`); `Detector.ResolveSpec`
is what upgrades it, and the session runner must use that rather than
`apps.SpecFor` or it drives every Codex through the fallback. Codex's MCP
config travels as `CODEX_HOME` in the environment and Claude Code's as
`--mcp-config`; getting that backwards leaves one of them with no tools and
no error.

**`codex mcp-server` reports no usage and cannot constrain output**, and both
are reported as absent rather than guessed -- its own token events are
documented as "accumulated, estimated, or replayed", and its tool schema is
`additionalProperties: false`, so an invented output-schema argument is
REFUSED rather than ignored.

Four rules here are load-bearing, and each is the kind that fails silently:

1. **`signed_in=false` beats a guess.** A routing label needs `allowed` AND
   `signed_in`, so the router cannot pick a machine that would then refuse.
   Every probe reports false when it cannot tell -- including on macOS, where
   Claude Code's token is in the Keychain and the worker must not read it (a
   LaunchAgent raising a Keychain prompt is the same hazard the credential
   store already avoids).
2. **`apps_present` is always true on the beat.** proto3 cannot distinguish an
   empty repeated field from an absent one; `false` means "this build does not
   report apps", which is wrong for a machine that just uninstalled one.
3. **App consent is default-deny, PER CLUSTER** (`policy.yaml
   apps.homes.<home>.allow`). An app session does what `workerHost.exec`
   does, and a machine-wide list meant consent given for a local test
   cluster reached production too -- the crossing the per-home tool consent
   windows (#433) rule out. Each home gets its own inventory and its own
   session gate; a cluster the block does not name is allowed NOTHING. The
   retired machine-wide `apps.allow` is not read: it is reported as a
   problem naming `memql worker apps --allow <app> --home <cluster>`, which
   writes the entry (a textual edit, `inference.MergeList`) and removes the
   old list. The block REPLACES on SIGHUP, so a `--deny` needs no restart.
   An app present but unlisted is reported with `allowed=false` rather than
   omitted -- the portal can then say "present, blocked" instead of
   rendering it identically to "not installed".
   The Linux/macOS enrollment installers explicitly grant installed Claude
   Code and Codex for their named cluster through `worker apps --allow` before
   starting the service. Repeating an installer repeats that grant; `--no-apps`
   preserves existing permissions. Discovery/heartbeats never grant consent.
4. **The MCP config file is deleted on every exit path.** The per-run bearer
   **cannot be revoked** (the engine's verify path is JWKS-only and DB-free),
   so deletion is the security control, not housekeeping. A `defer` is not
   enough: every write is recorded in a ledger under the state dir, and
   `appsession.Sweep` clears what a SIGKILL left behind at the next start.

**A RUN WORKSPACE IS SHARED; SESSION SCAFFOLDING IS NOT.** Transcripts, Claude
configs, and interactive launcher/exit paths have exclusive names inside
`.memql-session/`. New Codex conversations get private homes. A workspace-local
ref index lets attach reuse provider state after credential cleanup, guarded by
an OS lock so a second attach cannot replace an active bearer. Legacy refs use
only the old home in that workspace. Keep provider state separate from credential
lifetime; randomizing every attach home silently loses the conversation.

**AN EMPTY `AppSessionStart.workspace` IS THE MACHINE'S CHOICE**, never a
refusal: the engine cannot know a path here. `appsession/workspace.go` makes
`<fs.workspace_root or the platform data dir>/<home>/<run_id or session_id>`,
passes it through `CheckWorkspace` like an engine-named path, and removes it
at End only when it was keyed by the session -- and not when the push failed
or the kind is `open` (kept, logged). Never under `~/.memql` (tokens,
consent). Read the RESOLVED workspace (`session.workspace`), not the start's
field: the start's is empty for a chosen directory, and the output push reading
it pushes nothing the app made.

**THE WORKSPACE IS THE APP'S WRITE GRANT** (`appsession/protected.go`): any
workspace, engine-named or chosen, that is, contains or lies inside the
worker's own files (`sessionWorkerPaths`: the config dir, state, token file,
consent socket) or an `fs.deny` entry is refused -- `CheckPath` alone accepts
`$HOME` and `/`. The same paths reach the app as `harness.Spec.DenyPaths`,
denied for READING too: without them the sandbox confines writes alone.

**BROWSER PROFILES ARE RUNTIME, NOT OUTPUT.** Chromium user-data roots may have
arbitrary names beside the proof they produced. `isBrowserRuntimeDir` recognizes
multiple on-disk markers without reading their contents; collection, snapshots,
fingerprints and write-byte retention share that boundary. Keep sibling PNGs,
scripts and receipts. Do not solve over-retention by deleting work or weakening
the output archive's integrity checks. Other task-owned runtime belongs under
an already-excluded cache directory; cleanup and old Library artifacts are
separate from future selection.

**CLAUDE CODE'S PERMISSIONS ARE IN THE ARGV, NOT IN ~/.claude**
(`claudeGrantArgs`): `--setting-sources=` (no settings file),
`--strict-mcp-config`, `--permission-mode dontAsk`, `--allowedTools
"Edit(/**) Read(/**) mcp__memql"`, `--disallowedTools` with a bare
`Read(//path) Edit(//path)` per deny path (no `/**`: the Linux sandbox
expands or skips a glob), and the sandbox inline via `--settings` with the
same paths as `filesystem.denyRead`/`denyWrite`.
Bash has no rule -- the sandbox's auto-allow is its only approval, so a
command the sandbox did not take has none. `--tools` is NOT passed: it would
drop ToolSearch, which loads deferred MCP tools. Every flag before `--`.
`CLAUDE_CODE_CERT_STORE=bundled,system` is pinned per turn (mkcert CA), and an
init event reporting MemQL's server `failed`/`needs-auth` stops the turn;
`pending` does not.

**The app-server's frames carry no `"jsonrpc"` header** -- its README
says so, and none of a real 0.153.4 turn's lines has one -- so
`jsonrpcConn.route` recognises a frame by its shape (`rpcMessage.isFrame`:
the header, or a method, or an id with a result or an error). Requiring the
header dropped the answer to `initialize` as stray output and hung every
app-server session at Start while fakes that added it passed: a fake here
must print what the real binary prints, header included or not.

`usage.known=false` when the app reported nothing, and `exit_code` passes
through unnormalised -- the engine records the first as billing "unknown" and
reads the second as a failed run.

**A LEVEL IS TRANSLATED ON THE MACHINE** (epic memql-cockpit#436, the cockpit
half of the engine's 2026-09-13 recording-and-learning record, epic A, D8/D9).
`AppSessionStart.level` carries core/airoute's word -- `fast`, `strong`,
`reasoning`, `embeddings` -- and `internal/worker/harness/levels.go` turns it
into the app's own knobs: Claude Code `--model`/`--effort` by alias (haiku /
sonnet high / opus xhigh), Codex `model_reasoning_effort` only (low / medium /
high; no model, because Codex has no tier aliases and its catalogue is per
account). `policy.yaml apps.levels` overrides a row. `memql worker apps`
prints the effective table. Five rules, each of which fails silently:

1. **The vocabulary is the engine's package, not a copy.** `harness` imports
   `core/airoute` (stdlib-only), so a rename fails at the pin bump instead of
   refusing every session afterwards. `embeddings` NEVER runs through an app
   (D10), an undefined word refuses, and an empty level is the app's own
   defaults -- nothing else falls back to defaults.
2. **Report, never request.** `AppSessionEnd.model/effort` are what the APP
   stated (Claude Code: `modelUsage`; Codex: the thread's settings, a
   `model/rerouted`, `session_configured`), and empty when it said nothing.
   Claude Code states NO effort anywhere (verified 2.1.270), so its effort is
   always empty -- copying `--effort` there would record the request as a
   measurement. Codex echoes even a model nothing answers to, so its settings
   are reported only for a turn that completed or reported spend.
3. **`CheckKnobs` exists because Claude Code IGNORES an effort it does not
   know** (a stderr warning, exit 0). The check runs on the built-in table (a
   test), on every `apps.levels` entry when the file is read, and in every
   harness's `Start` before anything forks.
4. **`apps.levels` is NOT default-deny.** Absent means the built-in table; an
   entry replaces its row WHOLE; the block REPLACES on SIGHUP. An entry the
   app would misread REFUSES its level with the policy's sentence -- never a
   fallback to the built-in row it was written to replace, and the refused
   row leaves the table the harness sees. Entries no session can reach
   (unknown app, `fastt`, `embeddings`) are logged, not refused. The block is
   read from its `yaml.Node`, NOT a typed decode: a typed decode drops an
   unknown key (`efort`) silently and fails the WHOLE file on a shorthand
   (`reasoning: opus`) -- and a worker that cannot parse policy.yaml runs on
   defaults that allow no app. Keep it a node walk.
5. **The level is resolved before any side effect.** The session refuses it
   before writing the bearer, pulling inputs or opening a transcript, and the
   harness resolves the same table again in `Start` with the same function.
   The `open` kind reads no level: a person picks their own model. An
   attach through `codex-mcp` cannot take one at all (`codex-reply` declares
   no configuration), so `harness.CheckResume` refuses it rather than let
   the transcript claim a level the app never received.

**The memql#5096 app-session fields are mapped** (memql-cockpit#444): the
follow-up is `AppSessionControl.prompt` (never `reason`), the schema is
`AppSessionStart.response_schema_json`, the answer is
`AppSessionEnd.result_json` (the `memql.app_session.result` chunk is gone),
and `Register.app_descriptors` says which harness drives each app. The pin
carried all four for a week before anything read them -- when a pin bump
lands a field, grep for the stand-in the same commit.

## Local models on the fleet

The worker can serve MemQL's own operations -- planning, conductor/routing,
suggestions, embeddings -- from a model running on **this** machine, over
the stream it already holds open. The engine half is memql#4676; the
canonical record is the engine's
`docs/public/operate/local-models.md`. `docs/local-models.md` here covers
the machine side.

- **`internal/worker/models`** discovers Ollama natively (`/api/tags` +
  `/api/show`) and reads OpenAI-compatible endpoints declared in
  `policy.yaml`. It also owns the **hardware floor** -- Apple Silicon /
  16 GB / macOS 13+, or x86_64 with a >= 8 GB discrete GPU. The check runs
  on the machine because only the machine can see its own GPU.
- **`internal/worker/modelcall`** serves `ModelCallStart / Delta / End /
  Cancel` against the local runtime: both kinds (`chat`, `embedding`),
  monotonic delta `seq`, envelope-owned deadlines, and both concurrency
  ceilings.

Five rules here are load-bearing, and each fails silently:

1. **Every capability defaults to ABSENT.** The engine is fail-closed: a
   model that says nothing about structured output is never selected for a
   structured prompt. A probe that cannot establish a capability must claim
   nothing -- an over-claim surfaces as a parse failure three layers away,
   naming nothing here. Ollama has no structured-output capability of its
   own, so `tools` is the proxy; an operator who disagrees declares the
   model under an OpenAI-compatible runtime instead. Tools, structured and
   vision are claimed only for a model Ollama lists as `completion` (an
   embedder reported `tools` once and was sent chat calls), and a runner
   that answers `format` with 501 gets the schema in the prompt and passes
   on only an answer that validates (`modelcall/ollama_schema.go`).
2. **`max_concurrent` is the exception, and it is never absent.** It is the
   one attribute whose absence is PERMISSIVE -- the engine reads a missing
   ceiling as unlimited. `OLLAMA_NUM_PARALLEL` when set, otherwise 1.
3. **The floor GATES the inventory; it does not annotate it.** Below it a
   machine advertises nothing at all, and `Discover` does not even probe.
   `memql worker models` uses `Probe` instead, so an operator can still see
   what they have.
4. **Model labels are bound at Register, and Heartbeat cannot refresh
   them.** `Heartbeat` carries apps but no labels, and the engine's stream
   handler accepts `Register` exactly once. Re-advertising therefore costs
   a RECONNECT -- taken only when the advertised labels actually changed,
   only when nothing is in flight, and never twice inside two minutes.
5. **`sharedInference` is the owner's grant, never the cockpit's.** The
   engine reads it from `operatorLabels` alone, because `labels` is
   overwritten from Register on every reconnect. A cockpit that derived one
   would be granting itself a permission and revoking it whenever the lid
   closed.

`policy.yaml models.allow` is default-deny like app consent, and a model
present but unlisted is REPORTED as blocked rather than omitted. Usage
rides on `ModelCallEnd` exactly as the runtime reported it -- silence stays
silence, which the engine records as billing "unknown".

**The label carries `params`, `quant` and `tools` as well as `ctx`,
`structured`, `embeddings` and `max`** -- the engine ranks a fleet on size
(parameters descending, then context) and skips a machine whose runtime
cannot carry a tool turn. `params` is a COUNT, not Ollama's `"8.0B"`,
because "70B" sorts before "8.0B" as text. Both are ORDERING signals:
`Satisfies` does not read them and a model that states no size is still
served, it just sorts last -- silence never makes a model the strongest.
`quant` is the only free-text value in a comma-and-equals delimited label,
so a level carrying either character is dropped whole rather than escaped
(`quantSafe`); without that, a runtime string could forge a second
attribute the machine never claimed.

The engine parses these model attributes. `TestWireContract` checks the
shared names against the pinned engine; keep both sides synchronized.


## Setting a machine up to run models (epic memql-cockpit#387)

`memql worker setup --inference` takes a machine from bare to advertising a
model: floor check, runtime installed if absent, models pulled with
progress, `models.allow` written, worker signalled.
`internal/worker/inference/`, plus `--inference` on both installers.
The engine half is epic memql#5103. Operator doc:
[docs/local-models.md](docs/local-models.md).

**`Decide` is a PURE FUNCTION of `Host`.** Every platform question is
answered into the struct by build-tagged files, so the decision is
table-testable on fixtures with no build tags and no real machine -- and
every path is asserted on the exact SENTENCE it prints, which makes
changing the words a thing somebody does on purpose. Those sentences are
the entire product for a person who is blocked.

**Native Ollama on both platforms, Docker on Linux only by request, and
never sudo.** On macOS a container has no GPU access, so Ollama there
would serve on the CPU -- which is what the hardware floor exists to
prevent -- and Homebrew installs it natively; without Homebrew the same
stage as Linux fetches Ollama's own `ollama-darwin.tgz`, checks it against
the release's `sha256sum.txt`, and a LaunchAgent (`com.znasllc.memql-ollama`)
keeps it up (memql-cockpit#524). **Installed is not serving**: an `ollama` on
PATH with its service stopped gets a printed, consented start command
(`withStart`) and a wait for `/api/version`, never a pull against a closed
port. **The disk is judged before the pull** on the bytes still missing
(`MissingBytes`), and the mid-pull check counts only what each layer still
has to write. On Linux the first design
(2026-09-06 record, D1) was the `ollama/ollama` container, and it stopped
on every fresh machine at the same place: the NVIDIA container toolkit is a
root install, Pop!_OS 24.04 has it in no configured repository, and
NVIDIA's own steps end in a Docker restart that bounces the k3d cluster
running beside it. The cockpit runs no sudo, so "install end to end" was
not something it could do. The 2026-09-08 record (engine
`docs/superpowers/specs/2026-09-08-linux-native-runtime-and-class-defaults-design.md`,
D1) makes the Linux default the vendor's release archive unpacked under
`~/.memql/ollama/runtime` and kept up by a user systemd unit
(`memql-ollama.service`, loopback only, models in `~/.memql/ollama/models`):
`internal/worker/inference/stage.go` fetches it, checks it against the
release's `sha256sum.txt`, refuses any archive entry that would land
outside the runtime directory, and writes the unit -- all after the same
one consent as the commands, and before the first command runs. The
container survives as `--runtime docker`, and every refusal it can produce
names dropping the flag as the way out. The cockpit still RUNS no sudo
command; it may PRINT one for the person, and the copy says which is
which. There is no ROCm equivalent of the NVIDIA container toolkit: for
AMD the device nodes are the passthrough and the missing piece is
`amdgpu-dkms` for the container, or the `render`/`video` groups for the
native runtime, so reaching for a symmetric package name produces a
refusal naming something nobody can install. The Linux uninstaller
removes the runtime's unit with the worker's, and `--purge` takes
`~/.memql/ollama` -- runtime and models -- so a machine is never left
serving models from something Fleet cannot see. If stopping a unit fails or `systemctl` is unavailable,
uninstall reports a partial result and preserves the unit and runtime for
a retry; it still removes the worker token.

**Working context is explicit.** Chat forwards the engine's
`ModelCallParams.context_tokens` to Ollama's `options.num_ctx`; an absent
value preserves the runtime default. Embedding uses 8192 tokens so the
0.6B embedder's cache can fit beside the class's chat model. The native unit
sets `OLLAMA_KV_CACHE_TYPE=q8_0`; Ollama enables Flash Attention itself on
supported devices. The recommended pairs are 4B below class 16, 9B at 16,
27B Q4 at 24/32, and 27B Q8 at 64/128, always followed by the same 0.6B
embedder. The 24 GB pair is budgeted at 32K chat context, not every context
the model supports.

**The pull is `POST /api/pull`, NOT `ollama pull`**, and the design
record's plan naming a subprocess was wrong on the platform its own D1
chose: a Linux machine set up by this command runs the runtime from
`~/.memql/ollama/runtime`, off PATH, or in a CONTAINER under `--runtime
docker`, and neither puts an `ollama` on PATH. The HTTP route also gives
exact byte counts where the CLI gives a TTY progress bar. **A 200 is not
success** -- Ollama emits `pulling manifest` before it fetches anything,
so a failure arrives inside an already-started 200 body, the same trap
`/memql/query` sets. Absence of the closing `{"status":"success"}` is
failure too, because the other direction advertises a half-pulled model.
`Completed`/`Total` are PER LAYER and restart for each blob.

**`Allow` is a targeted textual edit, not a YAML round trip.** yaml.v3
load-modify-save re-indents sequences, drops blank lines and re-quotes
scalars; the parse decides only what and where. Where it cannot edit
textually it REFUSES and names the line, rather than reformatting an
operator's commented `policy.yaml` behind their back.

**`models.pull` is default-TRUE**, the opposite of `models.allow` beside
it, and the field is a `*bool` for that reason -- a plain bool reads an
absent key as false and would have switched pulls off on every
`policy.yaml` already on disk. Allowing a model spends this machine's GPU
on somebody else's prompt; pulling one is its owner acting on their own
machine.

**A SIGHUP reloads policy; it does not make the model visible.** Labels
bind at `Register`, so visibility needs a reconnect --
`Runner.RequestImmediateReadvertise` arms a one-shot bypass of the
two-minute floor and invalidates the 90-second inventory cache in the same
call, because a re-advertise that re-registered the pre-pull labels looks
exactly like a pull that did nothing. It never bypasses the BUSY guard: a
reconnect that interrupts a running model call is worse than a stale
label. Never print "your model is available now".

**The cluster can ask for the pull** (memql#5103; the install wizard's
D13, memql#5218). The `ModelPullStart` / `Progress` / `End` / `Cancel`
arm in `internal/worker/modelpull.go` runs the SAME path
`memql worker models --pull` runs -- `inference.Pull` against the
discoverer's base URL, `inference.Allow`, then the policy reload the CLI
gets through SIGHUP (done in-process here, because this arm IS the running
worker), then `RequestImmediateReadvertise` -- and answers `ok=false` in a
sentence when this build has no inventory or `models.pull` is off. The End
goes out BEFORE the re-advertise is requested, because the reconnect that
re-advertises closes the stream the End rides; a live pull counts as BUSY
for the same reason, so one model landing cannot cut its sibling's
download short. A pull is not in `Runner.active` (hours, not a tool
result) and takes no ModelCall slot (bandwidth, not the runner).


## The scanner, the probe, sharing, and the four modalities

Epics memql-cockpit#393 (open-weight defaults) and #396 (the scanner and
shared machines). Engine halves: memql#5137 and memql#5146 are merged. Design records live in the ENGINE repo,
`docs/superpowers/specs/2026-09-07-*-design.md`; this repository has no
separate record for either. Operator doc: [docs/local-models.md](docs/local-models.md).

**`internal/worker/hardware` is what this machine IS**, as presence facts
only: chip, memory, gpu {name, vram, backend}, cpu cores, os version,
disk free, runtimes with versions, reportedAt. No serials, no user
names, no paths, no hostname -- and the FIELD SET IS ASSERTED BY A TEST
rather than reviewed, because the payload lands on a registration row
the owner's whole cluster can read and review is where "just this one
path, for debugging" gets through. `Scan` is a pure function of `Probe`
for the reason `inference.Decide` is a pure function of `Host`.

**The class is not the floor**, and conflating them refuses a machine
that works. The floor decides whether this machine serves models at all;
the class decides only which set is RECOMMENDED, so a Linux box with
8 GB of VRAM clears the floor, serves fine, and classes `unsupported` --
and still gets the smallest set. **Usable memory ROUNDS to whole
gigabytes, never floors**: nvidia-smi reports total minus the driver's
reservation, so a 24 GB card reports 23.99 and flooring puts every one
of them a whole class low, systematically and always in the same
direction.

**A probe FIGURE and an ABSENCE are never the same shape.** A case that
scored zero and a case that could not run are opposite facts -- the
first says the model failed, the second says nothing about the model at
all -- so `probe.Figure` carries one or the other and `Measured()` is
the only way to ask. Rendering both as `0` ranks a working model below a
broken one. The suite version is a PIN, not a floor: figures are filed
by `(machine, model, suiteVersion)`, so an unknown version is refused in
BOTH directions. Each case runs under its own deadline, because a
whole-run ceiling loses every figure to the last case that hung.

**Fleet owns model sharing.** The enrolled owner chooses private inference or
specific people and groups on the machine's Sharing page. The engine checks
that stored audience on every routing path and again on the receiving replica.
Cockpit advertises capabilities and enforces its model allowlist; it does not
need a separate sharing preference or a reconnect when the audience changes.
Model sharing never grants commands, files, app sessions or computer control.
**`CapabilityDescriptorSchemaVersion` STAYS 1** for this descriptor change.

**Two of the four modality flags are real probes and two are not.**
`vision` and `imagegen` come from Ollama's own `/api/show` capability
list. `audioin` and `audioout` have no capability anywhere, so a bare
Ollama offers neither and an operator's declaration under
`models.runtimes` is the only source. The alternative was never "probe
harder" -- it was guessing from a model id, and a "kokoro" in a name is
not a runtime that answered. False is ABSENT on the label; there is no
`vision=0`.

**Media and tool payloads travel on the worker stream.** `modelcall/payload.go`
reads images, audio input, speech options and image parameters from the actual
protobuf fields and writes binary outputs on `ModelCallEnd`. Tool definitions,
assistant tool calls and tool-result IDs retain their wire identities. A missing
required payload is refused as `payload_unavailable`.

Declared ASR runtimes select `transcription: openai` (multipart
`/audio/transcriptions`), `whisper-cpp` (`/inference` with a ready `/health`),
or the default chat-audio protocol. Local speech can declare `voices.male` and
`voices.female` aliases. `models.allow` is still required for either modality;
declaring an endpoint does not authorize it. See [local-models.md](docs/local-models.md).

**`--runtime` means two different things and the mix is REFUSED.**
Alongside `--inference` it chooses how Ollama runs (docker | native); on
its own it installs kokoro or image. Kokoro runs in Docker on macOS TOO,
diverging from the inference record's D1 on purpose: D1 forbids a
container there because it cannot reach the GPU, which matters for a
language model and not for an 82M speech model. `--runtime image`
installs nothing at all -- image generation is a capability of a runtime
this machine may already have -- and its refusal states what the runtime
REPORTED rather than which platforms the vendor offers it on.

**The `runtime:<kind>` label now carries a VERSION as its value** and
appears only once the runtime answers a probe, never because an install
command exited zero. That value change, plus the four modality keys,
costs every machine one reconnect on rollout.


## Capacity on every heartbeat (memql-cockpit#539)

The engine admits a model call to this machine only when the model is
loaded or there is room to load it (engine design record
2026-10-18-routes, "Fleet capacity"). `internal/worker/capacity` measures
that on EVERY beat and `SendHeartbeat` carries it as `Heartbeat.capacity`:
the pool (`unified`: the smaller of `vm_stat`'s free + inactive +
speculative and the GPU wired limit less what loaded models hold; `vram`:
nvidia-smi or the amdgpu sysfs nodes; `system`: `/proc/meminfo`
MemAvailable), and every model Ollama's `/api/ps` holds, with the calls the
shared `modelcall.Limiter` is running on it.

**Nothing it cannot vouch for is sent.** No runtime list, no memory reading,
or a probe slower than two seconds sends no report, and the engine admits
the machine on its slots alone. A report claiming no loaded models would get
calls refused that this machine could serve, which is worse than silence.

## The installer's setup report (memql-cockpit#541)

The installer starts the worker and only THEN installs the model runtime,
pulls the models and sets up workbenches, so MemQL OS can show those stages
only through the worker (engine half znasllc-io/memql#6229).
`internal/worker/setupprogress.go` keeps the machine's one report, which
every home's heartbeat carries as `Heartbeat.setup`. The installer sends the
PLAN once the worker is up (`worker control --action=setup --plan=...`) and
FINISHED at its end; `worker setup --inference` and `--workbench` report
their own stage in between (`setupreport.go`), a download with its
whole-model bytes rather than the part in flight.

**Reporting never fails a stage.** Every send is best-effort, and no worker,
or an older one, changes nothing about the install. **Silence is not
progress**: a running stage is re-sent every thirty seconds, so a report
quiet for two minutes is carried as `stopped`, an installer that is gone.

**The person's turn comes first** (memql-cockpit#548). On a computer-use Mac
the plan starts with `permissions`: `worker setup --permissions`
(`setup_permissions.go`) opens the Permissions window and waits until both
grants are reported by the WORKER (MemQL.app holds them, never the setup
process) or the person chooses to go on without them -- **Skip for now** /
**Continue without X** in the window, or Return in the terminal. No timeout
decides for them; with no terminal it does not ask at all (exit 3, stage
failed "Not asked"). The window's choice is an ANSWER on the control socket
(`--stage=permissions --answer=skip`), never a stage report, so the setup
command's keepalive cannot overwrite it; the setup command reads the answer
back from `worker control --action=status` and reports how the stage ended.
**It survives a restart**: Screen Recording needs one mid-install, so the
report (with answers) is kept in `setup.json` at the machine's state root and
read back at start; an install with no stage sends `--reset`, so an earlier
install's report does not stand for it. **A step reaches the cluster at
once**: a report that changes a step (plan, a stage's state or sentence, an
answer, the end) asks every home for a heartbeat now, at most one a second,
and a stream that opens while an install runs beats immediately (Register
carries no report). These beats do not prove the stream; only the cadence
does. The engine writes a changed step ahead of its batch interval too.

## Watched-folder backup (memql#4841)

One folder on this machine, kept arriving in a MemQL Library folder. The engine
half landed in epic memql#4783; this is the half that can see the origin.
`internal/worker/backup/`, plus `memql worker backup [--once]`. Operator doc:
[docs/watched-folders.md](docs/watched-folders.md).

**ONE-WAY SYNC IS EXPLICIT.** New `mode: sync` watches upload changed regular
files and conditionally move confirmed local deletions to the Files Bin. Legacy
watches without that mode retain deleted originals with `origin_gone`. A
complete scan, the same root identity and fresh local consent are required
before deletion; the cluster also checks the exact file version and SHA-256.
`internal/worker/transfer` handles bounded direct Blob transfers and downloads,
with durable checkpoints and separate `backup.download_roots` consent. Archive
uploads do not create a watch. The shared server quota includes retained Bin
copies and reservations; native workers cannot bypass it.

**The credential is the SIGNED-IN USER'S, and it has to be.** The Library's
HTTP routes gate on the actor RESOLVING TO A USER -- the upload path stamps
`ownerUserId` from `actor.userId`, so a credential with no user behind it has
nowhere to put the bytes. The `mql_wkr_` token this process authenticates its
STREAM with is one of those: it names a machine, is admitted on WorkerService
and nowhere else, and no HTTP middleware reads it. Neither can a PAT (PATs
verify only on the identity node). **The rule is not "class must be `user`"** --
since memql#4863 an app session's back-channel is `class="app_session"` whose
`sub` is the owning user's id, and `/artifacts` admits it, which is what makes
`appsession`'s Library pull and push work; the classes that stay off the
surface are the ones naming a MACHINE. The sweeper runs as the person anyway,
and for its own reason: a folder backup is that person's files moving and must
not wait on a delegated run's short-lived credential existing.
A machine that is paired but not signed in backs nothing up, which is the
ordinary state of a fresh worker and must not be a startup failure --
`backupBearer` returns nil and the manager is a working no-op. The sign-in is
resolved NON-INTERACTIVELY (`auth.EnsureValidTokenNonInteractive`): under a
LaunchAgent the browser step is not slow, it is a block on a window that will
never open.

**The graph says WHICH folder; this machine says WHETHER.** A watch row is
written from a browser, so its path is one the cluster names on somebody else's
computer -- the situation `appsession`'s `CheckWorkspace` exists for, and the
same answer. `policy.yaml`'s `backup.roots` is default-deny, and a refusal is
REPORTED (`originState=refused_by_policy`) rather than silent: a machine that
quietly ignored a watch is indistinguishable from one that is offline.

**A SCHEDULED WALK, NOT fsnotify**, and this is a decision rather than a
shortcut. A backup must RECONCILE -- everything that changed while the process
was down produced no event; the verify lane has to look on a schedule anyway to
answer `stale` and `origin_gone`; and recursive watches are not portable
(inotify is one watch per directory against a per-user cap, and exhausting it
presents as a backup that silently stops noticing). It also adds no dependency.
fsnotify is a sensible ACCELERATOR later; the sweep stays the source of truth.

**Three gates, cheapest first**: an unchanged (size, mtime) costs nothing; a
moved stamp with an unchanged digest costs one read and no upload; only new
bytes are sent. **The size a push DECLARES comes from a stat taken immediately
before it**, never from the walk's -- a stale smaller size makes the session
route send only that many bytes of a file that has since grown, the engine's
commit check passes (staged == declared), and the copy is silently truncated
and then never repaired, because the digest already matches.

The ledger under `<state_dir>/backup/<watchId>.json`, with its append journal, is a CACHE -- losing it
costs one expensive sweep, because every re-push is keyed on `(machine, path)`
and lands as a new VERSION rather than a duplicate. It is saved BEFORE sending a chunked session's first bytes and after each
completed file, as well as on sweep exit. The saved session is bound to a
SHA-256, never size alone. Uploads read a private disk snapshot, not a live host
file. A lost completion response is recovered idempotently. Root unavailability
is not evidence that every file was deleted. Device/inode PLUS digest allows a
rename within an authorized watch to retain its file identity; a same-name or
same-content guess never does. Optimistic version preconditions preserve edits
made in Files. See docs/watched-folders.md for recovery and local spool costs.

**`memql worker backup --once` needs a recorded registration id.** The id
arrives on a RegisterAck inside a connection only the running worker holds, so
the loop persists it and the command reads it. Without one the command REFUSES
-- sweeping as nobody matched no rows, pushed nothing and reported success.

**A 200 is not success.** `/memql/query` answers HTTP 200 with the refusal in
an `errors` array, so a client that only checked the status would read "you may
not see these rows" as "you are watching nothing" -- and a backup with nothing
to do looks exactly like one that is up to date. Every call reads `errors`
first.

## CI pipeline steps (memql#5494)

`workerHost.pipeline_step` runs one of the cluster's CI pipeline steps on this
machine with the clone-and-command contract of the cluster's Kubernetes Job.
Engine half: epic memql#5478. `internal/worker/tools/pipeline_step.go`, the
`pipelines` block in `policy.go`. Operator doc:
[docs/pipelines.md](docs/pipelines.md).

**ADMITTED BY POLICY, NEVER BY A WINDOW.** A step arrives when a push lands,
so the owner's consent is STANDING: `pipelines.allow`, default-deny, checked
by the step before it touches anything. `consent.AdmittedByPolicy` names the
one pair the dispatcher does not ask the window about; adding a pair there
whose handler checks no policy runs it on nobody's say-so. The block
REPLACES on reload (a consent a SIGHUP could not withdraw is a grant the file
no longer states).

**WHAT `allow` GRANTS IS THE PIPELINE RUNNER'S CHOICE OF COMMAND, as the
owner, for the repositories it names** -- the machine cannot see the pipeline.
Two refusals hold a step to that: one whose `ToolDispatch.agent_id` is set
(the engine's pipeline dispatch carries no agent; an agent's tool loop always
does), and one whose clone URL's path does not name its `repository` (so
`repos` filters what is CLONED, not a name the request states).

**THE LABEL IS THE POLICY'S, NOT worker.yaml's.** Register carries
`pipelines=allowed` exactly when `pipelines.allow` is true, and REMOVES an
operator label of that name -- the opposite of the model labels' rule -- because
the engine's router requires the label, and a machine that would refuse every
step routed on it must not carry it. Like every label it binds at Register, so
a changed allow flag OR repository list re-registers (`maybeReadvertiseModels`,
under the same busy and floor guards). The capability descriptor carries
`repositoryScopes["workerHost.pipeline_step"]`: absent means no consent,
a present empty list explicitly accepts every repository. The engine filters
before dispatch; the live policy check here still decides execution. Unknown
scope metadata is ineligible, so engine and Cockpit upgrade together. The
implementation also reports `actionContracts["workerHost.pipeline_step"] = 4`;
the engine must require it before dispatching the streamed-artifact contract.
An operator label cannot substitute for this build-provided metadata.

**THE TOKEN NEVER TOUCHES DISK OR ARGV**: it is an `http.extraheader` in
`GIT_CONFIG_COUNT/KEY/VALUE`. And the fetch is hermetic, because "an empty
token is an anonymous fetch" is false otherwise: no global or system gitconfig
(an `insteadOf` would fetch with the owner's SSH key), credential helpers
reset, the machine's `GIT_` variables stripped, no prompt, https only. The
NATIVE COMMAND inherits the machine environment, excluding
`MEMQL_WORKER_TOKEN`. Execution and OS/architecture are explicit; missing or
mismatched contracts refuse before cloning. CONTAINER work uses its pinned
image on a freshly checked Docker daemon, with the checkout and declared cache mounted;
request environment travels on container stdin and cannot configure the host
Docker client. No fallback to native execution.

**SERVICES AND CACHES ARE CONTAINER CONTRACT 3.** Services share localhost on
an attempt-specific bridge, with pinned images, bounded resources and Docker-
managed readiness probes. They receive no checkout, cache, socket or step
secrets. Persist every resource name BEFORE creation and reconcile every name
before clearing capacity. Cache reuse is partitioned by the locally bound
cluster enrollment, runner owner/repository/trust scope, clone URL, platform,
image and UID/GID. The 10 GiB limit is retention BETWEEN builds, not an in-flight
disk quota. Never prune while an uncertain container may still be writing.

**BUILD CAPACITY IS SHARED ACROSS CLUSTERS.** A per-OS-user kernel lock lives
outside cluster-specific homes. The durable attempt record is written before
starting the command/container. A worker crash releases the kernel lock but
not that record: another worker reconciles only that exact container on the
same daemon before reuse. Interrupted native work and uncertain cleanup
require reconciliation. Never unlink the lock file (a second inode defeats
mutual exclusion), and never sweep containers by a broad label. This pipeline
reservation does not yet reserve agent workloads or other OS users' resources.

**EVERY CHUNK BEFORE THE RESULT.** The engine relays a `ToolStream` only while
its call is pending and drops one that arrives after the result, so
`Dispatcher.DispatchStream` returns only once its output is sent and the
runner sends the result after. Output is MASKED AS THE CLUSTER MASKS IT
(the engine's `pipelines.MaskSecrets`): every form of a value -- as stored,
trimmed, each line of a multi-line one trimmed (a line ends at a newline OR A
CARRIAGE RETURN, so each part of "user\rpass" masks alone), four bytes or
more -- and values that overlap where printed as one mask, a LINE AT A TIME,
as the cluster masks the lines it reassembles, so how the output was read
changes nothing and a value stored with a line end never takes the line end
with it.
It is cut only at line ends (or, past 64 KiB of one line, where the line's
masking is decided: a span's mask goes out where it starts and its reach is
carried to the next part), because masking a chunk cannot see a value split
across two -- and a line is HELD past 64 KiB, from the first byte of a value
that may still be arriving, until all of it can be seen: sending it whole
leaked such a value in full. The runner ENDS a tool call on
`ToolCancel` and on the loss of its stream: it does not reconnect until its
calls return, so a step left running would keep the machine off the cluster
for its whole timeout.

**ARTIFACTS STREAM BEFORE THE RESULT.** Contract 4 spools a private GNU tar.gz
inside the owned workspace and emits `ToolStream.data_chunk` frames of at most
256 KiB. The terminal result names `artifactsArchiveBytes` and
`artifactsArchiveSHA256` (prefixed `sha256:`). The receiver must verify both
before extracting or filing anything. No inline Base64 archive remains.
At most 1024 files, 256 MiB raw and 256 MiB compressed may travel; the engine's
configured quota can be lower. Over a size/count limit the result says
`artifactsTooLarge` and keeps the command's exit status. Transfer failure or
cancellation never returns a success receipt. Both sides spool to disk and
clean up under the attempt's existing lifecycle.

## App-session recording (memql-cockpit#440)

Every tool call an app completes leaves the session as ONE normalized
`event` chunk, and the session's first event is the environment
fingerprint. Engine half: epic memql#5396, **not merged**; the record is
the ENGINE repository's
`docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md`
(epic B; D2, D5, D12, D16), and this repository has no separate record.
`internal/worker/harness/record.go` is the wire shape;
`claudeactions.go` / `codexactions.go` translate each app;
`internal/worker/appsession/record.go` and `fingerprint.go` are the
machine's side. Operator doc: [docs/local-apps.md](docs/local-apps.md).

**One shape, normalized HERE, so the engine never parses a vendor
format.** `memql.app_session.action` carries `seq, turn, id, parentId?,
tool, appTool, args, argsDigest?, argsOmitted?, cwd, command?, mcp?, url?,
query?, exitCode?, isError?, resultType?, resultDigest?, contents?,
incomplete?`; `tool` is the closed set exec / fs_read / fs_write / fetch /
mcp / agent / other.
`agent` is the app's own bookkeeping (known to touch nothing); `other` is
unclassified and its effects are UNKNOWN -- guessing `agent` for a tool
that sent a notification would let a replay skip it. No proto change:
the type words are namespaced because the same stream carries the apps'
own events verbatim. `TestActionWireContract` pins the names; the engine
has not written its decoder, so this repository DEFINES them.

**`seq` counts ACTIONS, densely, from 1, across every turn; the
fingerprint is 0.** It is not the chunk seq: a gap in it says exactly one
call is missing, which a chunk gap cannot.

**Every unknown stays unknown.** `exitCode` is present only when the app
reported one. Claude Code has no exit-code field: a failed Bash result's
text opens "Exit code N", and a succeeded one exited 0 ONLY when
`tool_use_result` says it ran to the end in the foreground with no
`returnCodeInterpretation` -- `grep` finding nothing exits 1 and comes
back as a success. A call the app started and never finished is flushed
at the end of its turn with `incomplete: true` and no result: never
dropped, never assumed. A result line over the 1 MiB parse bound is one
way that happens. The same rule reaches every field: an item with no
status (a Codex web search, image view, sleep) records NO `isError`; a
result nobody reported is ABSENT, never the digest of null (every file
change would share it); a declined command never ran and has neither an
exit code nor a result; and a Codex item of a type this build has never
seen is recorded as `other` with the item as its arguments -- an extra
`other` costs a replay a fallback, a missing call costs it a skipped one.
Only the conversation's own item types are left out (`codexNonCallItems`).

**The harness names files; the SESSION decides what leaves the machine.**
Contents are read when the call completes, only if it succeeded, only if
the path resolves (symlinks followed) inside the workspace and outside
the scaffolding (`.mcp.json` with the per-run bearer, `.memql-session/`,
a moved-aside config, the `.memql-mcp-*` temp file the bearer is written
through), and only a regular file checked on the OPEN descriptor
(`O_NONBLOCK|O_NOFOLLOW`, so a pipe cannot hang the session). The check
is repeated on the file that OPENED -- `/proc/self/fd` on Linux,
re-resolve plus `os.SameFile` elsewhere -- because a directory swapped for
a link between the check and the open is followed. The configuration files
and every directory up to the workspace are also matched BY IDENTITY,
because a hard link to the bearer's file or a Mac's case-insensitive
`.MCP.json` is a second name no string compare sees. (A hard link to some
other file under `.memql-session/` is an ordinary file; the credential scan
still applies to it.) The write rule and the digest cache use the path that
OPENED, not the one checked.

**A READ NEVER SENDS MORE OF A FILE THAN THE APP ALREADY DID.** A READ's
bytes travel only when they equal what the app's own result already carried
(`Content.Seen`, `json:"-"`: Claude Code's Read record, a Codex command's
output when its parse names exactly one read). `head -1 .env` reported one
line, so the rest travels as a digest (`digest_only`). A WRITE's bytes are
the app's own output and travel, except under a `pushExcludedDirs`
directory (`.git/config` holds remote URLs and their tokens). A file
holding any credential the session was given travels as neither data nor
digest (`contains_credential`), at any size -- it is scanned in the pass
that hashes it -- and the end-of-session push skips it too
(`contentPolicy.readOutput` scans the same bytes it bundles and hashes):
the redactor rewrites text on its way out, but it
never sees inside base64 or a file pushed whole, and the bearer cannot be
revoked. A remembered digest is trusted only while the redactor holds no
new credential. 256 KiB per file and 1 MiB per action travel inline; up to 64 MiB
is digested (remembered by identity, size and mtime once the mtime is 2s
old, so forty reads of one large file hash it once); the rest carries a
closed `omitted` reason. The digest is over the whole file, not the window
the app read. Codex reads with its shell, so its reads come from its own
command parse (`commandActions` / `parsed_cmd`) and nothing else.

**ONE ACTION IS AT MOST 8 MiB ON THE WIRE** (`maxActionBytes`, under the
stream's 32 MiB). Arguments travel whole and can carry a file (a Write, a
Codex patch deleting one), so an oversize action sheds its inline bytes
first, then its arguments (`args: null`, `argsDigest`, `argsOmitted:
too_large`, and the command / url / query read out of them), and is not
sent at all past that -- the hole in the dense seq says so.

**The recording is uncapped.** `limits.max_transcript_bytes` bounds the
narration the engine keeps on the row; an action is not narration, so
`Sink.Record` is a method of its own (never a stream word a typo could
turn into narration) and the session sends it past the cap.

**ORDER ON THE WIRE IS THE ORDER OF THE SEQ, and two locks keep it.** The
engine DROPS a chunk that arrives behind a higher seq. The session numbers
and sends every chunk under `sendMu`, because the recording is sent from
the harness's goroutines while narration goes out from others; and the
Codex clients complete calls on the reader but flush a turn's open ones
on the turn's goroutine, so they number and send under the recording's
own lock (`completeAndEmit` / `flushAndEmit`). Numbering under a lock and
sending after it loses whichever lower chunk came second. A call routed
as progress rather than tool activity is recorded after the app's own
line for it, as a tool call is.

**The fingerprint is facts to compare, so values are digests.** App id,
version (the session manager's own Detector -- a second probe of the
binary, not the registration's answer) and harness; platform; a fixed
toolchain asked `--version` in `/`, cached by binary stamp -- on darwin a
`/usr/bin` shim is never run without the developer tools, because it
answers by opening an install dialog from a LaunchAgent; the workspace
LISTING digest (names and kinds, dependency directories listed but not
entered, scaffolding left out, a moved-aside config listed under its own
name); the harness-named variables as set-or-not plus digest; the Library
inputs. `CODEX_HOME` is deliberately not a named variable: it is fresh
for every session and would match nothing. The four slow parts run at
once, in front of the app's start, and a version probe that leaves a
child holding its stdout is released by `cmd.WaitDelay`.

## The role is a slug with a rank (memql-cockpit#403)

`memql access` prints what a cluster says about the credential on THIS
machine: the user it resolves to, the role held, the groups, the account
scope. `internal/access/`. Engine epic memql#5166; the record is the
ENGINE repository's `docs/superpowers/specs/2026-09-07-roles-as-data-design.md`
section G, and this repository has no separate record. Operator doc:
[docs/access.md](docs/access.md).

**BY NAME, NEVER BY NUMBER.** Both records that add fields to
`MyAccessResult` settle the NAMES and hand the NUMBERS to whoever writes
the engine change -- "field numbers are chosen by the implementer against
the current message; the names are the contract". A number written down
here is a GUESS the engine is free to contradict, and the failure is the
worst kind: whatever the landed message puts at field 11 would render as
somebody's role. It also rules out the obvious alternative -- proto keeps
unrecognised fields as `unknownFields` BYTES KEYED BY NUMBER ONLY, the
name never travels, so `role` cannot be pulled out of an unknown-field
blob without already knowing the number the records decline to settle.

**PRESENCE COMES FROM THE RESPONSE, NEVER FROM THE DESCRIPTOR**, and this
is the trap the by-name design sets for itself. The descriptor is compiled
into the binary, so the moment the pin moves past memql#5181 EVERY build
carries `role` -- and a check that only asked "does the field exist?"
would report every answer as reported, including from a node one release
behind that sends nothing. A cluster that said nothing would render as a
person who holds nothing everywhere, which is the exact inversion this
surface exists to prevent. So the descriptor decides only whether a field
COULD arrive (`OnTheWire`); the value decides whether it DID (`Reported`).
The two get different sentences, because only the first is explained by a
pending engine issue and telling somebody to wait for a change their
cluster already has is its own wrong answer.

**What proto3 cannot tell apart, this package does not claim to.** An
empty repeated field and an absent one are THE SAME BYTES, and so are a
false bool and an unsent one -- the limitation `apps_present` exists for.
So "in no groups" and "sent no groups" collapse, and they collapse toward
the SAFE reading: a person told "not reported" looks further, where one
told "none" believes they hold nothing. `rank` is never read alone for the
same reason -- an int32 of 0 puts no bytes on the wire, so rank 0 and no
rank are one silence; attached to a slug that did arrive it becomes the
record's "holds nothing", which is the sentence that explains every
refusal the reader is about to hit.

**Nothing in this repository names the retired enum, and a test says so.**
`TestNoCockpitCodePathNamesTheRetiredRoleEnum` walks every Go file with
nothing excluded but build and vendor directories (its needles are
assembled from fragments so it does not match itself). The criterion holds
vacuously today -- the slim-down deleted the TUI that read the enum --
which is exactly why it is worth pinning: the next person needing a role
will find the generated getter for `cluster_role` sitting on the pinned
proto and use it because it compiles. That renders identically for the
five predefined roles and breaks only for the custom role the epic exists
for.

**The ceiling is enforced by a `select`, not by the context.** The SDK
opens its stream on `context.Background()` BY DESIGN -- the stream must
outlive the connect timeout -- so a deadlined context handed to
`sdkclient.Connect` never reaches the stream open. Against a peer that
completes a TCP handshake and then says nothing, which is precisely the
"cluster is not answering" case the ceiling is for, the deadline passes
unnoticed and the command waits forever at a prompt somebody is sitting in
front of. The sign-in is deliberately OUTSIDE the ceiling in the other
direction: it opens a browser, and twenty seconds is a terrible limit on a
person finding a window.

**The fields do not exist at the current pin** (memql#5181 and memql#5165
are both unmerged), so `memql access` reports them as not reported and
names the engine issue. No cockpit CODE changes when they land, but the
binary must be rebuilt at a pin that carries them -- the generated
descriptor is compiled in. `future_wire_test.go` builds the message the
records describe, at field numbers DELIBERATELY not the ones they
illustrate, and runs the real decode against it; that is the only way to
test a wire this repository cannot yet see.

## The stream, hardened (epic memql-cockpit#425)

One worker process holds one `WorkerService.Stream` per enrolled cluster, and
this is what keeps those streams honest. The findings are the cockpit half of
the 2026-09-13 connection-layer audit (a comment on znasllc-io/memql#5327);
the engine half is epic memql#5327. There is no separate design record.

**EVERY WRITE REACHES THE SDK'S LOCK THROUGH ONE SEAM.** grpc-go forbids
`SendMsg` from two goroutines on one stream, and this worker writes from the
heartbeat, the recv loop (Pong), every tool dispatch, every model call's
deltas, every pull and every app session at once. The lock lives in
`sdk/go/worker.Connection.Send` (memql#5351, which also deleted the raw
`Stream()` accessor); `Connection.Send` in `connect.go` is the ONLY call to it
here, and `TestEveryWorkerWriteGoesThroughConnectionSend` walks every Go file
to keep it that way. Do not add a lock in the cockpit "to be safe" -- it
protects nothing the SDK's does not -- and do not call the SDK connection
from anywhere else. CI runs the suite under `-race`.

**A WORKER-SIDE REFUSAL IS A FAILED CALL.** At the current pin the engine
treats any error a worker ends a model call with as the call having run, and
does not try another machine. So nothing here refuses a call to make itself
convenient: the model ceiling WAITS rather than refusing its way to an idle slot. Revisit both once the engine reroutes a
refusal made before start.

**A REFUSAL BY THE CLUSTER IS NOT A BLIP, AND NOT A REASON TO DISABLE A
HOME.** Unauthenticated, or the engine's exact sentences for an inactive or
expired token or a revoked registration, get a HOLD once the cluster has
refused at least three times IN A ROW (an unreachable attempt starts the
count over) AND for at least 30 seconds: one error line, a
retry every minute doubling to fifteen, the `worker_home_refused` gauge, and
`<home state dir>/refused.json`, which `memql worker config` prints
(`refusal.go`). The engine maps EVERY token lookup failure to "invalid worker
token", so a database blip during a deploy looks exactly like a revocation
from here -- which is why the grace exists and why nothing writes `enabled:
false`. The first accepted handshake ends all of it. A Send that answers
io.EOF during the handshake is followed by a Recv, because grpc only reports
the real status there; without it a refused token read as "send: EOF".

**THE BACKOFF RESETS ONLY FOR A STREAM THAT PROVED ITSELF** -- one heartbeat
sent. A cluster that accepts Register and drops the stream used to be asked
again every second, each time a full Register and a hardware scan.

**MODEL SHARING NEEDS NO RECONNECT.** The engine uses the owner's persisted
Fleet audience for each new call. Model inventory and repository-scope changes
still re-register, waiting until work in flight finishes.
The heartbeat goroutine
is joined before `runStream` returns, so a stale one can never act on the
next stream's state. Each stream is opened on its own context, and
`Connection.Close` cancels it if the graceful half-close is still stuck
after `closeGrace` -- a Send blocked behind a cluster that stopped reading
would otherwise hold it indefinitely, since such a peer still acks
keepalive pings.

**THE WORKERS FILE IS THE WHOLE TRUTH ONCE IT EXISTS.** `worker run` reads
`worker.yaml` as a home only on a machine that has never had a
`workers.yaml` (`decideRunMode`). `syncLegacyMirror` keeps the legacy mirror
naming an enabled home with its current token, or deletes it -- on every
`unpair` and at every `worker run` start, so a token an older unpair left
behind does not stay on disk. It touches ONLY the mirror, the `worker.yaml`
beside `workers.yaml` (`isMirrorOf`): a `--config` anywhere else is a file
a person wrote. A registry with nothing enabled makes the
worker wait, connected to nothing. Paused enrollments still run the fleet's
local control socket so the menu can resume them. A registry with no homes left
uses the empty-registry wait: at a terminal it exits; under the LaunchAgent or
user unit it waits (SIGHUP does not end the wait), because both restart an exited
worker. See `docs/macos-menu.md` for the owner-only control socket and native UI.

**ONE MACHINE ID, AT THE STATE ROOT.** `machineStateRoot` maps a per-home
state dir (`<root>/homes/<id>`, which the legacy mirror carries too) to
`<root>`, where `machine-id` lives; the fleet resolves it once before any home
connects, resolution is serialized in-process, and a per-home id from an
earlier build is ADOPTED, not replaced.

**PER CLUSTER: CONSENT, METRICS, STREAMS. PER MACHINE: THE MODEL CEILING.**
`consent.Homes` holds one window per home and each home's dispatcher asks its
own; `consent revoke` with no `--cluster` closes every window. Each
scoped directory read instead requires a freshly checked engine receipt and
honors the local stop independently of window expiry. `consent revoke` also
persists that stop per home under the policy directory's `consent/`; local
`consent grant` resumes it. Both worker startup paths must use
`consent.NewPersistentHomes`, never an in-memory registry. Corrupt state
fails closed; a directory receipt cannot authorize writes or commands.
Every per-stream metric carries `home`, and each home's series exist from startup.
Two enabled homes on one cluster open ONE stream (`runnableHomes`: the later
entry, which holds the newer token); the file stays valid so pair and unpair
still work on it, and the installer matches clusters by host as Go does so it
no longer produces one. The model concurrency ceiling is one
`modelcall.Limiter` shared by every home -- a semaphore: a call that finds
the machine full waits, with keepalives, for up to its own idle ceiling.
`Fleet.Run` builds every home before it starts any, and joins every
goroutine it starts.

## Worker + auth notes

- **Enrollment:** `memql cluster add <domain>` fetches
  `https://identity.<domain>/.well-known/memql-config.json` (falling back to
  the api./identity. convention when discovery is unreachable), registers the
  cluster and signs in. On a box with no browser (`DISPLAY` unset on linux, or
  the launch fails), sign-in falls back to the RFC 8628 device flow against
  `/device/code` + `/oauth/token`.
- **Services:** macOS LaunchAgent label `com.znasllc.memql-worker`; Linux
  user-systemd `memql-worker.service`. Installers retire the pre-rename
  `memql-cockpit-worker` agent/unit and binaries in place. **MemQL.app
  registers its own** (memql-cockpit#545): the worker and menu plists live in
  the bundle and `memql worker service register` hands them to SMAppService,
  because macOS lists a per-user plist as a bare executable unless a Team ID
  ties it to the app. The bundled worker plist is the same for every account,
  so `prepareAppService` (`app_service.go`) supplies HOME, the log redirect and
  `~/.memql/state/service.env` itself; `launchctl print` then names the
  program inside the bundle, not by path. Headless installs keep their plists.
- **Unpairing:** `memql worker unpair --cluster <id>` removes (or
  `--disable`s) one home and keeps the legacy mirror honest; the running
  worker keeps that stream until it restarts, and the command prints the
  restart for the platform.
- **Credential stores:** OS keyring preferred, file fallback
  (`~/.memql/credentials/`, 0600). `MEMQL_COCKPIT_CRED_STORE` forces one.
  The keyring service name stays `com.znasllc.memql-cockpit` on purpose —
  renaming it would strand every existing entry for zero user-visible gain.
  The worker's own run path deliberately never resolves the keyring (a
  LaunchAgent must not trigger Keychain prompts); it keeps the lazy file
  default.
- **worker.yaml** (`~/.memql/worker.yaml`) carries cluster URL, worker token,
  name, capabilities. `memql worker config` prints the effective config.
- **The cluster pings; the worker pongs** (memql#5218, D11). A `Ping`
  arrives a few seconds after RegisterAck and then once a minute; the
  answer echoes `sent_at` VERBATIM (the agent measures against its own
  clock, so nothing here can shape the figure) and is logged at debug,
  never info -- one line a minute per machine forever is noise.

## Testing

`go test ./...` is the whole suite (single module). The installer shell
library has its own tests: `bash scripts/install/lib_test.sh`.

**Fuzz targets assert a PROPERTY, never "does not panic."** None of the
fuzzed functions crashes; their failure mode is returning something
plausible that is wrong, so a target that only checked for panics would
find nothing. Each one states the thing that would actually go wrong: a
filename that escapes a workspace, a session id that escapes the ledger
directory, a bearer that survives redaction, an argv the printed command
does not describe, a label attribute forged through `quant`, two tool
calls whose arguments merge. Seeds run in the ordinary suite; the `fuzz`
CI job walks every target for 20s. It DISCOVERS them (one grep pass for
package and name together) rather than listing them, so a target added
without touching the pipeline recipe still runs.

Adding one: put it in `fuzz_test.go` beside the code, and write down the
property in the doc comment. A crasher is written to
`testdata/fuzz/<Target>/` by `go test` itself, so the reproducer is
committed with the fix rather than described.

## Releases

`make release` recommends the next semver; `ARGS="--prepare"` opens a PR that
bumps VERSION and both binary version fallbacks. After native checks and merge,
tag the reviewed main commit and start its release build in MemQL Pipelines.
The native run retains per-platform raw binaries (`memql-<os>-<arch>`), tar.gz
archives (`memql-<version>-<os>-<arch>.tar.gz`), Mac app/menu archives, checksums
and binary SBOMs. MemQL verifies and publishes the approved retained candidate
to the GitHub Release consumed by the installers. See VERSIONING.md.

The installers verify each downloaded binary against its manifest before running it;
a headless binary from an older release is vouched for by its verified archive.
Each installer lists the lib.sh functions it calls (`REQUIRED_LIB_FUNCTIONS`,
kept complete by `lib_test.sh`) and refuses an incomplete cached library after
one refetch past the cache.
