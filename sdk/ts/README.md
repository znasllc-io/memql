# @znasllc-io/memql-sdk-core

The client-agnostic **runtime core** for the memQL web SDK. It owns
everything coupled to the wire protocol (the `/memql/ws` WebSocket
bridge to `MemqlService.Stream`) and nothing coupled to a particular
product's concept set.

It is consumed two ways:

- **Directly**, for the wire-level primitives below.
- **As the base of a product SDK.** Each backend-for-frontend (BFF)
  generates typed query/mutation/logic methods from its DSL and layers
  them onto `QueryClient`, then re-exports this core. The CoPresent web
  app, for example, depends on `@visionarys-io/copresent-sdk`, which is
  this core plus the generated CoPresent surface.

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

// conn.query         : QueryClient          (executeNamed / executeRaw / listConcepts / getMyAccess)
// conn.subscriptions : SubscriptionManager  (subscribe(pattern, handler) -> unsubscribe)
// conn.dispatcher    : Dispatcher           (low-level multiplexed stream)
// conn.rotateAuth(jwt)                       (swap the bearer on a live stream)
// conn.done() : Promise<void>               (resolves on stream close)
// conn.close() : void
```

Authentication is stamped onto the WebSocket URL (browsers cannot set
headers on the upgrade): `auth.bearer` -> `bearer_token`,
`auth.guestToken` -> `guest_token`, `auth.workerToken` ->
`worker_token`. Pass `onTokenExpired` to refresh without redialing.

### Subscriptions

```ts
const unsubscribe = conn.subscriptions.subscribe(
  "graph.node.created.*.v1:cognition:utterance",
  (event) => render(event),
);
```

The pattern grammar matches the server topic grammar verbatim.

### Voice

```ts
import { pushToTalk } from "@znasllc-io/memql-sdk-core/voice";

const final = await pushToTalk(conn.dispatcher, audioStream, {
  audio: { encoding: "opus", sampleRate: 48000, channels: 1 },
  onPartial: (p) => renderPartial(p.text),
});
```

### SI (chat / speech / transcribe / suggest)

One-shot SI ops on `MemqlService.Stream`. Each helper takes the
connection's `Dispatcher` directly and returns a typed result.

```ts
import { siChat, siChatStream, siSpeech, siTranscribe, siSuggest }
  from "@znasllc-io/memql-sdk-core/si";

// Non-streaming chat
const reply = await siChat(conn.dispatcher, [
  { role: "user", content: "Hi there" },
], { provider: "chat54Mini" });

// Streaming chat
const handle = siChatStream(conn.dispatcher, [
  { role: "user", content: "Stream me a story" },
]);
for await (const delta of handle.deltas) {
  if (delta.textDelta) process.stdout.write(delta.textDelta);
}
const finalReply = await handle.result;

// Text-to-speech
const audio = await siSpeech(conn.dispatcher, "Hello there", {
  voice: "alto",
  format: "wav",
});

// One-shot transcription (streaming STT lives in /voice)
const transcript = await siTranscribe(conn.dispatcher, audioBytes, {
  mimeType: "audio/wav",
});

// Suggest (spaces / spaceTitle / agents / groups / *CardSummary / knowledge)
const suggestion = await siSuggest(conn.dispatcher, "spaceTitle", {
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
CoPresent SPA. The audio client stamps the same `bearer_token` /
`guest_token` / `worker_token` query string on the URL the main
Connection uses.

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
  endpoint: "wss://app.copresent.ai/memql/audio",
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
  joinUrlBase: "https://app.copresent.ai",
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

## Exports

- `.` -- `Connection`, `Dispatcher`, `QueryClient`,
  `SubscriptionManager`, `Result` + row accessors
  (`rowString`/`rowBool`/`rowNumber`/`rowObject`/`rowArray`),
  `newShortId`, `renderMemQLValue`, and the shared types (`Concept`,
  `Event`, `Role`, `SubscriptionKind`, `AccessSummary`, `Row`). Also
  re-exports `identity`, `realtime`, `si`, and `voice` as namespace
  objects.
- `./client` -- the same client surface.
- `./identity` -- the 10 identity & access methods listed above.
- `./realtime` -- `polyphonRoomToken` (LiveKit token mint via the
  main stream) + `AudioClient` / `dialAudio` (separate WS for
  streaming STT + TTS on `/memql/audio`).
- `./si` -- `siChat`, `siChatStream`, `siSpeech`, `siTranscribe`,
  `siSuggest` and their types.
- `./voice` -- `pushToTalk` and its types.

## Build & test

```
npm run build     # tsc -> dist
npm run typecheck # tsc --noEmit
npm test          # compile + run node:test against the identity/SI/voice surface
```

ESM only, strict TypeScript, browser-targeted (`lib: ES2022 + DOM`).
