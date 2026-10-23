# Computer Use (the `computeruse` build variant)

This document is the contract reference for the cockpit's computer-use
surface (`workerComputer.*`): the dispatch architecture, the action
vocabulary, the coordinate model, the capability descriptor, platform
support, permission setup, and troubleshooting. It documents what
shipped in the computer-use v2 epic (memql#161).

The surface lives in the **computer-use build variant** — the
`memql` binary, built with `-tags computeruse`
(CGO + RobotGo). Build it with `make cockpit-computeruse`, or install it
on a worker host with the OS installers' `--computeruse` flag
(`scripts/install/install-mac.sh --computeruse` /
`scripts/install/install-linux.sh --computeruse`). The default headless
`memql` binary exposes only `capabilities` + `wait`; every other
action returns `gui_unavailable`.

> **Migration note — the computer-use binary was renamed.** The build
> variant was renamed `gui` → `computeruse`: the binary
> `memql-gui` → **`memql`**, the build tag
> `-tags gui` → `-tags computeruse`, and the advertised capability
> `GUI` → `COMPUTERUSE`. macOS TCC grants are keyed to the binary's
> identity, so after upgrading you **must re-grant Accessibility and
> Screen Recording to `memql`** under System
> Settings → Privacy & Security, then **re-run
> `memql worker setup`** to reprobe. The old
> `memql-gui` entries in those TCC panes are stale and can be
> removed. (The `gui_unavailable` error code is intentionally
> **unchanged** — it remains a stable wire value.)

Source-of-truth code paths (this repo unless noted):

- Dispatcher + consent + preflight: `internal/worker/tools/dispatcher.go`, `preflight.go`, `internal/worker/consent/consent.go`
- Action handlers: `internal/worker/tools/computer_computeruse.go`, `window_computeruse_{darwin,linux,other}.go`, `wait.go`, `capabilities.go`
- Coordinate model: `internal/worker/tools/coords.go` (the package comment is the canonical contract)
- Server-side scope policy: `integrations/agent/worker/scope.go` (memql repo)
- Descriptor validation: `component/worker/capability_descriptor.go` (memql repo)

## Architecture

A computer-use call travels from the agent runtime to the user's
machine through consent, platform preflight and the target guard. Each gate fails with a structured error
code rather than letting the failure surface downstream as an opaque
RobotGo error.

```
  agent runtime (memql node)
        |
        v
  memql dispatch gate                 integrations/agent/worker/dispatch.go
    - action -> required capability + scope (scope.go);
      unknown actions denied with unknown_action
    - per-task approval: dispatch must carry a PlanId from an
      approved scope-elevation Plan (requestComputerUseScope)
    - user kill switch (ComputerUseEnabled preference)
    - agent standing scope (observe / full) must satisfy the action
        |
        v
  WorkerService gRPC stream           component/grpc/worker.proto (memql)
    bidirectional stream; ToolDispatch down, ToolResult up
        |
        v
  cockpit dispatcher                  worker/tools/dispatcher.go
    - panic recovery (cgo panics become tool_panic failures)
    - consent gate (per-host operator window; consent_required
      when no window is open -- applies to EVERY action,
      including capabilities and wait)
        |
        +--> capabilities / wait      build-agnostic; routed BEFORE
        |                             preflights and the per-build router
        v
  preflight chokepoint                worker/tools/preflight.go
    - macOS: Accessibility (input actions), Screen Recording
      (screenshot) -- per-call TCC reads
    - Linux: display-server gate (input + screenshot require X11)
        |
        v
  per-build action router
    - computeruse build: computerActionHandlers map (computer_computeruse.go),
                         target guard for input; read-only capture/enumeration handlers
    - headless:          gui_unavailable for everything except the
                         build-agnostic capabilities + wait
```

### Build variants

| Build | Tag | workerComputer surface |
|---|---|---|
| `memql` (headless, default) | none | `capabilities` + `wait` only; every other action returns `gui_unavailable`. Registers capability `HEADLESS`. |
| `memql` | `computeruse` (CGO + RobotGo) | 16-action dispatch vocabulary; supported input transactions are listed below. Registers `HEADLESS` + `COMPUTERUSE`. |

The computer-use action table in `computer_computeruse.go` retains the legacy
dispatch vocabulary. The descriptor filters actions which now always refuse,
including input on platforms without a targeted backend. Input and focus calls
pass through the target guard before any legacy helper. `inputTargetPolicy`
describes supported transactions and required arguments.

### Scoped host permissions from Nexus

Both builds on macOS and Linux advertise `workerHost.computer_permission: 1`;
other platforms omit it. This separate
contract supports a person's **Once** or **Always** choice in Nexus for
read access to one directory or for one exact shell command. It requires
Cockpit 0.18.0 and MemQL 0.27.0 or later; an older Cockpit cannot execute these
requests by silently accepting a broader permission.

The engine binds each receipt to the person, agent, enrollment and machine.
Once permits one exact dispatch and is durably consumed by the replica
holding that machine's stream. Always permits matching calls until the
person revokes it under **Nexus → Approvals → Saved permissions**. A saved
permission never opens Cockpit's broader computer-control window or
overrides a local deny rule. From Cockpit 0.18.3, a directory approval is
sufficient for its bounded read: it does not require a second, temporary
window. Always remains effective across window expiry, worker restarts and
new requests until revoked. The engine checks the saved permission afresh
before each dispatch; Cockpit does not cache it. Exact shell commands still
require the local control window.

`memql worker consent revoke` also pauses approved directory reads on every
connected cluster (`--cluster <id>` limits the stop to one cluster). This
local stop survives Cockpit restarts. A local `consent grant --window=1h`
resumes approved directory reads and opens the broader window for one hour;
the directory approval remains independent of that window's expiry.
`consent status` shows both states. Pausing or removing the cluster
connection also prevents dispatch. Damaged access-state files fail closed;
storage failures are reported instead of claiming the stop was saved.

Directory permissions cover only `fs_read`, `fs_list` and `fs_stat` under
the reviewed absolute directory. Cockpit opens path components without
following symlinks and reads through the held descriptor, including list
metadata. A link or alias is refused with an instruction to request the
real directory. Local workspace and filesystem deny rules still apply.

`fs_read` retains `{path, maxBytes?}` prefix reads and advertises action contract
`workerHost.fs_read: 2` for bounded inspection of one specified file:

- `offsetBytes` with `maxBytes` reads a byte range.
- `startLine` with `lineCount` reads a one-based line range.
- `search` finds a nonempty literal, with optional `maxMatches` (default 20,
  maximum 100). `content` contains whole matching lines; `matches` identifies
  each source line/byte offset and its byte span within `content`.

These three modes cannot be combined. All reads bound `maxBytes` to 1–5,242,880
(default 5 MiB). Line ranges and search also accept `maxScanBytes`, default
5 MiB and maximum 16 MiB; skipped lines count toward that scan bound. A partial
line at the scan boundary is withheld. An empty incomplete search does not
establish absence. No recursive filesystem search or command permission is
introduced.

Results include `path`, `content`, `size` (returned source bytes), `fileSize`,
`offsetBytes`, `nextOffsetBytes`, `scannedBytes`, `modTime`, and the SHA-256 of
returned source bytes. For prefix/offset reads, `complete` means reaching EOF;
for line ranges, it means covering the requested lines or reaching EOF; for
search, it means reaching EOF without withholding matches. It never establishes
coverage before an offset or before the requested lines. `truncated` is the
inverse of `complete`. `changedDuringRead` checks size and mtime
on the same opened descriptor before and after the read; a detected change
makes coverage incomplete. This is not a concurrent snapshot guarantee.

Engine status exposes `boundedFileReadSupported`; routing and the receiving
replica check the advertised contract before dispatch, so an old worker cannot
silently substitute a prefix read. This capability check grants no access.
Saved directory and exact-once permissions still apply to these read arguments,
and Cockpit's local filesystem rules and symlink checks remain authoritative.

Command permissions accept only `cmd`, an explicit canonical absolute
`cwd`, and optional integer `timeoutSec` from 1 to 600 (default 60).
Nexus shows all three resolved values. Environment overrides, stdin and
unknown command options are refused instead of stored as hidden approval
inputs. The original argument hash binds the invocation, including whether
the timeout was supplied; changed arguments require a new approval. Local
shell policy still decides whether the approved command may execute.
Cockpit preserves the exact command and directory bytes. It opens the working
directory without following links and enters that held directory in the child
process, so replacing the pathname cannot redirect the approved command. The
existing process-group cancellation and child resource limits still apply.

## Action vocabulary

16 actions. "Consent class" is the cockpit consent gate's
classification (`consent.Classify`); "Scope" is the memql-side
requirement the agent's authorization must satisfy
(`workerComputerScope` in scope.go); "Headless" marks availability on
the headless (non-`computeruse`) build.

| Action | Consent class | Scope (memql) | Headless |
|---|---|---|---|
| `capabilities` | observe | observe (1) | yes |
| `wait` | observe | observe (1) | yes |
| `screenshot` | observe | observe | no |
| `cursor_position` | observe | observe | no |
| `display_info` | observe | observe | no |
| `window_list` | observe | observe | no |
| `mouse_move` | interact | full | no |
| `mouse_click` | interact (2) | full | no |
| `mouse_down` | interact (2) | full (1) | no |
| `mouse_up` | interact (2) | full (1) | no |
| `mouse_drag` | interact | full | no |
| `mouse_scroll` | interact | full | no |
| `key_type` | interact (2) | full | no |
| `key_combo` | interact | full | no |
| `key_hold` | interact (2) | full (1) | no |
| `window_focus` | interact | full | no |

(1) The memql-side scope table is deny-by-default: an action absent
from `workerComputerScope` is rejected at the memql dispatch gate with
`unknown_action`. The v2 additions (`capabilities`, `wait`,
`key_hold`, `mouse_down`, `mouse_up`) are classified per memql#1333
(merged): `capabilities` + `wait` admit at observe against the
`HEADLESS` capability (so a headless-only worker can serve them); the
three input primitives require full against `COMPUTERUSE`.

(2) Strict-mode high-risk subset. Under a `--strict` consent grant
these five actions (`key_type`, `key_hold`, `mouse_click`,
`mouse_down`, `mouse_up`) additionally block on a per-call operator
Allow/Deny approval (30s timeout, default deny). `mouse_down`/
`mouse_up`/`key_hold` are in the subset because they compose into
clicks and chords -- leaving them out would let strict mode be
bypassed by decomposition. The legacy cursor-region exemption is disabled for target-addressed clicks:
that cursor is unrelated to the explicit screenshot point. Strict grants
still require per-call approval for these actions.

Note: ordinary calls in both consent classes require an open consent window -- observe
vs interact drives the audit stream and strict-mode behavior, not the
basic gate decision. Unknown (tool, action) pairs classify as
`unknown` and are treated like interact (deny-by-default).
Scoped directory reads use the independent approval described above.

### Per-action arguments and results

All results are JSON objects in `Success.result_json`. Failures carry
a structured `error_code` + message (see Troubleshooting).

### Target-bound input contract (version 1)

The computer-use build advertises `workerComputer.input_target: 1`.
Input requires existing consent and a run ID, plus a one-action receipt:

1. Enumerate windows with `window_list`. The intended window must be focused
   before screenshot binding. On macOS, an unbound worker may perform one
   guarded initial selection using the window-selection contract below.
2. Call `screenshot` with that explicit `windowId`. The macOS backend checks
   the actual Accessibility focused application, window and element before
   and after capture. It matches the AX window to exactly one on-screen CG
   window with the same PID and bounds; unavailable or ambiguous focus refuses.
3. Pass the returned `inputTarget.id` as `inputTarget` on one input action.
   It expires after 60 seconds and belongs to that dispatcher/enrollment and
   run. An attempted transaction consumes it even on argument failure.
4. Inspect a fresh screenshot before another action, including Enter after
   typing. Every screenshot invalidates the preceding receipt.

A screenshot without `windowId` remains read-only and grants no input target.
The independent focus anchor survives read-only captures, new tokens and new
runs. A detected app/window or geometry mismatch stops all desktop input
until operator review and worker restart. A new screenshot cannot clear the
interruption. Input is serialized across enrollments in one worker process.
There is no automatic retry, focus stealing, or global-event fallback.
Unobserved field changes also stop input, except for the bounded observation
after a completed gesture described below.

An operator can inspect the running worker's exact-target acquisition with
`memql worker control --action=input-target-status`. This explicit local control
action uses the worker's own macOS permission identity and reports the failing
AX stage/error or observed PID/window. It does not read element contents, mint
a receipt, reset the interruption latch, activate a window, or send input.
Ordinary `--action=status` does not acquire an AX target. Diagnostic success
does not establish event delivery or authorize resuming an interrupted run.
macOS diagnostics also compare a public NSWorkspace application snapshot with
direct application AX reads and report elapsed time. That snapshot can be cached
between main-run-loop turns; it is diagnostic only and never substitutes for
the guard's focused-application read.
`first_interruption` preserves the first stop's UTC time, action kind, phase and
reason. AX failures additionally retain only the stage, numeric error and match
count. It contains no keys, text, titles, field contents or receipt tokens, and
later observations cannot overwrite it. `pending_observation` is not permission.

The macOS computer-use `worker run` initializes AppKit on the original process
thread with activation prohibited, then pumps its main run loop while the worker
runs on a Go goroutine. It creates no windows and does not activate applications.
Ordinary CLI commands and scoped-command children do not start this loop. The
diagnostic reports loop progress; the AX target guard remains authoritative.
If GUI initialization fails (for example, without a usable GUI session), host
and inference services still start. Desktop operations are disabled for that
process and its capabilities advertise only the platform-independent `wait`.

macOS delivery uses `CGEventPostToPid` for keyboard and mouse. Move/click
packets are constructed with AppKit's public explicit-window factory, then
converted to Quartz while preserving global logical coordinates, a private
event source and explicit modifiers. Packet tests decode the result back to
`NSEvent.windowNumber`: Quartz's two public mouse-window hints alone decode
as window zero. Those hints are also retained. Quartz ignores those fields for
scroll packets, which retain PID and
point addressing with the same sampled-focus checks. Window hints do not
acknowledge application delivery or atomically bind an AX field. Before each
press/text event the guard checks the AX window and field; releases check the
window and always address the original PID. Cleanup releases only an already
attempted down event to that same PID. macOS also checks content-free HID event
counters, held hardware input and a main-loop activation generation around
capture and before each event. The worker's events use a private source.

A completed click or key gesture can replace its field after the final release.
Its input receipt is already spent. For 60 seconds after completion, a new
screenshot in the same run may observe that change only if the exact retained
AX app/window, PID, CG window and geometry still match, no hardware input or
activation occurred, and focus stays stable across capture. The deadline is
checked after capture too. This observation does not repeat input, activate an
app or clear a latch. A fresh single-use receipt is required before more input.
Text, move and scroll actions keep the strict field anchor; unsupported activity
monitors retain the older strict behavior. Unavailable macOS monitoring, focus
query errors, unstable capture, identity changes and human activity fail closed.

For process-addressed move/click delivery, public `CGEventSetLocation` invalidates
the packet's window-local point. The Darwin backend therefore dynamically resolves
the private macOS `CGEventSetWindowLocation` / `CGEventGetWindowLocation` accessors
to preserve both points. This is an explicit OS compatibility dependency: missing
accessors, invalid window bounds, or a mismatched window/global/local round-trip
refuse packet creation before posting. There is no global posting fallback.
Local `pointer_coordinate_accessors` and capability `pointerCoordinateAccessors`
report symbol availability only, not delivery success. Tests include a foreign
producer and hidden receiving windows with title bars and nonzero origins; these
construct/deserialize packets without posting events or activating windows.

The passive `CGPreflightPostEventAccess` check is separate from AX inspection
permission. The actual worker reports it in local `input-target-status` as
`post_event_access` and capabilities as `inputTargetPolicy.postEventAccess`.
A false result refuses the transaction before input and is checked again before
each post. The probe never requests permission or displays a prompt; absent/null
means this backend cannot report it, not permission granted.

**Limits:** process-addressed events have no delivery acknowledgement and
are not atomically bound to an AX window or field. Checks sample focus; they
cannot atomically attribute an application's field replacement or prevent every
same-process race. HID counters and delivered activation notifications add
interruption evidence; they are not an atomic OS input lock. Focus-query errors
still stop input, including transient errors during navigation. The earlier live
stops did not retain their initial reason, so their exact cause remains unknown.
Pointer events do not warp the global cursor; applications may
ignore synthetic hover/scroll delivery. This is not a claim of uninterrupted
multi-app support or that automation can never compete with a user. Normal
live interactions still require controlled validation before sustained use.
Guard tests use fake event backends. Platform packet tests construct and inspect
Quartz events without posting them; neither establishes application delivery.

Linux and other platforms have no verified target-addressed input backend in
this version and refuse input. Read-only capabilities remain as documented.
`key_hold`, `mouse_down`, `mouse_up` and `mouse_drag` refuse.
Legacy global input and app-only activation code is unreachable
through the dispatcher. No consent or server scope requirement is relaxed.

**`screenshot`** -- `format` (`png` default or `jpeg`), `display` (default
0), `region: {x,y,w,h}` in display-local logical points, `quality` (JPEG,
default 80), `maxLongEdge` (default 1568, clamped to 512–8000), and optional
`windowId` to acquire an input receipt. Result includes `format`, `display`,
`width/height` (emitted pixels), `sourceWidth/sourceHeight` (captured pixels),
`logicalWidth/logicalHeight`, `logicalOriginX/logicalOriginY`, `scale`,
`bytesBase64`, `sizeBytes`, optional `region`, and (when requested)
`inputTarget: {id,windowId,pid,expiresAt,singleUse:true}`. `windowId` validates
focus; it does not crop the capture to that window.

**`cursor_position`** -- read-only; returns global LOGICAL `{x,y}`.
These are not coordinates for the target-addressed pointer API.

Supported inputs require `inputTarget`. Success reports `targetWindowId`,
`targetPid`, `eventsSent`, `text_redacted:true`, `requiresFreshScreenshot:true`.
It means events were posted, not that the app processed them.

- **`mouse_move`**: explicit integer `x,y` from the receipt's screenshot.
- **`mouse_click`**: explicit `x,y`, optional `button` (`left`, `right`,
  `middle`) and `modifiers` (`cmd`, `ctrl`, `alt`, `shift`; aliases accepted).
  One down/up pair; multiple clicks, global cursor clicks and legacy options
  are refused rather than silently substituted.
- **`mouse_scroll`**: explicit `x,y`, `dx`/`dy` line deltas in [-1000,1000],
  at least one nonzero. Position comes from the bound image.
- **`key_type`**: 1–4096 bytes of valid printable UTF-8. Control characters,
  including newline/tab, are refused; use a separate reviewed `key_combo`.
  Unicode keydown/up events preserve non-ASCII text. Results redact the text.
- **`key_combo`**: `keys` string array, primary key first, then modifiers
  (for example `["a","cmd"]`). Supports lowercase ANSI character key
  positions, arrows, Enter, Escape, Tab, space, backspace/delete, navigation
  keys and F1–F12. Shortcut character positions assume an ANSI layout;
  `key_type` uses Unicode independently of layout. Unsupported keys refuse.

**`display_info`** -- no args. Result `{width, height, displays:
[{id, x, y, width, height, scale, primary}], primary}`. Top-level
`width`/`height` are the primary display's logical size (back-compat);
`displays` rects are in virtual-desktop logical space (origins can be
negative for displays left of / above the primary). `scale` is the
capture pixel density: the primary display's native scale (2.0 on
Retina macOS), 1.0 for secondary displays (see platform matrix).

