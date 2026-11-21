# Private media-worker transport

Build with `go build -o /absolute/private/path/memql-call-worker-bridge
./cmd/memql-call-worker-bridge`. This operator-side process connects an authorized
media worker to the existing MemQL typed Go SDK. It is not a public agent tool,
HTTP service, arbitrary-query proxy or replacement for native authorization.

Configure `MEMQL_WORKER_ENDPOINT` and `MEMQL_WORKER_OPERATOR_TOKEN` privately in
the environment. Use verified HTTPS/gRPC TLS; `MEMQL_WORKER_CA_FILE` optionally
supplies a PEM trust store. An explicit `MEMQL_WORKER_ALLOW_LOCAL=1` permits
plaintext only to loopback addresses for local port-forwarding. Credentials never
belong in arguments, URLs, process output or worker result records. Native calls
still require operator authority and reject execution from a user workflow run.

Each stdin line is a closed JSON request with an `id`, `action` and `siteId`.
`rooms` discovers eligible rooms and accepts no other fields. `lease` requires
`roomId` and optionally accepts `leaseId`, `reset` and integer `maxTracks` (0–300;
omitting it makes this adapter explicitly send 100). The native budget is required
so the SDK cannot omit a zero and accidentally authorize default capacity.
Zero capacity records an unavailable
processor without issuing media credentials. `publish` also requires `roomId`,
`leaseId`, positive integer `revision` and an `event` object. Only the generated
`CallsProcessingRooms`, `CallsProcessingLease` and `CallsProcessingPublish` SDK methods are called. IDs
are 1–128 ASCII letters/digits/underscores/hyphens. Unknown or duplicate envelope
fields are refused; the native service validates event contents and authority.

One stdout line answers each request: `{ "id": "...", "ok": true, "result": ... }`
or `{ "id": "...", "ok": false, "error": "fixed_code" }`. Invalid envelopes have
an empty response ID. Input is bounded to 64 KiB per line, responses to 512 KiB,
and each SDK call to ten seconds. Requests run serially; failures are never retried
implicitly. A lost publication response must retain its original event ID and
sequence so native deduplication can decide its outcome.

Error codes are `invalid_request`, `request_failed`, `deadline_exceeded` and
`response_too_large`. Raw engine/network errors are not echoed because they may
include sensitive context. Oversized input ends the process after a bounded error.
EOF or termination closes the SDK connection. A worker must stop media processing
on lease expiry or lost authority even if this subprocess remains connected.

The subprocess can return short-lived media credentials in a lease. Its pipes and
operator environment are private trusted-process boundaries, never log streams.
