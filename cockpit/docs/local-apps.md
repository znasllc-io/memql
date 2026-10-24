# Local apps as execution surfaces — the machine side

The cockpit can let a planner Task run inside an app the machine's owner
**already pays for** — Claude Code or Codex — on their own computer, with
MemQL's tools reachable from inside that app over MCP.

The canonical record is the engine's
[`docs/public/operate/local-apps.md`](https://github.com/znasllc-io/memql/blob/main/docs/public/operate/local-apps.md)
(epic [memql#4358](https://github.com/znasllc-io/memql/issues/4358)). This page
is the half that happens on the machine: what to configure, what gets written
where, and what to check when it does not work.

---

## Turning it on

The Linux and macOS Fleet installers detect installed Claude Code and Codex
executables and allow them for the cluster being added, before starting the
worker. This also runs when you repeat the installer to repair an enrollment.
Add `--no-apps` to install or repair without changing app permissions. Detection
uses PATH and the same standard app directories the worker searches.

Permission setup uses Cockpit 0.17.0 or later. If it fails, installation continues
and prints a recovery command. Apps installed later are not automatically
allowed by the running worker; use the command below. An app still needs its
own sign-in before MemQL can use it.

**1. Allow the app for a cluster** — the machine owner's word, given to ONE
cluster at a time:

```sh
memql worker apps --allow claude-code --allow codex --home https://api.memql.localhost
```

That writes `apps.homes.<cluster>.allow` in `~/.memql/policy.yaml` — a textual
edit that leaves the rest of the file, comments included, exactly as it was —
and signals the running worker. The cluster sees the change on the worker's
next heartbeat. `--deny` withdraws it the same way; `--home` takes the home id
from `workers.yaml` or the cluster's URL, and may be left out when the machine
is enrolled with only one cluster. By hand, the block is:

```yaml
apps:
  homes:
    api.memql.localhost:    # a home id from ~/.memql/workers.yaml
      allow:
        - claude-code
        - codex
```

**Default-deny, per cluster.** A cluster the block does not name is allowed
nothing — the installer grants only its named cluster. An app session does
exactly what `workerHost.exec` does, so it gets the same default-deny posture
as the rest of `policy.yaml`, and consent given for a local test cluster is
never consent for production. `SIGHUP` (or restart) after a hand edit; the
block is replaced on reload, so a withdrawal takes effect without a restart.

**Upgrading from the machine-wide `apps.allow`.** That list is no longer read,
and it allows nothing anywhere: the worker logs a warning naming the command,
and `memql worker apps` shows it. Run `memql worker apps --allow <app> --home
<cluster>` for each cluster you meant; the same edit removes the old list.

**2. Sign in to the app itself.** The engine routes to a machine only when the
app is both **allowed** and **signed in**, so a machine with the binary but no
session is never selected.

Delegation also has to be enabled per USER, in the portal at `/machines`. That
half is not configured here.

**To check both from the machine**, run `memql worker apps`: it names each app
as not installed, present but blocked, allowed but not signed in, or ready —
with the fix for each — and prints what every [level](#levels-how-much-intelligence-in-each-apps-own-words)
runs it at.

---

## What the cockpit reports

A failed heartbeat ends its stream and reaches the normal reconnect path, even
when the inbound read still waits. Owned calls are stopped by that disconnect
path; journaled work remains available for recovery, and permissions are not
renewed by reconnecting. The bounded connection close also handles a peer that
keeps the transport alive but stops reading. `TestHeartbeatFailureEndsAReadThatStillWaits`
reproduces the previously silent failure: without the close, the heartbeat loop
exited while the reader and process stayed alive. This regression establishes
that failure path, not the cause of every offline machine report.

On `Register` and on **every** heartbeat:

| Field | Where it comes from |
|---|---|
| `id` | `claude-code` / `codex` |
| `version` | the CLI's own `--version` output, verbatim |
| `signed_in` | the app's own state files (see below) |
| `subscription` | what the app REPORTS; `unknown` when it said nothing |
| `allowed` | `policy.yaml apps.homes.<this cluster>.allow` — each cluster is told its own |

`Register` also carries one **app descriptor** per app — the harness this
machine drives it through and whether that harness can return a structured
answer and take a follow-up (see [Turns](#turns-and-the-harness-that-drives-them)).

An app that is **present but not allowed** is still reported, with
`allowed=false`. The portal shows it as present-and-blocked, which is
something an operator can act on; omitting it would look exactly like "not
installed".

### How `signed_in` is detected, and why it errs toward `false`

- **Claude Code** — `~/.claude/.credentials.json` (the no-keyring shape), or
  the account record in `~/.claude.json`.
- **Codex** — `~/.codex/auth.json`, an API key or an OAuth token pair.

Nothing here shells out to a real prompt to answer a status question, and
nothing reads the OS keyring: the worker runs as a LaunchAgent on macOS, and a
keyring read there raises a Keychain prompt on a machine whose owner may not be
sitting at it.

**Consequence, stated plainly:** on a macOS box where Claude Code put its token
in the Keychain and wrote no account record, this reports `signed_in=false`.
That is the designed direction of the error. The machine is simply not
selected, and `/machines` says "not signed in" — rather than showing a green
row for a machine that will refuse the run after a plan has already committed
to it.

The version is cached (keyed on the binary's size and mtime, so an in-place
upgrade invalidates it immediately); presence and auth state are read fresh on
every beat, so signing in shows up on the **next beat** rather than the next
reconnect.

---

## Where a session runs

The engine may name a workspace (from the owner's delegation policy). When it
names **none**, this machine chooses one; the engine never invents a path on
somebody else's computer.

| `policy.yaml` | Directory |
|---|---|
| `fs.workspace_root` set | `<fs.workspace_root>/<home>/<key>` |
| not set, macOS | `~/Library/Application Support/MemQL/workspaces/<home>/<key>` |
| not set, elsewhere | `$XDG_DATA_HOME/memql/workspaces/<home>/<key>` (`~/.local/share/...` when unset) |

`<home>` is the cluster home, so two clusters enrolled on one machine never
share a directory. `<key>` is the **run** when the session belongs to one, and
the **session** otherwise:

- a **run's** directory is shared by that run's sessions, so a later step
  finds what an earlier one made. It is kept when a session ends; only the
  session's own scaffolding (the bearer's configuration, the transcript) goes;
- a **session's** directory is removed when the session ends, after its
  outputs have been pushed to the Library — **except** when that push failed
  (the directory then holds the only copy of what the app made; the worker's
  log names it at `WARN`) and for an `open` session, whose terminal a person
  may still be working in. Those two are kept, and nothing removes them yet:
  recover what you need and delete them by hand.

The chosen directory goes through the same `fs.deny` / `fs.workspace_root`
check as a path the engine named. It is **never under `~/.memql`**: that
holds the worker's tokens and the consent file, and a session runs shell
commands where it works.

A workspace the engine named is used as named and never removed.

**No workspace may overlap what a session must never touch**: the worker's own
files (the directory of `worker.yaml` / `workers.yaml` and `policy.yaml` —
`~/.memql` —, its state directory, a `--token-file`, the consent socket) and
every `fs.deny` entry. The workspace is the app's write grant, so one that
**contains** such a path — the home directory, or `/`, which a delegation
policy's workspace root of `~` would name — is refused as surely as one inside
it, with `workspace ... overlaps ...` naming the path.

## What Claude Code may do in a session

The machine owner's grant, passed as flags on every `claude -p` turn —
**not** read from anybody's `~/.claude/settings.json`:

- **edit files and run shell commands inside the session workspace;**
- **call MemQL's tools** over the session's own MCP server;
- nothing else.

| Flag | What it does |
|---|---|
| `--setting-sources=` | loads no settings file: not the user's, not the workspace's `.claude/settings.json`. The machine's **managed** settings still apply — they are its administrator's |
| `--strict-mcp-config` | loads no MCP server but the session's own |
| `--permission-mode dontAsk` | refuses anything not granted below — nobody is at a `claude -p` to ask |
| `--allowedTools "Edit(/**) Read(/**) mcp__memql"` | the file tools under the workspace only (a command-line rule's `/` is the working directory), and every MemQL tool |
| `--disallowedTools "Read(//<path>) Edit(//<path>) ..."` | the file tools may neither read nor write the worker's own files or any `fs.deny` entry — a deny rule beats the allow rule, so a symlink in the workspace does not reach them either |
| `--settings '{"sandbox":{...}}'` | runs every shell command in Claude Code's own sandbox (Seatbelt on macOS, bubblewrap on Linux): writes confined to the workspace and the temp directory, `.mcp.json`, `.git/hooks`, `.git/config` and shell rc files protected, network only through the sandbox's proxy, which this grant approves no domain for. `filesystem.denyRead` / `denyWrite` carry the same paths as the deny rules, so no shell command reads the worker's tokens or an `fs.deny` entry either. `failIfUnavailable` makes a machine that cannot sandbox fail the turn at start; `allowUnsandboxedCommands: false` removes the per-call escape |

Bash has **no** allow rule of its own: it is approved only because it runs
sandboxed. Limits, stated plainly:

- the sandbox confines reads only **away from** the worker's own files and
  `fs.deny`; anywhere else a sandboxed command can still read outside the
  workspace;
- `--setting-sources=` drops **all** of `~/.claude/settings.json`, its `env`
  and `apiKeyHelper` included — Claude Code authentication configured there
  does not reach a session (see Troubleshooting);
- none of this has been exercised against a real prompt from the test suite,
  only against the flags and settings `claude` 2.1.275 / 2.1.283 document.

Every turn also runs with `CLAUDE_CODE_CERT_STORE=bundled,system`, pinned over
the worker's own environment: a local cluster's MCP endpoint is signed by an
mkcert CA that is only in the system keychain.

The turn's first event names each MCP server's status. MemQL's server
reported as `failed` or `needs-auth` **stops the turn at once** with that
status in the error; `pending` (still connecting) does not.

---

## What a session writes on the machine

| Path | What it is | Lifetime |
|---|---|---|
| `<workspace>/.memql-session/mcp-<unique>.json` | Claude Code's explicit MCP config, `0600`, carrying this session's bearer | deleted at session end |
| `<workspace>/.memql-session/codex-<unique>/config.toml` | Codex's MCP config in a conversation-scoped `CODEX_HOME` | credential deleted at session end; provider state retained for attach |
| `<workspace>/.memql-session/codex-refs/<ref-hash>` | workspace-local provider ref to home index, with no credentials or history | retained with the workspace |
| `<workspace>/.memql-session/transcript-<unique>.log` | this session's full transcript | pushed to the Library, then deleted; retained on failed push |
| `<workspace>/.memql-session/open-<unique>/` | this `open` session's launcher and exit receipt | deleted at session end |
| `<state_dir>/appsessions/<id>.json` | the write ledger — **paths only, never the bearer** | deleted at session end |

Overlapping sessions never share transcript or credential paths. Claude receives
its exact private config through `--mcp-config`, including interactive sessions;
the project's `.mcp.json` stays untouched. Startup still restores backups named
by an older worker's crash ledger.

Codex attach reuses the original conversation's provider state in this workspace,
with a fresh credential. An OS lock refuses a second active writer to that home
before it can replace the credential; a crash releases the lock. Older refs use
the former `.memql-session/codex` home in the same workspace. Attach never searches
another workspace or the user's personal conversation history. Provider state
and the reference index remain excluded from output retention.

Temporary Chromium user-data directories are also excluded from automatic
output retention and inline write contents. The worker recognizes `Local State`
together with a debugging-port file or a `Default`/`Profile ...` preferences
file, regardless of the directory's task-chosen name. It checks filenames and
file types without reading profile contents. Screenshots, scripts and receipts
beside the profile still travel. Put other disposable runtime files under
`.cache/`; exclusion does not delete local state or change an archive already
uploaded, so task-owned cleanup remains a separate operation.

### The deletion is the security control

The per-run bearer **cannot be revoked**. The engine's verify path is
JWKS-only and DB-free — that is what lets it work on every node without a
lookup — so there is no row to strike, and revoking one token would mean
rotating the cluster's signing key. Three things stand in for revocation: an
8-hour hard cap at the identity service, renewal in place so no single bearer
is long-lived, and **this file being deleted**.

A `defer` cannot cover a SIGKILL, an OOM, or a machine that lost power, so
every write is recorded in the ledger and the worker **sweeps** it at startup.
A non-zero count in that log line at boot means a previous process died with a
live session:

```
level=WARN msg="swept MCP configuration files left by a previous cockpit process" files=1
```

**Known limitation** (tracked on
[memql-cockpit#348](https://github.com/znasllc-io/memql-cockpit/issues/348)):
Claude Code reads its MCP configuration at **startup**. A `renew_credential`
mid-run rewrites the file correctly, but the already-running process will not
pick it up — so a run that outlives its bearer loses MemQL's tools even though
the file on disk is current. The engine can shorten the run instead; that is
its decision to make, and it can only make it because this says so.

---

## Turns, and the harness that drives them

A session is a **sequence of turns**, not one process with its output read off
the terminal. The cockpit drives each app through the app's **own protocol**,
so what the app said, what it spent, and which session to continue are three
facts rather than three guesses.

| Harness | What it is | Structured result | Usage |
|---|---|---|---|
| `claude-headless` | `claude -p --output-format stream-json --verbose`, **one process per turn**, continued with `--resume <session id>` | yes, via `--json-schema` | yes, from the `result` event |
| `codex-app-server` | `codex app-server`, JSON-RPC 2.0 over the child's stdio (its frames carry no `"jsonrpc"` header, and are read that way), one process per session and one request per turn | yes, via `outputSchema` | yes, per turn |
| `codex-mcp` | `codex mcp-server`, the `codex` / `codex-reply` tool pair over stdio MCP | **no** | **no** |

**Which Codex you have decides which of the two you get**, and only this
machine can tell: `codex app-server --help` is the probe, and a Codex too old
to answer it falls back to `codex-mcp`. The answer is cached against the
binary's size and mtime, so an upgrade takes effect at once. The session runner
and the inventory ask the same question the same way, so what a session drives
and what the machine advertises cannot disagree. The answer rides `Register` as
one **app descriptor** per app — the harness word, and whether it can return a
structured answer and take a follow-up — so the engine never asks this machine
for a protocol it cannot speak, and never sends a structured call to a
`codex-mcp` that cannot constrain one. Descriptors are bound at `Register`: a
harness gained mid-connection (a Codex upgraded to one with the app-server) is
advertised on the next reconnect.

**Codex through `codex mcp-server` reports no usage and cannot constrain the
answer, and both are stated rather than papered over.** The tool pair's output
schema is `{threadId, content}` and nothing else; the token counts on its event
stream are described by Codex itself as "accumulated, estimated, or replayed",
and an estimate written to a ledger somebody bills from is worse than a gap. It
also takes no output-schema argument, and its input schema refuses arguments it
does not declare — so a turn that asked for a schema runs **unconstrained** and
reports a structured result only if the answer happens to parse.

### A follow-up

`AppSessionControl{action: "message"}` starts the **next turn in the same
conversation**. It is delivered even when it arrives while a turn is running:
the prompt is queued and the loop takes it as soon as the turn ends. Turns are
strictly sequential — two processes resuming one Claude Code session id would
both append to the same transcript and neither would see the other's turn — and
a follow-up that cannot be delivered (the session is finishing, or eight are
already waiting) is **refused out loud**, in the log and in the transcript,
rather than dropped.

The session ends when a turn finishes and nothing is queued behind it.

The follow-up's text is the control's own `prompt` field. `reason` is never read
as a prompt — it is transcript free-text on a cancel — and a `message` control
that carries no prompt starts no turn and says so in the transcript
(`[memql] a follow-up arrived with no prompt, so no turn was started`).

Because Claude Code reads its MCP configuration at startup and each turn is a
new process, a `renew_credential` in the middle of a session **does** reach the
next turn. That is the honest form of what renewal could never do for one long
process.

### A structured result

When the engine asks for an answer against a JSON Schema
(`AppSessionStart.response_schema_json`), the schema reaches the app —
`--json-schema` for Claude Code, `outputSchema` on the Codex app-server turn —
and the answer comes back on the **End**, as `AppSessionEnd.result_json`. It
rides a failed End too: an app can answer the schema and still exit non-zero,
and throwing the answer away with the failure would lose the one part of the
run that can be read.

**There is often no structured result, and that is not a failure.** No schema
was asked for; or the app answered in prose; or the app is a Codex driven
through `codex-mcp`, which cannot be constrained at all (its descriptor says
so, and the engine does not send it a schema). The run is reported as having
succeeded, with a note in the transcript, because "the app ran and answered
unstructured" bills and retries differently from "the app never started". A
result is never synthesised: an empty object would read downstream as *the app
answered nothing*.

---

## Levels: how much intelligence, in each app's own words

The engine does not name a model for an app. It names a **level** — how much
intelligence the step needs — on `AppSessionStart.level`, and **this machine**
turns it into the app's own knobs. The knob names are the app's (`--model` /
`--effort` for Claude Code, `model` / `model_reasoning_effort` for Codex), and so
are the values, so the translation lives where the app is.

| Level | What it is for | Claude Code | Codex |
|---|---|---|---|
| `fast` | triage, intake, classification, suggestions | `--model haiku` | `model_reasoning_effort=low` |
| `strong` | an agent's reply, a conductor turn, a design | `--model sonnet --effort high` | `model_reasoning_effort=medium` |
| `reasoning` | emitting or repairing a construct, re-planning | `--model opus --effort xhigh` | `model_reasoning_effort=high` |
| `embeddings` | — | never through an app | never through an app |

Why these values:

- **Claude Code** names models by **alias**, which it resolves to the newest
  model of that tier, so the table does not go stale with each release. `fast`
  passes **no effort** because Haiku takes none — Claude Code drops the flag
  without a word, so it would be a knob that is never turned.
- **Codex** gets an effort and **no model**: Codex has no tier aliases, and its
  model catalogue differs between accounts and releases, so a model named here
  would be wrong for somebody. The account's default model runs, at an effort
  every current Codex model accepts. Name a model per level yourself (below)
  if you want one.
- **`embeddings` never runs through an app**: an embedding has to come from the
  same embedder as the index it is written into, and neither app has one.
- **No level** runs the app at its own defaults, exactly as before levels
  existed.

Both Codex harnesses take the same knobs: the app-server sets them on the
thread (`thread/start` and `thread/resume`), and `codex-mcp` on the `codex` tool
call that opens the session. `codex-reply` takes no configuration, so a
continuation runs at the settings the session started with — and an **attach**
through `codex-mcp` (which continues with `codex-reply` from its first call)
**cannot take a level at all**. It is refused, with the two ways out: attach
without a level, or upgrade Codex to a version with `codex app-server`. A knob a
row leaves unset is decided by Codex's own defaults — the session runs under a
per-session `CODEX_HOME` holding only MemQL's MCP server, not your
`~/.codex/config.toml`.

**Claude Code 2.1.111 or later** for the built-in table: `xhigh` (the
`reasoning` row) arrived in 2.1.111, and `--effort` itself shortly before 2.1.72.
An older Claude Code answers an effort it does not know with its own error or
its default effort (the End reports the effort as unknown either way, since
Claude Code never states one). Upgrade, or override the row — for example
`reasoning: {model: opus, effort: high}`.

### Choosing your own

Override any row in `~/.memql/policy.yaml`, beside `apps.homes`:

```yaml
apps:
  homes:
    api.memql.localhost:
      allow: [claude-code, codex]
  levels:
    claude-code:
      reasoning:
        model: fable
        effort: max
    codex:
      strong:
        model: gpt-5.5
        effort: xhigh
      fast: {}          # no knobs: the app's own defaults for this level
```

- **An absent block is the built-in table**, not "nothing" — unlike
  `apps.homes`, which is default-deny, because this decides *how* an allowed
  app runs, not *whether* it may. Levels are the machine's, not a cluster's.
- **An entry replaces its row whole.** `strong: {model: opus}` runs Opus at
  Claude Code's default effort, not at the built-in `high` — what you write is
  exactly what the app is given.
- **A row you leave out keeps the built-in entry.**
- **`SIGHUP` the worker** (or restart it) and the next session reads the new
  table. The block is replaced on reload, so deleting it puts the built-in
  table back.

**An entry the app would misread refuses its level.** Claude Code *ignores* an
effort word it does not know (it prints a warning and uses its default), so a
typo would otherwise run every session at a default you did not choose. The
cockpit checks every entry first — Claude Code's efforts are `low`, `medium`,
`high`, `xhigh` and `max`; a Codex effort must be a plain word, which the
account's model then judges; a model name must start with a letter or a digit
and contain no spaces — and a bad entry refuses **that level only**, with a
sentence naming the line, rather than quietly falling back to the built-in row
you wrote it to replace. An entry of the wrong **shape** is refused the same
way and never costs the rest of the file: a shorthand (`reasoning: opus` — an
entry is `{model: …, effort: …}`), a key that is neither knob (`efort: max`), or
a list where a single word belongs. An entry for an app or a level no session
can ask for (a typo like `fastt`, an unknown app, `embeddings`) is ignored and
reported.

Both kinds of problem are logged whenever `policy.yaml` is read:

```
level=WARN msg="policy.yaml apps.levels has a problem" problem="apps.levels.claude-code.strong: effort \"extreme\" is not one Claude Code takes (it takes low, medium, high, xhigh, max, and ignores any other word rather than refusing it) -- this machine refuses strong sessions for claude-code until the entry is fixed"
```

### Checking what a level runs at here

A model or effort chosen for one step overrides that field of the resolved
level for that session only. Omitted fields keep the level's value; shared
defaults and `policy.yaml` are unchanged. The same validation runs before
launch, and an explicit choice cannot revive a level refused by policy.
The transcript identifies a session override separately from the level's
defaults. Completed-session model and effort still come from the app's own
report, not from the requested values.

`memql worker apps` prints each app, whether the cluster can use it here (and
the fix when it cannot), how it is driven, and what every level becomes — each
row saying whose entry it is:

```
claude-code  2.1.270 (Claude Code)
  state      allowed and signed in -- the cluster can send it sessions
  harness    claude-headless: structured answers, follow-up turns
  levels     fast        --model haiku                built-in
             strong      REFUSED (see the problem below)
             reasoning   --model fable --effort max   policy.yaml
             embeddings  never through an app
```

It reads the same policy and runs the same detector the worker does, so what it
prints is what a session gets — with one caveat it states itself: it finds the
apps on **your shell's** `PATH`, and a LaunchAgent's `PATH` can differ.

Every session run at a level also says what it became, in its transcript before
its first turn:

```
[memql] level reasoning runs claude-code with --model opus --effort xhigh (the cockpit's built-in table)
```

### What comes back

The End carries the model and effort **the app reported** serving the session
(`AppSessionEnd.model` / `.effort`) — never the knobs this machine passed. A
request is not a report, and the engine records these as what *served*.

| App | Model | Effort |
|---|---|---|
| Claude Code | the result event's `modelUsage` — the session's own model when it spent tokens, else the one that produced the most output | **always empty**: Claude Code states no effort anywhere in its output |
| Codex (app-server) | the thread's `model` as the app stated it (`thread/start`, `thread/resume`, later `thread/settings/updated`); a `model/rerouted` replaces it for the turn it names | the thread's `reasoningEffort` / `effort` |
| Codex (`codex-mcp`) | `session_configured`'s `model` | `session_configured`'s `reasoning_effort` |

Empty means the app said nothing, and the engine records it as unknown. A turn
reports Codex's settings only once it shows the model ran — it completed, or it
reported spend — because Codex states whatever it was configured with, even a
model name nothing answers to.

### Chunk streams

| Stream | What it carries |
|---|---|
| `event` | structured progress, as the app reported it — and the recording ([below](#what-a-session-records)) |
| `text` | assistant prose meant for a person |
| `tool` | a tool call or its result |
| `stdout` | anything printed that the protocol did not account for — a stack trace on the way down |
| `stderr` | the process's stderr, verbatim |

`text` and `tool` are new with the harnesses. An engine that predates them
keeps every chunk regardless and renders the two as narration, so the finer
split is what is lost, never the words.

---

## What a session records

Every tool call an app completes on this machine leaves the session as **one
`event` chunk, in one shape**, whichever app made it and whichever protocol
drove it — so the cluster never has to learn that Claude Code calls it `Bash`
and Codex calls it `commandExecution`. The session's **first** event describes
the machine as the run found it. Together they are the recording the engine
turns into work-spine rows ([memql#5396](https://github.com/znasllc-io/memql/issues/5396)).
An engine that predates that shows each one as a progress line and appends it
to the session's transcript. That transcript is bounded on the engine's side,
so past its bound the actions survive only in the full transcript artifact
this machine pushes when the session ends. Nothing on the wire changed to carry
them: an `event` chunk has always been a JSON body.

The model's **prose is not recorded here**. It stays on `text` and in the
transcript artifact; the recording is about what the app *did*.

### One action per completed call

```json
{
  "type": "memql.app_session.action", "v": 1,
  "seq": 3, "turn": 1,
  "id": "toolu_01ULvDD2AmfTHJ8V4GN3ESkH",
  "tool": "fs_write", "appTool": "Write",
  "args": {"file_path": "/work/out.txt", "content": "hello"},
  "cwd": "/work",
  "isError": false,
  "resultType": "string",
  "resultDigest": "sha256:8f6c…",
  "contents": [{"op": "write", "path": "/work/out.txt",
                "digest": "sha256:2cf2…", "bytes": 5,
                "encoding": "utf8", "data": "hello"}]
}
```

| Field | Meaning |
|---|---|
| `seq` | Counts **actions**, densely, from 1, across every turn of the session. A gap means an action is missing, exactly. The fingerprint is 0. |
| `turn` | The turn the call finished in. A follow-up is a new turn. |
| `id` | The app's own id for the call — the only join back to the app's transcript. |
| `parentId` | The call this one ran inside, for a Claude Code sub-agent's calls. |
| `tool` | `exec`, `fs_read`, `fs_write`, `fetch`, `mcp`, `agent` (the app's own bookkeeping — nothing outside it moved) or `other` (not classified; effects **unknown**). |
| `appTool` | The app's own name for the tool. Provenance only. |
| `args` | The call's arguments, **whole**, as the app expressed them — unless they would make the action too large to send (a Codex patch deleting a large file carries all of it). Then `args` is `null`, `argsDigest` is the `sha256` of their canonical JSON (keys sorted, no insignificant space) and `argsOmitted` is `too_large`. |
| `command` / `mcp` / `url` / `query` | The one thing a reader needs without parsing `args`: the command line an `exec` ran, the `{server, tool}` an `mcp` call reached, what a `fetch` asked for. |
| `exitCode` | Present **only when the app reported one**. |
| `isError` | The app's own verdict. Absent when nobody knows: an `incomplete` call, an end event this build could not read, or a call the app gives no verdict on (a Codex web search, image view or sleep). |
| `resultType` / `resultDigest` | The result's inferred JSON type and `sha256`. Text that is JSON is typed by what it parses as. Absent when the app reported no result — a file change through `codex-app-server` answers nothing, and a declined command never ran. |
| `contents` | The files the call read or wrote — see below. |
| `incomplete` | The app started this call and the session never saw it finish: the process died, the turn was cancelled, or the result was too large to read. Recorded, never dropped, never assumed. |

**What is recorded as unknown stays unknown.** Claude Code puts no exit status
on the wire: a failed Bash call says `Exit code 3` in its text, and that `3` is
recorded; a successful one is recorded as `0` **only** when its own record
shows it ran to the end in the foreground. `grep` finding nothing exits 1 and
Claude Code reports it as a success — that call's `exitCode` is absent, not 0.
The same for a command sent to the background, and for a refusal.

### How each app's tools are classified

| `tool` | Claude Code | Codex (`codex-app-server`) | Codex (`codex-mcp`) |
|---|---|---|---|
| `exec` | `Bash` | `commandExecution` | `exec_command_*` |
| `fs_read` | `Read`, `Glob`, `Grep`, `LS` | `imageView` | `view_image_tool_call` |
| `fs_write` | `Write`, `Edit`, `MultiEdit`, `NotebookEdit` | `fileChange` | `patch_apply_*` |
| `fetch` | `WebFetch`, `WebSearch` | `webSearch` | `web_search_*` |
| `mcp` | `mcp__<server>__<tool>` | `mcpToolCall` | `mcp_tool_call_*` |
| `agent` | `Task`, `ToolSearch`, `TodoWrite`, `Skill`, `StructuredOutput`, … | `collabAgentToolCall`, `sleep` | — |
| `other` | anything else | `dynamicToolCall`, `imageGeneration`, `functionCallOutput`, and any item type this build has never seen | — |

A Codex item of a type newer than this cockpit is recorded as `other`, with
the item itself as its `args`, rather than dropped: a replay has to know a call
happened even when nothing here knows what it did. Only the conversation's own
items — messages, reasoning, plans, review and compaction markers — are not
calls.

**Codex reads with its shell.** It has no read tool; it runs `cat` or `sed -n`.
Its own parse of the command names the file it read, and that is how a Codex
`exec` carries a `contents` entry with `op: "read"` — the same file, digested
the same way, as Claude Code's `Read` of it.

### File contents

A call that read or wrote a file names it, and the **session** decides whether
its bytes leave this machine — the harness never reads a file itself. A file is
read, at the moment the call completes, only when:

- the call **succeeded** — a refused write wrote nothing;
- the path resolves, **symlinks followed**, to somewhere **inside the workspace**
  — checked again on the file that actually opened, so a directory swapped for
  a link in between cannot lead the read outside;
- it is not the session's own scaffolding: `.mcp.json` with the per-run bearer,
  `.memql-session/` (the transcript, Codex's per-session home and the `auth.json`
  linked into it), a configuration moved aside, or the temporary file the bearer
  is written through. The configuration files and `.memql-session/` are also
  recognised by **identity**, not only by name, so a hard link to the bearer's
  configuration, or `.MCP.json` / `.MEMQL-SESSION/` on a Mac where names ignore
  case, is refused too. (A hard link to some other file inside
  `.memql-session/` is read like any other file — the credential check below
  still applies to it);
- it is a **regular file**, checked on the opened descriptor, so a path swapped
  for a named pipe cannot hang the session.

**A read never sends more of a file than the app's own result already did.**
Every file is digested; whether its bytes travel as well depends on why the file
is on the action:

- a file the app **read** travels only when the app's own result carried the
  whole file — Claude Code's `Read` of it, or a Codex `cat` whose output is the
  file. A read that looked at part of a file (`head -1 .env`) sent that part in
  its own result, and the recording carries the rest as a digest
  (`omitted: digest_only`), never as bytes;
- a file the app **wrote** travels, except inside `.git/`, `node_modules/`,
  `vendor/` and the other dependency directories the output push skips
  (`digest_only` there — `.git/config` holds remote URLs and the tokens in
  them);
- a file holding this session's own credential travels as **neither bytes nor
  digest** (`omitted: contains_credential`), wherever it is and whatever its
  size — every file is scanned for it in the same pass that digests it — and it
  is not pushed to the Library as an output either (see Outputs).

| The file | What travels |
|---|---|
| up to 256 KiB | the bytes inline (`utf8`, or `base64` when they are not text), with the digest and size |
| up to 256 KiB, but the action already carries 1 MiB inline | the digest and size, `omitted: over_budget` |
| over 256 KiB | the digest and size, `omitted: over_ceiling` |
| over 64 MiB | the size alone, `omitted: too_large` |

One action is at most **8 MiB** on the wire. The few that would be larger carry
a file in their own arguments; they drop their inline file bytes first (each
file keeps its digest, `omitted: over_budget`), then their arguments (see
`argsOmitted` above), and an action still too large after both is not sent. The
gap it leaves in `seq` says a call is missing, and the worker log names it.

Anything that was not read carries its path and a reason: `outside_workspace`,
`session_scaffolding`, `not_found`, `not_regular`, `unreadable`. The digest is
over the **whole file** as it stood — not over the lines the app happened to
look at — because it is what a later replay compares before it trusts the file
is the same.

The recording is **not** subject to `limits.max_transcript_bytes`. That limit
bounds the narration the engine keeps on the session row; dropping the fortieth
call because the app was chatty about the first thirty-nine would record a
session that stopped halfway. Like every chunk, the recording passes the
session's redactor on the way out.

### The fingerprint

```json
{
  "type": "memql.app_session.fingerprint", "v": 1, "seq": 0,
  "takenAt": "2026-09-13T23:00:41.113Z",
  "app": {"id": "claude-code", "version": "2.1.270 (Claude Code)", "harness": "claude-headless"},
  "platform": {"os": "linux", "arch": "amd64"},
  "tools": [{"name": "git", "version": "git version 2.43.0"},
            {"name": "go", "version": "go version go1.26.6 linux/amd64"}],
  "cwd": "/work", "cwdDigest": "sha256:…", "cwdEntries": 42,
  "variables": [{"name": "PATH", "set": true, "digest": "sha256:…"},
                {"name": "TZ", "set": false}],
  "inputs": [{"artifact": "art_123", "path": "/work/spec.md", "digest": "sha256:…", "bytes": 2048}]
}
```

- **`tools`** are `cargo`, `docker`, `git`, `go`, `make`, `node`, `npm`,
  `python3` and `rustc`, where installed, each as it reports its own version.
  They are asked once and cached against each binary, so back-to-back sessions
  cost nothing. On a Mac without the command-line developer tools,
  `/usr/bin/git` and its neighbours are stubs that answer by opening an install
  dialog, and they are never asked.
- **`cwdDigest`** is over the workspace's **listing** — names and kinds, never
  contents. `node_modules/`, `.git/` and the other dependency directories are
  listed but not entered; the session's scaffolding is left out, so the listing
  describes the workspace as you left it.
- **`variables`** are `PATH`, `SHELL`, `LANG`, `LC_ALL` and `TZ` (plus
  `CLAUDE_CONFIG_DIR` and `ANTHROPIC_MODEL` for Claude Code), as set-or-not and
  a **digest** of the value — never the value, which can carry a home directory
  or a token.
- **`inputs`** are the Library artifacts handed to the session, as they landed.
  The files the session *read* are on its actions, digested when they were read.

---

## The three kinds

- **`run`** — headless and autonomous. This is the only kind anything
  initiates today.
- **`open`** — launches the app for the human in a terminal window, with the
  workspace and prompt loaded. On macOS it resolves a terminal application
  (iTerm, Ghostty, Alacritty, kitty, WezTerm, Terminal); on Linux it uses
  `$TERMINAL` or the first of `x-terminal-emulator`, `gnome-terminal`,
  `konsole`, `xfce4-terminal`, `alacritty`, `kitty`, `wezterm`, `foot`,
  `xterm`. Anywhere else it **fails immediately with a reason** rather than
  doing nothing.
- **`attach`** — resumes the app's own session named by `app_session_ref` and
  sends it a turn. It needs a prompt: neither app offers a way to *watch* a run
  somebody else started, so an attach with nothing to say would spend the
  machine owner's subscription asking the app nothing. It is refused by name
  instead.

An `open` that cannot launch ends the session **at once** with a non-empty
error. It never falls back to a headless run: the user asked to drive it
themselves.

---

## Outputs

Files the run created or changed in the workspace are pushed to the Library,
plus the full transcript. Bounded, and the bounds are announced rather than
applied silently:

- 64 MiB per source file and per output artifact, 128 MiB total, and at most
  32 output uploads per session. More than 32 files use bounded ZIP parts
  (stored without compression); archive overhead counts toward both byte
  limits. At most 4096 source entries bound the manifest and ZIP metadata.
  Candidates omitted by limits are named in an `outputs.json` artifact;
- a repository acquired during the session is an input. Unchanged tracked
  files do not enter the upload budget. Changed files and explicitly recorded
  writes are retained, including recorded writes committed before the session
  ends. Git worktree pointer files are scaffolding, not output;
- `.git`, `node_modules`, `vendor`, `target`, `dist`, `build`, `.venv`,
  `__pycache__` and friends are never pushed — reproducible, enormous, and
  they would bury the actual output;
- a file holding this session's credential is never pushed — the app copied
  the bearer out of its configuration, and an artifact would keep a
  credential nothing can revoke. It is named in a chunk like any other file
  left behind;
- nested paths are flattened (`api/schema.json` → `api__schema.json`) because
  the Library keys on one path segment, and two `schema.json` files from
  different directories would otherwise arrive indistinguishable.

The output manifest links retained files to their workspace-relative paths,
sizes, SHA-256 digests and Library artifact IDs, and links the transcript.
Manifest version 2 adds `archiveEntry` when a file lives inside a ZIP, plus an
`archives` list with each uploaded ZIP’s ID, byte length and SHA-256. Download
the artifact, verify its digest, then read the named member and verify that
member’s length and digest. Small sets still have individual artifacts.
Each file is checked through one regular-file descriptor inside the workspace;
its exact retained bytes are scanned for session credentials before bundling
and hashed in the same read. Missing, changed or unreadable files are explicit
gaps. Only successfully uploaded members enter `files`; failed parts name all
their members in `omitted`. The manifest records omissions and transfer failures. An incomplete export reports an error and preserves the
workspace for recovery; a transcript whose upload failed is also preserved.
This is separate from the action journal, which retains file revisions as the
app records them. Clean checkouts and dependency caches remain reusable local
resources rather than Library copies of a repository. Existing `.tar` and
`.zip` outputs remain eligible under the same rules. A later session takes a
new input snapshot, so unchanged files omitted by an earlier export must be
explicitly recovered as newly produced evidence; restarting alone does not
retroactively retain them.

Uploads declare a type from the produced bytes instead of multipart's default
`application/octet-stream`. Recognized binary signatures retain their type.
Text candidates must be valid UTF-8 across the whole body and contain no binary
control bytes (tab, newline and carriage return are allowed); checking only the
first 512 bytes would miss a binary suffix. Valid `.json` files declare
`application/json`, Markdown declares `text/markdown`, and other text uses the
sniffed type. An empty file stays opaque. A misleading extension cannot promote
binary content into text. This classification preserves the exact uploaded bytes
and digest and changes neither output limits nor credential/ownership/provenance
checks. MIME is a format hint, not trusted content or a schema verdict.

The Library deliberately honors a declared type. Its saved-file reader still
checks the current owner and revision, its text-type gate, UTF-8 and the 2 MiB
read bound. Previously retained opaque files are not relabeled by this change;
they need a normal authorized upload/version or a format-specific reader.
Readable does not imply indexed: semantic extraction and archive contents have
their own coverage limits. `TestLibraryPushContentTypePreservesBytes` exercises
the actual multipart upload for both file and in-memory outputs, including
misleading names and suffix corruption. The engine's
`UploadedJSONReadAcrossEngines` database case connects multipart ingestion to a
fresh replica's saved-file reader and verifies that binary and owner gates hold.

---

## When it does not work

An approved `workerHost.exec` script may start with a no-argument command
followed by a newline. The local shell-policy name classifier recognizes that
newline without joining the next line to the command name. It does not rewrite
the reviewed script: quoted multiline text, the exact-command receipt, local
consent, working-directory checks, resource limits, and allow/deny lists retain
their existing behavior. A scoped approval does not override the local policy.

| What you see | What it means |
|---|---|
| `/machines` shows the app but not selectable | one of `allowed` / `signed in` is false; the badge says which |
| The machine never appears at all | `claude` / `codex` is in none of the directories the worker searches. A service does not get your shell's `PATH`: the worker searches its own, with `~/.local/bin`, `~/.claude/local`, `/opt/homebrew/bin` and `/usr/local/bin` appended at start, and logs the result once (`worker PATH extended`). An app installed anywhere else needs that directory in the service's own `PATH` |
| `is not allowed for this cluster by this machine's policy.yaml (apps.homes)` | the engine routed here anyway. `memql worker apps --allow <app> --home <this cluster>`, or ask why the label was derived |
| `apps.allow is no longer read` (worker log, `memql worker apps`) | a `policy.yaml` from before consent was per cluster. Allow the app for the cluster you meant with `--home`; that removes the old list |
| `apps.homes.<cluster>…` (worker log, `memql worker apps`) | an entry the worker cannot read allows nothing. The sentence names the line; `{allow: [claude-code]}` is the shape |
| `is allowed here but is not on this worker's PATH` | the binary moved, or it lives outside the directories above. A service inherits neither your shell profile nor a version manager's shims |
| `workspace refused by this machine's policy` naming a directory under `workspaces/<home>/` | the engine named no workspace and the one this machine chose is outside `fs.workspace_root` or under `fs.deny`. Set `fs.workspace_root` (the choice then goes under it) or fix the deny entry |
| `no workspace in AppSessionStart, and this machine has no home directory` | there is nowhere to choose one. Set `fs.workspace_root` in `policy.yaml` |
| `reported MemQL's MCP server "memql" as failed` | Claude Code could not connect to the session's MCP endpoint: unreachable, or a certificate it does not trust. The mkcert CA must be in the system keychain |
| `reported MemQL's MCP server "memql" as needs-auth` | the endpoint refused the session's bearer |
| a turn fails at start saying the sandbox is unavailable | Claude Code cannot sandbox shell commands on this machine (on Linux, install bubblewrap and socat). By design the session does not run them unconfined |
| sessions fail to authenticate although `claude` works in your shell | Claude Code's auth is configured through `~/.claude/settings.json` (`env` such as `ANTHROPIC_BASE_URL` / `CLAUDE_CODE_USE_BEDROCK` / `CLAUDE_CODE_USE_VERTEX` / proxy variables, or `apiKeyHelper`). A session loads no settings file, so none of that reaches it. Log in with OAuth / the keychain (`claude auth`), set the variables in the worker's own environment, or put them in managed settings |
| `workspace ... overlaps ...` | the workspace is, contains or lies inside the worker's own files (`~/.memql`, its state, a `--token-file`, the consent socket) or an `fs.deny` entry. Name a directory beside them — a project directory, not the home directory |
| `fatal: unable to access '~/.config/git/config': Operation not permitted` in a transcript (macOS) | the default `fs.deny` lists `~/.config`, which a session's shell commands may not read, and git gives up on an XDG config it cannot read. Move the file to `~/.gitconfig` |
| a sandboxed command on Linux cannot look up a user name (`whoami`, `ssh`) | the default `fs.deny` lists `/etc/passwd`, which the Linux sandbox then replaces with an empty file for the session's shell commands |
| `Claude requested permissions to use …` in a transcript | a tool the session grant does not cover — outside the workspace, or not a file tool, shell command or MemQL tool. Correct refusal |
| `kind=attach ... needs a prompt` | an attach that only wanted to watch. Send the turn you want run, or use `run` |
| `this session is no longer taking turns` | a `message` control arrived after the last turn had already ended the session |
| `a follow-up arrived with no prompt` | a `message` control with an empty `prompt`. The follow-up's text travels in `prompt`, never in `reason` |
| `cannot run at level "embeddings"` | correct refusal: no app has an embedder. The engine should not route an embedding to an app |
| `cannot run at level "…": unknown level` | the engine sent a word that is not one of `fast`, `strong`, `reasoning`, `embeddings` — an engine newer than this cockpit, or a bug on that side |
| `apps.levels.<app>.<level>: … refuses <level> sessions` | your own `policy.yaml` entry for that level is one the app would misread. Fix the line (`memql worker apps` shows it) and `SIGHUP` the worker |
| the model on a finished session is not the one the level names | usually just the name: the level passes an alias (`opus`) and the End carries the full id the app resolved it to (`claude-opus-5`). Otherwise the app ran something else and said so — Codex rerouted the turn (`model/rerouted` is in the transcript), or a Claude Code turn spent nothing on the session's own model. The `[memql] level …` transcript line says what was asked for |
| `codex mcp-server cannot set a level on a thread it resumes` | an attach at a level on a Codex without the app-server. Attach without a level, or upgrade Codex |
| `apps.levels.<app>.<level>: an entry is a mapping of model and effort` | a shorthand like `reasoning: opus`. Write `reasoning: {model: opus}` |
| `the app answered, but not against the schema` | the run SUCCEEDED and produced no structured result. For `codex-mcp` that is expected: it cannot constrain the answer at all |
| `not found (404) -- either the artifact does not exist, or the owning user cannot reach it` | the Library answers 404 for both on purpose, so a link cannot probe which ids exist. Check the OWNING USER's access, not the worker token |
| `the session credential was rejected (401)` | this one IS the cockpit's side: the bearer expired or is malformed |
| `no display: DISPLAY and WAYLAND_DISPLAY are both unset` | an `open` on a headless box. Correct refusal |
| `live transcript truncated at limits.max_transcript_bytes` | the engine's row cap bit. The complete transcript is the pushed artifact |
| an action's file carries `omitted: outside_workspace` or `session_scaffolding` | the app named a file the recording will not read: outside the session's workspace, or the session's own files. Correct refusal; the path is still recorded |
| an action carries `incomplete: true` | the app started the call and the turn ended before it finished — a cancel, a crash, or a result line over 1 MiB the harness could not parse |
| a file the app read carries `omitted: digest_only` and no bytes | the app's own result did not carry the whole file — it read part of it, or the file changed after it was read. By design: the recording sends no more of a file than the app did. A file written inside `.git/` or `node_modules/` gets the same |
| a file carries `omitted: contains_credential` | the file holds this session's bearer — the app copied it out of its MCP configuration. The recording carries neither its bytes nor its digest, and the output push skips it with `not pushed to the Library: … (it holds this session's credential)` |
| an action carries `argsOmitted: too_large` | its arguments would have made it larger than the 8 MiB an action may be — usually a Codex patch deleting or rewriting a large file. `argsDigest` identifies them |
| the action `seq` skips a number | an action could not be sent: too large even without its arguments, or the stream failed — the worker log names the call — or the app reported it finishing after its turn had already ended, when nothing was listening for it |

## Related

- [computer-use.md](computer-use.md) — the other surface that touches the
  machine directly
- The engine's
  [workers runbook](https://github.com/znasllc-io/memql/blob/main/docs/public/operate/workers-runbook.md)