**`window_list`** -- normally no args. Result `{windows: [{id, title, app,
pid, x, y, width, height, focused}]}`. Coordinates are LOGICAL
(RobotGo input space), not emitted-screenshot space -- to click
inside a window, still map through a screenshot. On macOS, `title`
for other apps' windows requires the Screen Recording permission
(empty without it; `app` is always available). `id` is the platform
window handle (CGWindowID / X window id), stable for the window's
lifetime only.

**Initial window selection (macOS, `workerComputer.window_selection: 1`)** --
Call `window_list` with an explicit `windowId` and `pid` from an observed row.
The worker re-enumerates the window, resolves exactly one matching AX window,
and retains its AX and application-instance identities, geometry, foreground
application identity, activation generation and content-free HID event counters.
Ambiguity or held input refuses. The result includes `focusTarget.id`, bound to
this dispatcher/enrollment and run, single-use and valid for 60 seconds. A new
window list replaces an earlier selection receipt.

**`window_focus`** -- requires that `focusTarget`, `windowId` and `pid`.
Only an unbound, uninterrupted worker may use it. It invalidates any input
receipt, checks for intervening activity or changed identities, and uses AX
main/raise plus AppKit activation on the original main thread. It never posts
keyboard/mouse events or falls back to app-only/global input. The exact AX
focused application/window/element must be verified afterward; activation alone
does not produce an input receipt. Take a fresh screenshot before input.

