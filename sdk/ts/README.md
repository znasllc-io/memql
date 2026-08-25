# @znasllc-io/memql-sdk-core

The client-agnostic **runtime core** for the MemQL web SDK. It owns
everything coupled to the wire protocol (the `/memql/ws` WebSocket
bridge to `MemqlService.Stream`) and nothing coupled to a particular
product's concept set.

It is consumed two ways:

- **Directly**, for the wire-level primitives below.
- **As the base of a product SDK.** Each backend-for-frontend (BFF)
  generates typed query/mutation/logic methods from its DSL and layers
  them onto `QueryClient`, then re-exports this core. A product web app
  depends on its product SDK package, which is this core plus that
  product's generated surface.

The typed concept methods are **not** in this package by design -- they
live in the per-product SDK so the core stays identical for every
client. The generator and per-BFF wiring produce them.

## Install

Published to GitHub Packages under the `@znasllc-io` scope. Point the
scope at the GitHub registry in an `.npmrc`:

```
@znasllc-io:registry=https://npm.pkg.github.com
```

then:

```
npm install @znasllc-io/memql-sdk-core
```

## Transport

A single WebSocket carries everything, speaking protojson over the
engine's `MemqlService.Stream` bidirectional gRPC (browsers cannot do
raw gRPC). One multiplexed stream handles request/reply (correlated by
message id), per-request streaming sessions (transcription), and an
event fanout (subscriptions). There is no REST, gRPC-web, Connect, or
SSE.

## Usage

```ts
import { Connection } from "@znasllc-io/memql-sdk-core";

const conn = await Connection.dial({
  endpoint: "wss://staging.host/memql/ws",
  auth: { bearer: jwt }, // or { guestToken } / { workerToken }
});

// conn.query         : QueryClient          (executeNamed / executeRaw / listConcepts / subscriptionCatalog / subscribeConceptRegistry / getMyAccess)
// conn.subscriptions : SubscriptionManager  (subscribe(pattern, handler) -> unsubscribe)
// conn.dispatcher    : Dispatcher           (low-level multiplexed stream)
// conn.rotateAuth(jwt)                       (swap the bearer on a live stream)
// conn.uploadAttachment({ partitionId, file, fileName, contentType })
// conn.done() : Promise<void>               (resolves on stream close)
// conn.close() : void
```

Authentication travels as WebSocket subprotocols (browsers cannot set
headers on the upgrade, and query params would leak the credential into
access logs): `auth.bearer` dials with `["bearer", token]`,
`auth.guestToken` with `["guest", token]` -- the server negotiates the
scheme entry back on the 101. `auth.workerToken` still stamps
`worker_token` onto the URL until the worker surface migrates. Custom
`webSocketFactory` implementations MUST forward the `protocols`
argument to their WebSocket constructor. Pass `onTokenExpired` to
refresh without redialing.

For an older front door that does not negotiate the subprotocol scheme,
`auth.legacyUrlToken: true` opts back into the deprecated query-param
carry (`?bearer_token=` / `?guest_token=`). This leaks the credential
into request-line logs -- avoid it unless the target predates the
subprotocol channel. In-place rotation is driven by the auth source
(`onTokenExpired`), not the transport, so it works identically under
either carry.

### Subscriptions

