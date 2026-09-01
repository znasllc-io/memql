# MemQL OS -- Ask voice, the theme marketplace, the VS Code artifact handoff

- **Date:** 2026-09-01
- **Status:** approved (owner brief: take the three filed follow-up epics
  end to end, in one PR, with the UX treated as the deliverable). Every cut
  below records the choice and the reason.
- **Epics:** memql#4747 (Ask voice), memql#4745 (theme marketplace),
  memql#4748 (VS Code artifact handoff). All three were filed by the
  memql-os-foundation epic (#4710) as follow-ups against contracts it shipped.
- **Scope:** `clients/os/` (Ask voice + the marketplace drawer + the theme
  registry), `sdk/ts/src/voice/` (one correctness fix), `component/edge/`
  (one response header), `editors/vscode/` (the `kind=artifact` route). No
  proto changes, no DSL changes, no new HTTP routes, no engine work.

## Why these three together

They are one PR because the owner asked for one, and they cost less together
than apart: all three build on contracts the foundation shipped deliberately
unfinished (the inert mic toggle, the `tokensHref` registry type, the
`kind=artifact` link the OS already fires), and each of the three turned out
to have a blocker that is invisible from the side the epic was written on.
Those blockers are the first section, because they are the part worth
reading.

## 0. Three things that were already broken, and would have shipped broken

Each of these is silent, and each fails in a way that accuses something else.

### 0.1 The edge forbade the microphone on every hosted site

`component/edge/csp.go` answered `Permissions-Policy: geolocation=(),
microphone=(), camera=()` -- ported from the identity service, where nothing
has ever needed any of the three. On the edge that is a different statement,
because the edge serves MemQL OS.

`microphone=()` disables `getUserMedia` for the document outright: the promise
rejects with `NotAllowedError` **before the browser shows a permission
prompt**, which is character-for-character what a person DECLINING the
microphone produces. Voice would have passed every test, worked perfectly
under `npm run dev` (vite sends no such header), and been dead in every
deployed cluster while reporting itself as the user's own choice.

The header is now `microphone=(self)`, and the widening is narrow: `self`
admits the site's own document and no cross-origin frame (the edge already
answers `frame-ancestors 'none'`), the per-origin permission prompt is
untouched, and every hosted bundle arrives through the authenticated publish
route. **Camera and geolocation stay closed**, and that asymmetry is the
point: a capability is opened when a first-party surface needs it, never
speculatively. `TestHostedSitesMayAskForTheMicrophoneAndNothingElse` is the
gate, and it names the escape hatch (make the policy a function of the SITE,
the way `policyForSite` already is) so the next person does not close it by
"harmonising" the four copies of that header.

### 0.2 `format` is a label the transcription server does not read

`AiTranscribeStreamStart` carries a `format` field, and the obvious browser
capture -- `MediaRecorder`, which yields webm/opus -- can declare
`format: "webm"` and look correct.

It is not. The cluster's default STT provider is `openai-realtime`, and
`integrations/stt/openai_realtime.go` passes only `SampleRate` through to the
ASR client: it never reads `Format`, and the client resamples what it is
handed from 16 kHz to the 24 kHz PCM the Realtime session is configured for.
Hand it opus and it resamples container bytes as though they were samples.
Nothing errors. The session opens, chunks flow, and the transcript comes back
as plausible nonsense.

(The other provider, `openai-whisper`, DOES decode webm -- and is batch, so it
emits no interim deltas at all. Live transcript and container audio are
mutually exclusive; live transcript is the feature.)

So the browser owns the conversion, and `clients/os/src/ask/pcm16.ts` is a
real resampler rather than a cast: 16 kHz, mono, signed 16-bit little-endian,
stateful across capture blocks, box-averaged rather than point-sampled.

### 0.3 Every artifact link was already being refused

`clients/os/src/items/vscode.ts` fires
`vscode://znasllc.memql/open?v=1&cluster=...&kind=artifact&id=...`, and the
extension's parser required `name` and had no `id` key at all. Every
double-click on a desktop file has been answered with "missing name" since the
foundation shipped -- and the OS's own fallback ("VS Code did not answer") is
worded for a DIFFERENT failure, so the symptom pointed at an uninstalled
extension.

## 1. Ask voice (memql#4747)

### D1.1 The mic control's geometry does not change