An existing anchor can never be replaced by selection. Any detected interruption
or unverified activation latches input off; a new token/run cannot recover it.
Cancellation before main-thread execution skips the operation. Once execution
starts, the call waits for completion, preventing a queued activation after the
call returned. Checks remain sampled: macOS does not offer an atomic transaction
covering user activity, activation and AX focus. HID counters do not include
every special keyboard event. Live application delivery must still be proved.

**`capabilities`** -- no args. Returns the capability descriptor
(below). Works on every build and never runs preflights:
introspection must work even (especially) when everything else is
broken.

**`wait`** -- `ms` (clamped to [1, 5000]). Bounded, context-cancelled
sleep; the build-agnostic pacing primitive. Longer pauses are issued
as multiple waits so each stays cancellable and audit-visible. Result
`{ms}`.

## Coordinate model

Canonical contract: the package comment in
`internal/worker/tools/coords.go`.

Model/tool coordinates are ALWAYS in the coordinate space of the most
recently emitted screenshot image: `(0,0)..(emittedWidth-1,
emittedHeight-1)`. Three spaces exist on a HiDPI host and they
differ:

- **logical (input) space** -- the points RobotGo moves/clicks in;
  `robotgo.GetScreenSize()` reports this size.
- **captured (physical) space** -- the raw capture's pixel
  dimensions; 2x the logical size on a 2x Retina display.