Graph subscriptions are STRUCTURED: you name a concept and the CDC verbs,
and the SERVER composes the bus topic (memql#2460). The
`graph.node.<action>.<concept>` grammar never appears on the client wire,
and `subscribe(pattern, ...)` throws for `graph_events` — use
`subscribeGraph`:

```ts
const unsubscribe = conn.subscriptions.subscribeGraph(
  (event) => render(event),
  { concept: "v1:cognition:utterance", actions: ["created", "updated"] },
);
```

`subscribe(pattern, ...)` survives for the non-graph kinds only
(`telemetry`, `message`, `ai_stream`, `all`), and those now require the
`owner` or `admin` cluster role: they carry node-level events with no row
owner to authorize against (memql#4311).

#### `payloadOmitted` — the id-only notification

**A graph event only reaches you if you may read the row.** Since
memql#4309 the engine runs the same row-authorization gate at fan-out that
it runs on a read, so a subscription is no longer a way around one. Rows
you may not read are dropped; you are not told they existed.

One case cannot be decided against a single row — a concept whose tier is
`granted`, whose predicate is a relationship spec needing a join. Those
arrive with `event.payloadOmitted === true` and a payload carrying only the
row's identity (`concept`, `id`, `createdAt`, plus the topic/kind that say
which action fired). **Re-read the row through the normal read path and use
what that returns; if the read refuses, drop the event.**

```ts
conn.subscriptions.subscribeGraph(async (event) => {
  if (!event.payloadOmitted) return render(event.payload);
  try {
    const row = await getRowByConceptAndId(conn.query, conceptId, event.payload!.id as string);
    if (row !== null) render(row);
  } catch {
    // Refused: you were not entitled to this row.
  }
}, { concept: conceptId });
```

Ignoring the flag is safe but useless: you get a row whose fields are all
`undefined`, so a consumer degrades to rendering blanks rather than to
leaking anything. It is always `false` on an ordinary event (the wire omits
a false bool; the SDK normalises it).

#### `seq` and `gapBefore` — continuity

Every event carries `seq` (its position in **this connection's** delivery
sequence, from 1) and `gapBefore` (deliveries were dropped between the
previous notification and this one). The engine's per-stream event channel
is bounded and the forwarder drops on full rather than stalling a write, so
overload is real — what was missing until memql#4536 was any way for you to
find out.

**The answer to a gap is to RE-SEED**: re-run the read that produced your
current rows and fold subsequent events onto the fresh answer. There is no
replay to ask for, and deliberately so — a best-effort replay buffer is
worse than none, because it teaches clients to skip the re-seed path that is
the only correct answer when the buffer misses.

Two things to get right if you fold rows by hand:

- **A reconnect is a gap.** A new stream starts a new `seq` space at 1, so
  treat stream establishment as an implicit gap rather than comparing
  numbers across connections. `seq === 0` means "this server does not number
  its deliveries" (it predates the field), not "the first event".
- **Continuity is a property of the STREAM, not of your subscription.**
  `seq` numbers every notification on the socket, and `gapBefore` lands on
  whichever delivery happens to come first after a drop. So a handler
  watching only its own subscription sees holes that belong to its
  neighbours, and may never see the flag for the events it actually lost.
  Use `conn.subscriptions.onDelivery(...)`, which observes every delivery on
  the stream, and re-seed everything when it reports a break.

`LiveCollection` below does all of this for you.

### Auto-reconnect

Opt in at dial time and the SDK owns the drop: exponential backoff with full
jitter, every subscription replayed on the new stream with its original id,
and a status a UI can render.

```ts
const conn = await Connection.dial({
  endpoint: "wss://api.example.com/memql/ws",
  auth: { bearer, onTokenExpired },       // the bearer is re-resolved per redial
  reconnect: { enabled: true },           // 1s -> 30s, forever, by default
});

conn.onStatusChange(({ status, attempt }) => render(status, attempt));
conn.onConnectionCycle(() => reseedEverything());  // fires AFTER the replay
```

- `close()` **never** reconnects, and `done()` resolves only on a FINAL
  close — a drop the SDK recovers from is not the end of the connection.
- `retryNow()` collapses the current backoff. A "Retry" button should call
  it: the SDK is already retrying, so the button means *sooner*, not
  *instead*.
- Backoff resets once a stream has SURVIVED `stableAfterMs` (10s), not when
  a dial succeeds — a server that accepts streams and drops them immediately
  looks like success every time.
- Without the option nothing changes: one dial, `done()` on any close.

### LiveCollection — a list that stays current

`LiveCollection` is the machine every live surface was hand-rolling: read a
list, subscribe, fold events by id, re-read `payloadOmitted` rows, drop what
the read refuses, re-apply the read's own scope, notice a gap, and re-seed.

```ts
const store = liveStoreFor(conn);                 // one store per connection
const machines = store.collection<Row>("myMachines", {
  concept: "v1:worker:registration",
  seed: (cursor, signal) => readPage(cursor, signal),
  reread: (id, signal) => getRowByConceptAndId(conn.query, concept, id, { signal }),
  inScope: (row) => row.ownerUserId === me,       // the READ's scope, re-applied
});

machines.value.subscribe(() => paint(machines.value.snapshot));
// ... when this caller is done:
machines.release();
```

What it guarantees, and why each one is there:

- **Subscribe, THEN seed.** The engine registers a subscription
  synchronously and runs a read on a goroutine, so this order cannot miss a
  row; the reverse can miss one forever.
- **`seeding | live | degraded | disconnected`.** Render it. `degraded` is
  the state that did not exist before: rows on screen that are known to be
  behind, with a re-seed in flight. Rows are KEPT on a disconnect — an
  operator wants the last known answer labelled stale, not a blank.
- **Reference counted, keyed by (query + args + connection).** Two callers
  asking for the same key share one subscription and one seed, and the last
  release LINGERS before teardown — which is what makes navigating away and
  back issue zero new reads.
- **In-memory only.** A full page reload starts clean by construction.
- **`LiveValue`** is the single-read counterpart with in-flight dedupe: N
  callers in the same tick produce one round trip.

The store is framework-free on purpose. A React binding is ~40 lines over
`subscribe()` + `snapshot`; `clients/portal/src/cluster/useLive.ts` is the
worked example.

### Voice

```ts
import { pushToTalk } from "@znasllc-io/memql-sdk-core/voice";

const final = await pushToTalk(conn.dispatcher, audioStream, {
  audio: { encoding: "opus", sampleRate: 48000, channels: 1 },
  onPartial: (p) => renderPartial(p.text),
});
```

### Space attachments

`uploadAttachment` uploads a file to a space over the front door's
`POST /spaces/{partitionId}/attachments` endpoint (multipart, `file`
field) and returns the created attachment reference. It is the one HTTP
(non-WebSocket) helper in the core; everything else rides the stream.

It is auth-consistent with the dialed connection: it targets the same
origin the WebSocket dialed and sends the connection's CURRENT bearer,
so an upload issued after an in-place rotation carries the rotated
token. Only bearer-authenticated connections can upload.

```ts
const ref = await conn.uploadAttachment({
  partitionId: "spc-1",
  file: fileBlob,            // Blob | Uint8Array | ArrayBuffer
  fileName: "report.pdf",
  contentType: "application/pdf", // used when `file` is not a typed Blob
});
// ref.id is the new attachment id; ref.raw is the untouched server JSON.
```

The standalone `uploadAttachment(source, params)` is also exported for
callers that hold a bearer + origin without a live `Connection`.

### AI (chat / speech / transcribe / suggest)

One-shot AI ops on `MemqlService.Stream`. Each helper takes the
connection's `Dispatcher` directly and returns a typed result.

```ts
import { aiChat, aiChatStream, aiSpeech, aiTranscribe, aiSuggest }
  from "@znasllc-io/memql-sdk-core/ai";

// Non-streaming chat
const reply = await aiChat(conn.dispatcher, [
  { role: "user", content: "Hi there" },
], { provider: "chat54Mini" });

// Streaming chat
const handle = aiChatStream(conn.dispatcher, [
  { role: "user", content: "Stream me a story" },
]);
for await (const delta of handle.deltas) {
  if (delta.textDelta) process.stdout.write(delta.textDelta);
}
const finalReply = await handle.result;

// Text-to-speech
const audio = await aiSpeech(conn.dispatcher, "Hello there", {
  voice: "alto",
  format: "wav",
});

// One-shot transcription (streaming STT lives in /voice)
const transcript = await aiTranscribe(conn.dispatcher, audioBytes, {
  mimeType: "audio/wav",
});

// Suggest (spaces / spaceTitle / agents / groups / *CardSummary / knowledge)
const suggestion = await aiSuggest(conn.dispatcher, "spaceTitle", {
  description: "a brainstorm session",
});
```

All five accept `{ signal }` for cancellation and throw on
`QueryError` replies or transport failure.

### Realtime media

`polyphonRoomToken` rides the main dispatcher (sendAndWait) and
mints a short-lived LiveKit room token for joining a space's voice
room. `dialAudio` opens a SEPARATE WebSocket to `/memql/audio` for
the older streaming-STT + streaming-TTS protocol used by the
product SPA. The audio client carries auth the
same way the main Connection does: bearer / guest via WebSocket
subprotocols, worker tokens on the query string.

```ts
import { polyphonRoomToken, dialAudio } from "@znasllc-io/memql-sdk-core/realtime";

// LiveKit room token (browser hands roomName + livekitUrl to the
// LiveKit client SDK).
const t = await polyphonRoomToken(conn.dispatcher, {
  spaceId: "spc-1",
  participantId: "ptp-1",
  displayName: "Alice",
});
liveKitRoom.connect(t.livekitUrl, t.token);

// Audio WS -- streaming STT.
const audio = await dialAudio({
  endpoint: "wss://app.example.com/memql/audio",
  auth: { bearer: jwt },
});
const stt = audio.transcribe({
  spaceId: "spc-1",
  participantId: "ptp-1",
  format: "opus",
  sampleRate: 48000,
  channels: 1,
});
mediaRecorder.ondataavailable = (ev) => stt.pushBytes(new Uint8Array(await ev.data.arrayBuffer()));
for await (const ev of stt.events) {
  if (ev.isFinal) finalize(ev.text, ev.utteranceId);
  else renderPartial(ev.text);
}

// Audio WS -- streaming TTS.
const tts = audio.synthesize({ text: "Hello", voice: "nova", format: "wav" });
for await (const ev of tts.events) {
  if (ev.kind === "chunk") playChunk(ev.audioBase64);
  if (ev.kind === "ended") finalize();
}
audio.close();
```

### Identity & access

Guest invites, worker tokens, session revocation, and policy
evaluation. The five guest-invite ops, both session-revoke ops, and
the two worker-token ops mirror their proto shapes 1:1. Typed
`errorCode` strings (e.g. `invalid_email`, `unauthenticated`,
`POLICY_NOT_FRONTEND_VISIBLE`) ride the returned object so callers
can branch without a try/catch; `QueryError` on the dispatcher path
still throws.

```ts
import {
  sendGuestInvite, resolveGuestInvite, joinSpaceAsGuest,
  cancelGuestInvite, resendGuestInviteEmail,
  revokeCurrentSession, revokeAllSessions,
  createWorkerToken, revokeWorkerToken,
  evaluatePolicy,
} from "@znasllc-io/memql-sdk-core/identity";

// Guest invites
const invite = await sendGuestInvite(conn.dispatcher, {
  spaceId: "spc-1",
  spaceName: "Brainstorm",
  inviterName: "Alice",
  email: "guest@example.com",
  joinUrlBase: "https://app.example.com",
  expiresInMinutes: 15,
});

// Unauthenticated /join/<token> lookup
const lookup = await resolveGuestInvite(conn.dispatcher, token);
if (lookup.status === "ok") {
  await joinSpaceAsGuest(conn.dispatcher, {
    participantId: newShortId(),
    displayName: "Guesty",
  });
}

// Per-device + cross-device sign-out
await revokeCurrentSession(conn.dispatcher);
await revokeAllSessions(conn.dispatcher);

// Worker tokens -- plainToken is shown ONCE; capture it now
const token = await createWorkerToken(conn.dispatcher, { name: "macbook" });
await revokeWorkerToken(conn.dispatcher, token.identityId);

// Policy evaluation -- frontend-visible bff-tier policies only
const decision = await evaluatePolicy(conn.dispatcher, {
  policyName: "canArchiveSpace",
  args: { spaceId: "spc-1" },
  returnTrace: true,
});
if (decision.errorCode) handleRejection(decision.errorCode);
else applyResult(decision.result);
```

### Tools (MCP) + inbound client-tool dispatch

`listTools` / `callTool` enumerate and invoke server-side tools (the
MCP request/reply pair). `registerClientToolHandler` is the inbound
side: when the server's agent loop resolves a tool marked
`client_execution=true` it pushes a `ClientToolCall`; the SDK runs
the registered handler locally and ships the result back as a
`ClientToolResult` correlated by `callId`.

```ts
import { listTools, callTool, registerClientToolHandler }
  from "@znasllc-io/memql-sdk-core/tools";

// Enumerate
const { tools, nextCursor } = await listTools(conn.dispatcher);

// Invoke
const r = await callTool(conn.dispatcher, {
  name: "createSpace",
  arguments: { title: "Brainstorm", architecture: "polyphon" },
});
if (r.isError) handleFailure(r.content);
else applyResult(r.content);

// Inbound dispatch (one handler per dispatcher).
const unregister = registerClientToolHandler(conn.dispatcher, async (call, signal) => {
  // call.toolName, call.argumentsJson (raw JSON), call.timeoutMs
  // signal aborts after timeoutMs so cooperative handlers can bail.
  const args = JSON.parse(call.argumentsJson || "{}");
  return runLocally(call.toolName, args, signal);
});
// later: unregister() to detach.
```

## Exports

- `.` -- `Connection`, `Dispatcher`, `QueryClient`,
  `SubscriptionManager`, `LiveStore` / `LiveCollection` / `LiveValue` +
  `liveStoreFor` / `disposeLiveStoreFor`, `Result` + row accessors
  (`rowString`/`rowBool`/`rowNumber`/`rowObject`/`rowArray`),
  `newShortId`, `renderMemQLValue`, and the shared types (`Concept`,
  `Event`, `Role`, `SubscriptionKind`, `AccessSummary`, `Row`,
  `LiveState`, `LiveSnapshot`, `ConnectionStatus`, `ReconnectOptions`).
  Also re-exports `identity`, `realtime`, `ai`, `tools`, and `voice` as
  namespace objects.
- `./client` -- the same client surface.
- `./identity` -- the 10 identity & access methods listed above.
- `./realtime` -- `polyphonRoomToken` (LiveKit token mint via the
  main stream) + `AudioClient` / `dialAudio` (separate WS for
  streaming STT + TTS on `/memql/audio`).
- `./ai` -- `aiChat`, `aiChatStream`, `aiSpeech`, `aiTranscribe`,
  `aiSuggest` and their types.
- `./tools` -- `listTools` / `callTool` (MCP outbound) and
  `registerClientToolHandler` (inbound `ClientToolCall` ->
  `ClientToolResult` dispatch).
- `./voice` -- `pushToTalk` and its types.

## Build & test

```
npm run build     # tsc -> dist
npm run typecheck # tsc --noEmit
npm test          # compile + run node:test against the identity/AI/voice surface
```

ESM only, strict TypeScript, browser-targeted (`lib: ES2022 + DOM`).