Spec C promised it when the foundation shipped the button inert, and it
decides the whole visual design: the level indicator is a **ring on the
existing 30px circle**, not a meter beside it. It is a `box-shadow`, which is
the shell's existing cue language ("the ring is a box-shadow, never a
background" -- the arrival cue's own rule, for the same reason: the button
sits on an opaque plate).

**The level never enters React state.** It moves at the frame rate; putting it
in state would re-render the streaming answer log sixty times a second to
animate one ring. It is written to `--os-mic-level` from a rAF loop that runs
only while the mic is live.

### D1.2 One control, two gestures, resolved on release

Press and you are live either way -- that is the property that makes
push-to-talk feel instant. A release under `LATCH_BELOW_MS` (400 ms) latches;
longer ends the utterance. A press while latched ends it, because the control
is the same control.

A release that arrives while the session is still `starting` is REMEMBERED and
applied when capture goes live: the first ever press blocks on a browser
permission dialog for as long as the person takes to read it, so a tap
resolves before the microphone exists.

### D1.3 Hold Space to talk -- sheet only, and never over a caret

The guard is "no editable element has focus", so Space still types a space in
the box it is aimed at. It is the **sheet** only: the sheet is a surface
somebody deliberately opened, while the desk widget is always on screen, and
Space on a desktop must not start recording. The mic button's own Space works
in both, because a focused button is an explicit target.

### D1.4 Deltas REPLACE the field; they never append

`AiTranscribeStreamDelta` carries the full accumulated transcript, not an
increment. Appending renders "openopen theopen the fleet". This is the
opposite of the chat path in the same component, which is exactly why it is
worth writing down.

The field is `readOnly` while the mic is writing it -- never `disabled`, so it
stays focusable and selectable -- because a character typed there vanishes on
the next word.

### D1.5 Send on release is the default, and the portal's rule still holds

The portal's own voice control records: *"the transcript is shown and
editable, always. Voice that ran straight through would be a black box."* That
was written against a BATCH transcribe, where nothing is visible until it is
over. This path streams: every delta replaces the field's contents while the
person is still speaking, so by the time they release they have already read
what was heard. Nothing is hidden, so nothing needs reviewing, and the fast
path is the point of talking to it. **"Put it in the box" is one setting away**
(Settings -> Ask), for people who want the pause.

### D1.6 There is no input-language setting, and that is the epic's own rule

The epic says "input language if the engine surface offers it; otherwise omit
-- do not invent". It does not offer it:
`component/grpc/ai_transcribe_stream.go` accepts `language_hint` on the wire
and discards it, pinning the language cluster-wide from `MEMQL_STT_LANGUAGE`,
because an auto-detected language is the documented cause of the
wrong-language hallucination on short, noisy audio. A picker would be a
control that changes nothing -- worse than an absent one, because the person
who sets it and keeps getting English has been told the fault is theirs.

There is no microphone PICKER either: Chrome and Safari both expose a per-site
input-device choice in the address bar, and a second one here would disagree
with the browser's own half the time.

### D1.7 A refusal is a standing fact, not a phase

`denied` / `no-device` / `device-busy` / `unsupported` are held ACROSS phases:
the control keeps explaining itself while the person carries on typing, and
typing does not clear it. Modelling it as a phase would force the surface to
choose between "idle" and "explaining", and idle is what it actually is.

`denied` covers both a genuine refusal and a Permissions-Policy that forbids
the page from asking, because the browser reports them identically. The
sentence therefore names the browser rather than accusing the reader of a
choice they may not have made.