- **emitted space** -- the captured image after the downscale policy
  (long edge clamped to 1568 px by default). This is what the model
  actually sees and therefore the space it speaks.

The guard retains the exact image-to-logical mapping with its receipt,
including `maxLongEdge`, display and region origin. Rounding is half away
from zero, with clamping inside the logical capture rectangle. The input
point must lie inside both the emitted image and the bound window.

Screenshots are downscaled with CatmullRom and never upscaled. The default
long edge is 1568 pixels. An override is valid for input because the mapping
comes from that actual capture rather than a reconstructed default.

For multiple displays, select the display during screenshot acquisition.
The mapper adds its global origin (negative for displays left of or above
the primary display). Do not send another `display` on the input action.

## Capability descriptor

The worker's structured self-description of its computer-use surface
(memql#162). Shape (`schemaVersion` 1):

```json
{
  "platform": "darwin",
  "displayServer": "quartz",
  "computerUseAvailable": true,
  "actions": ["display_info", "key_combo", "...", "wait"],
  "displays": 2,
  "schemaVersion": 1
}
```

- `inputTargetPolicy`: supported input actions, receipt acquisition and lifetime,
  exact-image coordinates, guarded initial selection, interruption handling and
  sampled-focus limitations. Included in live capabilities and registration
  JSON; older servers may discard this additive field. The action contract
  version remains available in `actionContracts`.
- `platform`: `runtime.GOOS`.
- `displayServer`: `quartz` (macOS) | `x11` | `wayland` | `none`,
  detected at call time. Headless builds always report `none`.
- `computerUseAvailable`: whether the binary was built with the
  `computeruse` tag (the Go field is `ComputerUseAvailable`). Consumers
  that want "can I actually screenshot right now" must check
  `displayServer` too (a `computeruse` build on a Wayland session
  reports `computerUseAvailable: true, displayServer: "wayland"`).
- `actions`: supported actions, filtering always-refused legacy inputs and
  platforms without a targeted backend, plus build-agnostic `wait`, sorted. A headless build advertises
  `["wait"]`, not `[]`. The meta `capabilities` action is
  deliberately NOT listed (it is the introspection channel itself and
  is unconditionally available).
- `displays`: attached display count at call time (memql#165);
  0 on headless builds and when no RobotGo-drivable display server is
  reachable, at least 1 otherwise. Additive field -- schemaVersion
  stays 1; the memql server tolerates unknown JSON fields.

Where it surfaces:

1. **`workerComputer.capabilities`** -- the action result, on every
   build.
2. **`Register.capability_descriptor_json`** -- the same JSON
   (identical source: `ComputeCapabilities`), sent in the
   registration handshake so the server knows the action surface up
   front without a live round-trip. The server validates it
   (memql#1331, `ParseCapabilityDescriptor`): raw JSON at most 4096
   bytes, `schemaVersion` must be exactly 1, `displayServer` must be
   one of the four enum values, `platform` and every action name must
   match `^[A-Za-z0-9._-]{1,64}$`, at most 64 actions, no duplicates.
   An invalid descriptor REJECTS the registration; an absent one is
   fine (older builds register exactly as before).
3. **`v1:worker:registration.capabilityDescriptor`** -- persisted on
   the registration concept row (memql `dsl/worker/concepts.memql`),
   projected by the `workerRegistrationFull` shape.
4. **`agentworkerListWorkers`** -- the agent-side `listWorkers` tool
   includes a `capabilityDescriptor` entry per worker when one was
   sent at registration.

## Platform support matrix

| Action group | macOS (quartz) | Linux X11 | Linux Wayland |
|---|---|---|---|
| `capabilities`, `wait` | yes | yes | yes (no display needed) |
| `screenshot` | yes (Screen Recording TCC; per-call preflight) | yes | no -- `display_server_unsupported` |
| `cursor_position`, `display_info` | yes | yes | undefined (see note 3) |
| supported pointer transactions | target-addressed, Accessibility required | refused | refused |
| `key_type`, `key_combo` | target-addressed, Accessibility required | refused | refused |
| `window_list` | yes -- CGWindowList; titles need Screen Recording | yes -- EWMH `_NET_CLIENT_LIST` | no -- `unsupported_on_platform` |
| `window_focus` | guarded initial selection only | refused | refused |
| holds and drags | refused | refused | refused |
| multi-display | yes (note 2) | yes (Xinerama; single-display fallback when absent) | n/a |

Notes:

1. **Focus recovery is disabled.** Guarded initial selection is available on
   macOS only while unbound. It can never replace an anchor or clear a latch.
2. **Secondary-display capture caveat (macOS)** -- the primary
   display captures through RobotGo's native path at physical pixels
   (Retina quality). Secondary displays capture through the
   compositing path (`robotgo.Capture`) at LOGICAL resolution, so
   their capture scale is 1.0 (and `display_info` reports
   `scale: 1.0` for them). The coordinate mapper mirrors this
   (captured == logical on secondaries), so click targets stay
   correct -- only image sharpness differs. Retina-quality secondary
   capture needs a robotgo upgrade (v1.0.2 takes raw
   CGDirectDisplayIDs, not indexes) or direct cgo.
3. **Wayland is unsupported, by detection rather than by crash.**
   Detection (`detectDisplayServer`): `WAYLAND_DISPLAY` set or
   `XDG_SESSION_TYPE=wayland` means `wayland` -- this deliberately
   WINS over a set `DISPLAY`, so an XWayland session is still gated
   (decision recorded in #164/#168: RobotGo against XWayland is
   untested; if it proves workable this could soften to a warning).
   Input + screenshot fail the preflight with a structured
   `display_server_unsupported`; `window_list` does
   its own detection and fail with `unsupported_on_platform`;
   `capabilities` and `wait` work (and the descriptor advertises
   `displayServer: "wayland"`, `displays: 0` so consumers can refuse
   up front). `cursor_position`/`display_info` are NOT preflight-gated
   (they post no input and capture nothing); their RobotGo X11 calls
   have undefined results on Wayland -- a hard failure is caught by
   the dispatcher's panic recovery (`tool_panic`). Register Wayland
   hosts HEADLESS-only (`install-linux.sh` detects Wayland and does
   this automatically).

Windows and other platforms: no computer-use input backend; `window_list` returns `unsupported_on_platform`
(`window_computeruse_other.go`), the descriptor reports
`displayServer: "none"`.

## Permission setup

### macOS (TCC)

Two grants for the `memql` binary under System
Settings -> Privacy & Security:

- **Accessibility** -- gates synthetic mouse + keyboard events
  (everything `isComputerInputAction` matches). Checked per call via
  `AXIsProcessTrusted` (prompt-free, synchronous).
- **Screen Recording** -- gates `screenshot` (and window titles in
  `window_list`). Checked per call via
  `CGPreflightScreenCaptureAccess`.

Both preflights run on EVERY dispatch, so a grant revoked mid-session
surfaces as a clean `permission_denied` with a remediation hint on
the very next call instead of a raw RobotGo error or panic.

TCC grants are per signed-binary identity, and a command-line binary
launched from Terminal INHERITS Terminal's grants. That means a probe
can succeed from your shell while the same binary, launched detached
as a LaunchAgent at login, is denied -- the LaunchAgent needs the
`memql` binary's OWN entry in System Settings. Check the background process
with `memql worker control --action=status`. A Terminal setup result cannot
prove that the service has access. The [native permission window](macos-menu.md#guided-permission-setup-development-not-in-0140)
guides requests and verifies the running worker in development builds.

### Linux

- An **X11 (Xorg) session** is required at runtime: RobotGo drives
  X11 only (XTEST input, X11 capture). `DISPLAY` must be set and the
  X authority reachable. Wayland sessions are gated (see matrix
  note 3).
- Building the computer-use variant needs CGO plus the X11 dev packages
  (Debian/Ubuntu names): `libxtst-dev`, `libxinerama-dev`,
  `libxkbcommon-dev`, `libxkbcommon-x11-dev`, `libpng-dev`,
  `libx11-dev`, `libxi-dev` (the set CI installs in the
  `build-computeruse` lane). The matching runtime libraries must be
  present on the host.

### The `worker setup` wizard

`memql worker setup` is the permissions preflight: plain sequential
output (three steps on macOS: Accessibility -> Screen Recording ->
Validate), pausing for approval where a grant is missing. Pass
`--non-interactive` to report-and-fail instead of pausing -- for
scripted installs and CI.

The wizard probes the ACTUAL gated operations, using the same
detection the dispatcher enforces so its verdict cannot disagree with
runtime gating:

- macOS Accessibility: a real relative cursor move (CGEventPost) plus
  the `AXIsProcessTrusted` preflight -- both must pass.
- macOS Screen Recording: `CGPreflightScreenCaptureAccess`.
- Linux: display-server detection first (Wayland/none fails here with
  the same verdict the dispatcher would return), then an XTEST cursor
  probe and a small `SaveCapture` screenshot probe.

A denied step does not block: the worker can still register
HEADLESS-only, and computer-use calls fail with structured errors. The
headless binary's `worker setup` just prints guidance to install
`memql`.

## Consent gate (operator side)

Every `workerHost`/`workerComputer` dispatch passes a per-host consent
gate before anything runs (memql#64). Without an active
operator-granted window the call fails with `consent_required` -- this
includes `capabilities` and `wait`.

The worker exposes a local control socket (`~/.memql/worker.sock`,
mode 0600; override with `MEMQL_WORKER_CONSENT_SOCKET`):

```bash
memql worker consent grant --window=1h [--strict] [--cluster <id>]  # open a window
memql worker consent revoke [--cluster <id>]                        # close immediately
memql worker consent status [--cluster <id>]                        # show current state
memql worker consent watch [--cluster <id>]                         # live event tail
```

- **Consent is per cluster.** A machine paired with more than one cluster
  holds a separate window for each: a window opened while you work with one
  cluster admits nothing another cluster dispatches. `--cluster` takes the
  home id `memql worker config` shows. On a machine that serves one cluster
  it can be left off; on one that serves several, a grant without it is
  refused with the exact command for each cluster, and `status` shows every
  cluster in one table:

  ```
  Consent is per cluster. This machine serves 2:

    CLUSTER      CONSENT       EXPIRES    REMAINING   STRICT
    local        not granted
    production   GRANTED       14:40:00   40m         off
  ```

- **`revoke` without `--cluster` closes every window** — the kill switch
  never asks which. With `--cluster` it closes that one and says which
  others are still open.
- A second `grant` overwrites the active window (most recent decision
  wins). `revoke` also denies every pending strict-mode approval.
- **Strict mode** (`--strict`): flags the window for per-call
  Allow/Deny approval on the high-risk subset (`key_type`, `key_hold`,
  `mouse_click`, `mouse_down`, `mouse_up`). Interactive Allow/Deny
  enforcement previously lived in the Workers tab (memql#64,
  #130) -- an in-TUI approval modal + region picker -- but was removed
  with that tab in memql#216; worker consent is now managed via
  the `worker consent` subcommands. Other interact
  actions (`exec`, `fs_write`, `mouse_move`, `key_combo`, ...) stay
  admitted by the standing window.
- Close the active window immediately with `memql worker
  consent revoke` (this also cancels any pending approvals). The
  previous global `Ctrl+E` cockpit kill switch went away with the
  Workers tab (memql#216).

This gate is cockpit-side and operator-facing. It is independent of
the memql-side gates (per-task Plan approval, user kill-switch
preference, agent scope), which run before the call ever reaches the
worker.

## Troubleshooting

| Error code | Cause | Fix |
|---|---|---|
| `consent_required` | No active consent window on the worker host for the cluster that dispatched the call (or the window expired). | On the worker host: `memql worker consent grant --window=<duration>` -- with `--cluster <id>` when the machine serves more than one cluster; the error message names the exact command. |
| `permission_denied` | macOS TCC grant missing or revoked: Accessibility (input actions) or Screen Recording (screenshot). Checked per call, so mid-session revocation surfaces immediately. | System Settings -> Privacy & Security -> Accessibility / Screen Recording: enable `memql` (the binary itself, not just Terminal, for LaunchAgent use). Re-run `memql worker setup` to verify. |
| `display_server_unsupported` | Linux session is Wayland (`WAYLAND_DISPLAY`/`XDG_SESSION_TYPE` -- wins even when `DISPLAY` is set) or has no display server at all; RobotGo drives X11 only. | Log into an X11 (Xorg) session, or register the worker HEADLESS-only (`install-linux.sh` does this automatically on Wayland). |
| `out_of_bounds` | Coordinates outside the emitted screenshot rect, or the mapped logical point falls in a gap between displays (union-of-rects validation). The message names the valid rect(s). | Take a fresh default-policy `screenshot` of the target display and derive coordinates from THAT image; pass the same `display` arg on the mouse action. Don't target dead zones between monitors. |
| `gui_unavailable` | The connected worker is the headless build; only `capabilities` + `wait` exist there. (Error code retained for wire stability across the `gui` → `computeruse` rename.) | Install/run `memql` on the host (`make cockpit-computeruse`), then `worker setup` + `worker run`. Check `capabilities` first: `computerUseAvailable: false` means this build. |
| `window_not_found` | The `windowId` is not in the current enumeration -- window ids change as windows open/close. | Re-run `window_list` and use a fresh id. |
| `unsupported_on_platform` | `window_list` on Wayland or a platform without a backend (no portable Wayland window-enumeration API exists). | Use an X11 session (Linux) or macOS; check the descriptor's `displayServer` before calling. |
| `unknown_action` (memql side) | The action is not in the memql scope table (`scope.go`) -- deny-by-default for unclassified actions. | Verify the action name; if it is a newly added cockpit action, the memql-side classification must land first (see "Evolving the contract"). |
| `tool_panic` | A cgo/RobotGo path panicked (environment mismatch the preflights do not cover, e.g. RobotGo reads on Wayland). The dispatcher recovers it to keep the stream alive; the stack is on the cockpit's stderr. | Check the cockpit log for the stack; fix the environment (usually display-server related) and report a preflight gap if reproducible. |

## Evolving the contract

Adding a `workerComputer` action is a three-repo lockstep
(memql -> memql -> bff), in that order so nothing is exposed
to agents before every gate knows about it. Reference: epic
memql#161 (the v2 wave) for worked examples.

1. **Cockpit (this repo)** -- implement and gate:
   - Handler in `computerActionHandlers` (`computer_computeruse.go`) for
     computer-use-backed actions, or alongside `wait`/`capabilities` for
     build-agnostic ones (dispatcher routes those before the
     preflights and the per-build router). The capability descriptor
     picks the action up automatically from the handler map.
   - Consent class: add the action explicitly to `consent.Classify`
     (observe vs interact) and decide whether it joins the
     strict-mode high-risk subset (`isHighRiskAction`) -- anything
     that composes into typed text or clicks belongs there.
   - Preflight set: extend `isComputerInputAction` (Accessibility +
     display-server gates) if it posts input; screenshot-like capture
     needs the Screen Recording gate in `preflightComputerAction`.
   - Tests on the untagged side wherever possible (the `-tags computeruse`
     suite does not run in CI; the computeruse lanes build + vet only).
2. **memql** -- classify and admit:
   - `integrations/agent/worker/scope.go` `workerComputerScope`:
     required capability (`HEADLESS` vs `COMPUTERUSE`) + scope tier
     (`observe` vs `full`). Until this lands the memql dispatch gate
     denies the action with `unknown_action` -- deny-by-default is
     the designed failure mode, not a bug.
   - Safety descriptor classification + the
     `v1:worker:invocation.action` description in
     `dsl/worker/concepts.memql`.
3. **bff (last)** -- expose to the frontend: extend the
   `tools.memql` action enum so the agent tool schema advertises it.
   This goes last because it is the exposure step; an action listed
   there but unknown to memql or the cockpit fails confusingly.

Descriptor shape changes: additive JSON fields do NOT bump
`schemaVersion` (the server tolerates unknown fields -- `displays`
landed this way); incompatible shape changes bump the cockpit's
`capabilitySchemaVersion` and the memql server's
`CapabilityDescriptorSchemaVersion` in lockstep, since the server
admits exactly one version.
