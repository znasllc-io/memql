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

## Exports

- `.` -- `Connection`, `Dispatcher`, `QueryClient`,
  `SubscriptionManager`, `Result` + row accessors
  (`rowString`/`rowBool`/`rowNumber`/`rowObject`/`rowArray`),
  `newShortId`, `renderMemQLValue`, and the shared types (`Concept`,
  `Event`, `Role`, `SubscriptionKind`, `AccessSummary`, `Row`).
- `./client` -- the same client surface.
- `./voice` -- `pushToTalk` and its types.

## Build

```
npm run build     # tsc -> dist
npm run typecheck # tsc --noEmit
```

ESM only, strict TypeScript, browser-targeted (`lib: ES2022 + DOM`).