**Errors render in surface, never as a dialog.** Safari refusals are a normal
case (the epic's phrasing), and text stays fully usable throughout.

### D1.8 One SDK fix, and it is the reason the failure states are reachable

`pushToTalk` registered a stream listener that handled Delta and Complete and
nothing else. Every Start precondition the server checks answers with a
correlated `queryError` -- and the commonest of them, "streaming transcription
is not configured", is what **every cluster with no voice node returns**.
`wire.ts`'s `streamRequestId` routes `queryError` by request id precisely so a
`registerStream` listener can see it; `ai/chat.ts` and `automationRun.ts` both
have that branch and this one did not. The caller awaited a promise that could
never settle, which renders as a microphone that stays lit forever.

The same fix stops the audio pump on every terminal path. Before it, an
aborted or refused session unregistered its listener and went on reading the
caller's stream -- in a browser, a microphone still being read after the
person let go.

`yieldDeltas` was declared in the options type and read by nothing. It is
deleted rather than implemented: an option that silently does nothing is a
trap, and it had no callers.

## 2. The theme marketplace (memql#4745)

### D2.1 A pack is DATA, not a stylesheet

The foundation's registry carried `tokensHref` -- "where the pack's token
stylesheet lives". That is the one part of spec G this epic changes, for two
reasons that point the same way.

It cannot be **validated**. The epic requires a loader that refuses an
incomplete token set, because "a theme that omits a token silently inherits
another theme's color, which reads as a broken product". A fetched stylesheet
is opaque to the page that fetched it.

It cannot be **trusted**. A stylesheet is arbitrary CSS. A pack that sets
`--os-cell-w` breaks the desk grid; one that zeroes `--os-duration-cue`
silently disables the arrival cue for a reader who never asked for reduced
motion; one with a `content:` rule can write text onto the shell.

So a pack is JSON carrying values, and the CSS is something the shell writes.
Token VALUES are whitelisted by character set -- the same posture
`component/edge`'s `validHost` takes, and for the same reason: a denylist of
`;{}` would miss the next character that turns out to matter.

### D2.2 A theme changes how the OS LOOKS, never how it behaves

The format carries the 21 mode-dependent colour/depth tokens (twice, dark and
light) plus the four wallpaper parameters and a seed. The tokens that decide
ERGONOMICS -- the type scale, the radii, the grid cell, the motion durations
-- are not in the format at all. Wallpaper numbers are individually bounded,
because a `cell` of 4 or an unbounded `linkReach` ships an OS that will not
scroll.

This is a narrowing of the epic's "the complete `--os-*` token set", and it is
deliberate: a marketplace must not be able to sell a broken desktop.

### D2.3 The accent is not a status colour

This token set reserves amber for `warn` and red for `error`, and the whole
shell reads them that way. A theme with a status-coloured accent puts that hue
on every primary button, focus ring and live dot in the OS, and status stops
meaning anything. Accents come from the cool half of the wheel. The three
built-ins take green (the brand), indigo and cyan, and
`test/themes/builtins.test.ts` refuses a hue between red and yellow.

### D2.4 The store is a DRAWER, because the product is the desktop

The gesture is apply-on-hover to the real desktop: point at a theme and the
desk, the dock, the wallpaper and every open window restyle behind the panel;
leave and they snap back. That decides the surface's shape rather than
following from it. A window would occupy a desk slot and cover half of what it
is previewing; the Launcher is a full-screen glass overlay and would hide all
of it. **So the Launcher's Themes tile closes the Launcher and opens a
right-edge drawer**, and the drawer's backdrop is the one modal backdrop in
this shell that does not tint what is behind it.

Ask is the precedent, not an exception being stretched: a surface whose
subject is the desktop itself cannot be a window on the desktop.

**Focus previews too.** The preview is the product, and a keyboard reader who
could only preview by choosing would be shopping by trying things on
permanently.

The preview is session state (`previewPack`), absent from
`documentFromState`, so nothing a pointer does can roam to another machine.

### D2.5 A card is two miniature desktops, drawn from the pack's own values

Not a swatch strip: six colours say nothing about what they do. Not a
screenshot: it would go stale, and it could not show the pack's OTHER mode.
Two miniatures side by side are the epic's "per-pack light + dark
verification" made visible; `test/themes/contrast.test.ts` is the same
verification as a number, checking four token pairings against WCAG AA in both
modes of every shipped pack.

### D2.6 Installed packs live in the desktop document, and roam

`DesktopDocument.installedPacks`, added WITHOUT bumping `version` -- the
precedent the legacy icon-group lift set. A bump makes `sanitizeDocument`
reject every document written by an older bundle, and somebody would lose
their desks because a theme list arrived. Each pack is re-validated on the way
IN rather than trusted because it was stored.

Built-ins are never stored: the bundle already has them, and a stored copy
that outlived a release would be a stale theme nobody could update.

Uninstalling the pack you are WEARING lands on graphite, because graphite is
the bundle's unqualified `:root` and therefore the only answer that always
exists.

### D2.7 Delivery and commerce -- the record the epic asked for

The epic requires this section before building, and one constraint answers
most of it. **The edge answers `connect-src 'self' <site> <ws> <identity>`**,
so the OS cannot fetch a pack from a third-party origin at all; the browser
refuses before the network is reached. "Where do packs live" is therefore not
a preference:

- **In the bundle** (the three built-ins). Shipped.
- **From a file the person has** -- drop it on the drawer, or choose it.
  Shipped: no network, no CSP question, and it exercises the loader's
  refusals, which is what makes them real.
- **From the cluster's own Library**, same-origin through the `_memql`
  marker. NOT built here. It is the productized path and it is a small amount
  of work on top of what exists (`artifactContentPath` already does exactly
  this fetch for Files), but it wants an owner decision about whether a theme
  is an ordinary artifact or gets a kind of its own.
- **From a vendor's own origin.** Not possible without widening the site CSP,
  and it should stay impossible: a per-site `connect-src` that names arbitrary
  vendors is a materially different security posture for every hosted site,
  bought for a theme.

**What "sell" means on a self-hosted cluster**: entitlement lives with the
pack's ORIGIN, not with the OS. Any client-side gate on a self-hosted browser
is theatre -- the operator owns the machine, the bundle and the storage. The
honest model is the one type foundries already use: the vendor serves the pack
file to a customer who paid, and the OS's job is browse, preview, apply, and
refuse a broken one. **No commerce code is built here, and none should be
until there is a second cluster asking for it.** The epic's own "not in the
initial scope" already excludes revenue infrastructure; this records that the
exclusion is a conclusion rather than a deferral.

## 3. The VS Code artifact handoff (memql#4748)

### D3.1 `id` is a fifth key, not a reuse of `name`

`OpenRequest` becomes a discriminated union on a new `target` field.
`kind=artifact` requires `id` and refuses `name`; every other kind the
reverse; both is refused. An artifact id gets its own validator rather than
borrowing the construct-name one -- a name validator that happens to admit ids
is an accident waiting to break.

### D3.2 Text-vs-binary is decided from METADATA, before any bytes move

`libraryArtifactById` (plus `libraryFileById` for a `kind:"file"` row) over the
dispatcher the extension already holds. Never buffer a file to discover it is
binary. Over the 8 MiB cap, even a text artifact is offered as save-to-disk
with the reason named -- which is also what the OS's own >512 MiB message
already points at VS Code to do.

### D3.3 Read-only by construction, and write-back is not built

A `TextDocumentContentProvider` document cannot be saved back, which is the
correct posture for a first cut: "edit and push to the Library" needs answers
about mirror concepts and stale rows that do not exist yet. There is no
disabled save command either -- a control that cannot work is not an
affordance.

### D3.4 The bearer is REPORTED, not resolved

`ConnectionManager` gained a public `bearer` that reports the credential the
live stream carries. Resolving one independently could authenticate a
DIFFERENT actor than the stream -- which surfaces as a 404, reading as "the
artifact is gone", because every refusal on that route is deliberately a 404.

## 4. What was deliberately not built

- **Per-window theme mix.** Out of the epic's scope. Note that the foundation
  spec claims window and widget roots already carry `data-os-theme`; they do
  not (`tokens.css` re-inherits by attribute PRESENCE). Stamping it is three
  one-line edits the day a mix is wanted.
- **A theme editor**, and revenue infrastructure. Both excluded by the epic.
- **Spoken answers, wake words, the LiveKit path.** Excluded by the epic;
  this is push-to-talk dictation into Ask, not the Polyphon room pipeline.
- **Write-back to the Library, remote fleet-machine files, artifact-version
  diffs.** Excluded by the epic.
- **A `Range`/resume path on the extension's save-to-disk.** The epic scopes
  it to open and save.

## 5. How it is verified

- `make os-test` -- the OS suite, including the pure voice state machine, the
  PCM16 fixtures, the pack loader's named refusals, the contrast floor for
  every built-in pack in both modes, and the marketplace's preview/revert.
- `make os-build` -- the only step that parses the stylesheet.
- `make vscode-test` + `go test ./cmd/memql-lsp/...` + the Extension
  Development Host lane.
- `go test ./component/edge/...` -- the Permissions-Policy gate.
- **A real browser.** jsdom lays nothing out and resolves no custom
  properties, so the drawer, the miniatures, the mic ring, the four-block
  theme CSS and the refusal copy were driven in Chrome against a temporary
  Vite harness. That pass is what caught the read-only field carrying an
  accent border on top of its own focus ring -- three accent-coloured things
  in one 30px row, with the ring that actually carries information no longer
  the thing the eye went to.
