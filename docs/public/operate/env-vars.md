---
title: Environment Variables -- memQL
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Environment Variables -- memQL

**Audience:** engineers running memQL locally or operating it in lab/prod.
**Last updated:** 2026-04-25 (post env-var refactor; Phase 8 complete)
**Companion doc:** [`copresent/docs/public/operate/env-vars.md`](../../../copresent/docs/public/operate/env-vars.md) covers the frontend side.

---

## TL;DR

memQL splits configuration into two tiers:

1. **Bootstrap envelope** -- a small set of OS environment variables the
   process must see *before* it can read anything else. Things like
   "where is Postgres", "what node am I", "what's the master encryption
   key". These are set in `docker-compose.full.yml` /
   `docker-compose.cluster.yml` (dev) or in the Cloud Run service
   manifest (prod). There is no encrypted-at-rest path for these --
   they live in plain env.
2. **Concept storage** -- everything else. API keys, OAuth client
   secrets, model defaults, feature flags, mail-sender addresses, and
   any tunable a tenant might want to override. These live in four
   memQL concepts and are seeded via the `make secrets-*` /
   `make variable-*` workflow rather than env files.

The bootstrap envelope is intentionally tiny so that rotating an API
key, changing a default model, or adding a new tenant's BYOK
credential never requires a redeploy -- only a `make variable-set`
or a re-seed of the operator's local yaml.

---

## Naming convention

Every env var name should answer "what subsystem owns this?" at a
glance. The standard shape is:

```
<COMPONENT>_<VENDOR_OR_DETAIL>_<FIELD>
```

Where `COMPONENT` is the subsystem that consumes the value:

| Prefix          | Subsystem                                                                                  | Example                                                                          |
|-----------------|--------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------|
| `MEMQL_`        | memQL itself: master key, node identity, transport, engine tuning.                         | `MEMQL_MASTER_KEY`, `MEMQL_NODE_TYPE`, `MEMQL_GRPC_ADDRESS`, `MEMQL_DEFAULT_*`.   |
| `MEMORY_NODES_` | Database tier (the row store).                                                             | `MEMORY_NODES_DATABASE_DSN`.                                                     |
| `MEMQL_SI_`     | Synthetic-intelligence providers (LLM / STT / TTS). Vendor goes after the prefix.          | `MEMQL_SI_OPENAI_API_KEY`, `MEMQL_SI_ANTHROPIC_API_KEY`.                         |
| `EMAIL_`        | Email integration (Microsoft Graph or SMTP sender).                                        | `EMAIL_AZURE_TENANT_ID`, `EMAIL_SENDER`, `EMAIL_FROM_NAME`.                      |
| `IDENTITY_`     | In-house identity service (auth subsystem) -- both the service itself and the per-node verifier.   | `IDENTITY_BASE_URL`, `IDENTITY_VERIFIER_BASE_URL`, `IDENTITY_KEY_ENCRYPTION_KEY`.|
| `ANAM_` / `SIMLI_` | Avatar vendors (lip-synced video). Used by the voice-agent avatar and the direct/Guide avatar (`integrations/avatardirect` + `integrations/avatarvendor`). | `ANAM_API_KEY`, `SIMLI_API_KEY`. |
| `POLYPHON_`     | Polyphon voice helpers (room provider + /memql/audio path).                                | `POLYPHON_VOICE_PROVIDER`, `POLYPHON_LIVEKIT_URL`.                               |
| `SERVER_`       | HTTP transport (listen address, public path, CORS).                                        | `SERVER_ADDRESS`, `SERVER_PUBLIC_PATH`.                                          |
| `SERVICE_`      | Service-level metadata (logging, name).                                                    | `SERVICE_NAME`, `SERVICE_CAPABILITIES_LOGGING_LOG_LEVEL`.                        |

Frontend (`VITE_*` prefix is added by Vite to mark "safe to ship to
the browser"):

| Prefix              | Subsystem                                                                  | Example                                                                  |
|---------------------|----------------------------------------------------------------------------|--------------------------------------------------------------------------|
| `VITE_MEMQL_`       | Backend connection URLs (memQL is the backend product name).               | `VITE_MEMQL_WS_URL`, `VITE_MEMQL_API_URL`.                               |
| `VITE_IDENTITY_`    | Identity-service metadata visible to the browser.                          | `VITE_IDENTITY_BASE_URL`.                                                |
| `VITE_OPENAI_`      | Direct browser-to-OpenAI calls (Realtime / STT / TTS model names).         | `VITE_OPENAI_REALTIME_MODEL`.                                            |
| `VITE_BYPASS_AUTH`  | Dev-only auth bypass.                                                      | -                                                                        |
| `VITE_ENABLE_ADMIN` | Admin panel feature flag.                                                  | -                                                                        |

### Anti-patterns to avoid

- **Vendor-only names without a component prefix.** `AZURE_TENANT_ID`
  is opaque -- Azure could mean storage, identity, OpenAI-on-Azure,
  or anything else. Always pair the vendor with the subsystem
  (`EMAIL_AZURE_TENANT_ID`).
- **Two prefixes for the same thing.** We had `MAIL_*` and
  `AZURE_*` for the same (email) integration; merging onto `EMAIL_*`
  with the vendor as the second segment removes that ambiguity.
- **The `MEMQL_` prefix where it's redundant.** Inside the memQL
  repo, every var is "memQL's" -- prefixing every one of them with
  `MEMQL_` is noise. Reserve `MEMQL_` for things that are about
  memQL itself (master key, node identity, engine tuning), not for
  things memQL happens to call (`OPENAI_API_KEY` reads cleaner than
  `MEMQL_OPENAI_API_KEY`).

### Migration window

When a name changes (like `AZURE_*` -> `EMAIL_AZURE_*` in 2026-04),
the consumer accepts both forms during a transition window so
existing installs don't break. The pattern is:

1. Update the manifest + .env.local + docs to the new name.
2. Add a fallback in the consumer (Go integration / DSL provider)
   that tries the legacy name if the new one is empty.
3. Remove the legacy fallback in a follow-up commit once everyone
   has re-seeded.

Search for `Legacy*EnvKeys` / "legacy fallback" in the Go code to
find the active migration shims.

### Future renames (deferred)

These would tighten the naming scheme but the change radius is too
wide to justify in the same commit as the doc:

- `MEMQL_SI_*_API_KEY` -> `SI_*_API_KEY`. The `MEMQL_` prefix is
  redundant inside the memQL repo and the dev manifest already
  seeds the bare form. Touches 6 provider `.memql` files plus Go
  bridge-agent and STT bootstrap; coordinate with manifest +
  user-yaml renames.
  
- `VITE_BYPASS_AUTH` -> `VITE_AUTH_BYPASS`,
  `VITE_ENABLE_ADMIN` -> `VITE_FEATURES_ADMIN_ENABLED` for stricter
  prefix consistency on the frontend.

If you're touching these areas anyway, fold the rename in. Don't
do them as drive-by churn.

---

## The four concepts

| Concept                  | Scope     | Encrypted | Purpose                                                                                                |
|--------------------------|-----------|-----------|--------------------------------------------------------------------------------------------------------|
| `v1:platform:globalSecret`     | global    | yes       | Instance-wide secrets (OpenAI API key, identity signing-key encryption secret, Azure Graph client secret, etc.) |
| `v1:platform:globalVariable`   | global    | no        | Instance-wide plaintext config (default chat provider, default language, identity base URL, etc.)              |
| `v1:platform:partitionSecret`        | partition | yes       | Per-tenant secrets. Falls back to `v1:platform:globalSecret` if no row exists for the active partition.      |
| `v1:platform:partitionVariable`      | partition | no        | Per-tenant plaintext config. Falls back to `v1:platform:globalVariable` if no row exists.                    |

Source files:

- `concepts/v1/platform/secret/concept.memql`
- `concepts/v1/platform/variable/concept.memql`
- `concepts/v1/memql/secret/concept.memql`
- `concepts/v1/memql/variable/concept.memql`

The `_system` partition is reserved for global concepts -- platform
secrets and variables live there regardless of which partition the
caller is in.

### Encryption

Secrets are sealed with **NaCl secretbox** (XSalsa20-Poly1305) under
`MEMQL_MASTER_KEY` (32-byte hex). The cleartext is never stored; only
`base64(nonce || ciphertext)` plus a 4-character fingerprint for UI
display. See `component/secret/encryption.go`.

### Resolution chain (provider auth)

When a `.memql` provider file references a placeholder like
`env("MEMQL_SI_OPENAI_API_KEY")`, the resolver in
`component/memql/si_providers.go` (`resolveAuthPlaceholders`) walks:

1. `v1:platform:globalSecret`     -- `systemSecretResolver`
2. `v1:platform:globalVariable`   -- `systemVariableResolver`
3. OS env                   -- bootstrap-window fallback. See the note
                               on bootstrap order below.

#### Prefix elision

Provider `.memql` files historically reference
`MEMQL_SI_<VENDOR>_API_KEY` while the dev manifest seeds the bare
form (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, ...). To bridge that
gap without renaming either side, every layer of the chain tries
**both** names in priority order:

```
authConceptLookupNames("MEMQL_SI_OPENAI_API_KEY")
  -> ["MEMQL_SI_OPENAI_API_KEY", "OPENAI_API_KEY"]
```

So a provider asking for `MEMQL_SI_OPENAI_API_KEY` will pick up a
value seeded as `OPENAI_API_KEY` automatically. The same elision
applies to the OS env fallback.

#### Why OS env stays around

Providers are loaded eagerly during engine initialization. On a
fresh `make dev-refresh`, the database wipe runs *before* the seed,
so when providers first try to resolve their auth keys the concept
storage is empty. The OS env fallback (populated from `.env.local`
in dev or from the deploy manifest in prod) keeps providers alive
through that bootstrap window until the seed completes.

Future work to retire the OS env fallback cleanly: either lazy
per-request provider auth resolution, or a post-seed engine reload
hook so providers retry concept storage once seeding finishes.

#### Failure mode

A miss at every layer produces:

```
auth "apiKey" references MEMQL_SI_OPENAI_API_KEY but no value is in
concept storage or OS env. Tried name(s) MEMQL_SI_OPENAI_API_KEY,
OPENAI_API_KEY under v1:platform:globalSecret, v1:platform:globalVariable, and
the process env. Seed it with `make secret-set NAME=OPENAI_API_KEY
VALUE=... SCOPE=global` (or `variable-set` for non-sensitive values)
```

For partition-scoped resolvers (DSL `resolveSecret(...)` /
`resolveVariable(...)`) the chain is:

1. `v1:platform:partitionSecret`        -- partition row
2. `v1:platform:globalSecret`     -- global row
3. (no env fallback for this path)

---

## Bootstrap envelope (set in env, not in concepts)

These are read at process startup. Putting any of them in a concept
would be circular -- the process can't reach the concept without
them.

### Required to start

| Variable                             | Purpose                                                                                                     | Read by                                |
|--------------------------------------|-------------------------------------------------------------------------------------------------------------|----------------------------------------|
| `MEMORY_NODES_DATABASE_DSN`          | Postgres+TimescaleDB connection string. No default; the process exits if missing.                           | `component/database/database.go`       |
| `MEMQL_MASTER_KEY`                   | 32-byte hex key for NaCl secretbox. Required as soon as any encrypted secret is read; a binary that never decrypts (rare) can boot without it but every realistic deployment needs it. | `component/secret/encryption.go`       |

### Required when the matching feature is enabled

| Variable                       | Required when                            | Notes                                                                                                                                              |
|--------------------------------|------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------|
| `IDENTITY_BASE_URL`            | identity binary, when `IDENTITY_ENABLED=true` | Public origin (e.g. `https://auth.example.com`). Used as JWT `iss` and in outbound email links.                                                       |
| `IDENTITY_KEY_ENCRYPTION_KEY`  | identity binary in non-localhost prod    | Master secret (>=16 bytes) wrapping the on-disk Ed25519 signing keypair. Sourced from `v1:platform:globalSecret` of the same name in production.        |
| `IDENTITY_VERIFIER_BASE_URL`   | non-identity binaries, prod auth          | URL the per-node verifier fetches JWKS from. Empty -> dev no-auth identity (`local-dev@memql.local`).                                                  |
| `MEMQL_WORKER_PEERS`           | cluster mode, BFF first boot             | Comma-separated `type=host:port` seed list (e.g. `agent=agent:50055,cognition=cognition:50054,planner=planner:50056`). DB-based discovery via `v1:cluster:node` takes over once peers register. Without it the BFF can't find workers on first boot. |
| `MEMQL_PARENT_ADDRESS`         | cluster mode, every non-BFF node         | `bff:50058` -- so the worker's outbound stream reaches BFF for event forwarding.                                                                   |

### Optional with sensible defaults

#### Node identity

| Variable                       | Default                  | Purpose                                                                                                                |
|--------------------------------|--------------------------|------------------------------------------------------------------------------------------------------------------------|
| `MEMQL_NODE_TYPE`              | `bff`                    | Node role. Compiled build tag (`-tags agent` etc.) takes precedence; this env var only matters for untagged binaries.  |
| `MEMQL_NODE_ID`                | auto-generated UUID      | Stable identifier for this instance.                                                                                   |
| `MEMQL_NODE_ADDRESS`           | empty                    | Address peers dial back. Required in cluster mode.                                                                     |
| `MEMQL_NODE_SERVICE_ADDRESS`   | `:50052`                 | NodeService gRPC listen address.                                                                                       |
| `MEMQL_NODE_FLAVOR`            | empty                    | Optional sub-type metadata; reserved for future use.                                                                   |
| `MEMQL_NODE_LABELS`            | empty                    | Comma-separated `k=v` metadata (e.g. `region=us-west,tier=prod`).                                                      |

#### Transport

| Variable                  | Default       | Purpose                                                                            |
|---------------------------|---------------|------------------------------------------------------------------------------------|
| `MEMQL_GRPC_ADDRESS`      | `:50051`      | MemqlService gRPC listen address.                                                  |
| `SERVER_ADDRESS`          | per-node      | HTTP listen address. Per-binary defaults: bff `0.0.0.0:8088`, cognition `8086`, planner `8087`, agent `8089`. |
| `SERVER_PUBLIC_PATH`      | `/`           | Base path prefix for HTTP handlers.                                                |
| `SERVER_ALLOWED_ORIGINS`  | `*` in dev    | CORS allowed origins (comma- or space-separated).                                  |

#### Logging

| Variable                                         | Default | Purpose                                                                              |
|--------------------------------------------------|---------|--------------------------------------------------------------------------------------|
| `SERVICE_NAME`                                   | per-node `memQL-bff` etc. | Logged on every record; useful for routing.                              |
| `SERVICE_CAPABILITIES_LOGGING_LOG_LEVEL`         | `info`  | Service-level log level (`debug`, `info`, `warn`, `error`).                          |
| `MEMORY_ENGINE_CAPABILITIES_LOGGING_LOG_LEVEL`   | `info`  | MemQL engine log level. Independent of the service logger.                           |

#### Database tuning

All optional. Defaults baked into `component/database/database.go`:

| Variable                                              | Default     |
|-------------------------------------------------------|-------------|
| `MEMORY_NODES_DATABASE_MIGRATE_ON_START`              | `true`      |
| `MEMORY_NODES_DATABASE_MAX_CONN_RETRIES`              | `3`         |
| `MEMORY_NODES_DATABASE_MAX_CONN_RETRIES_INTERVAL_MS`  | `1000`      |
| `MEMORY_NODES_DATABASE_TICKER_INTERVAL_MS`            | `30000`     |
| `MEMORY_NODES_DATABASE_MIGRATION_TIMEOUT_MS`          | `30000`     |
| `MEMORY_NODES_DATABASE_MAX_OPEN_CONNS`                | `25`        |
| `MEMORY_NODES_DATABASE_MAX_IDLE_CONNS`                | `5`         |
| `MEMORY_NODES_DATABASE_CONN_MAX_LIFETIME_MS`          | `3600000`   |
| `MEMORY_NODES_DATABASE_CONN_MAX_IDLE_TIME_MS`         | `600000`    |

#### Auth (Identity service + per-node verifier)

For the identity binary (`-tags identity`):

| Variable                                  | Default                  | Purpose                                                                                                              |
|-------------------------------------------|--------------------------|----------------------------------------------------------------------------------------------------------------------|
| `IDENTITY_ENABLED`                        | `false`                  | Master toggle for the identity binary itself.                                                                        |
| `IDENTITY_BASE_URL`                       | none                     | Public origin (used as JWT `iss` and in email links).                                                                |
| `IDENTITY_JWT_AUDIENCE`                   | `memql`                  | Value placed in the JWT `aud` claim.                                                                                 |
| `IDENTITY_KEY_DIR`                        | `var/identity/keys`      | On-disk Ed25519 keypair directory.                                                                                   |
| `IDENTITY_KEY_ENCRYPTION_KEY`             | none (required in prod)  | Master secret (>=16 bytes) wrapping the private key.                                                                 |
| `IDENTITY_REGISTRATION_MODE`              | `open`                   | `open` / `domain_restricted` / `invite_only` / `waitlist`.                                                           |

For every other binary (bff/voice/cognition/agent/planner):

| Variable                                       | Default | Purpose                                                                                                                              |
|------------------------------------------------|---------|--------------------------------------------------------------------------------------------------------------------------------------|
| `IDENTITY_VERIFIER_BASE_URL`                   | empty   | Public origin of the identity service. Empty -> dev no-auth identity (`local-dev@memql.local`) with SECURITY warnings in the logs.   |
| `IDENTITY_VERIFIER_AUDIENCE`                   | `memql` | Value compared against the JWT `aud` claim.                                                                                          |
| `IDENTITY_VERIFIER_EXPECTED_ISSUER`            | `BASE`  | Override for JWT `iss`. Defaults to `IDENTITY_VERIFIER_BASE_URL`.                                                                    |
| `IDENTITY_VERIFIER_JWKS_REFRESH_SECONDS`       | `300`   | Background JWKS refresh cadence.                                                                                                     |
| `IDENTITY_VERIFIER_JWKS_FETCH_TIMEOUT_SECONDS` | `10`    | Per-fetch HTTP timeout.                                                                                                              |
| `IDENTITY_VERIFIER_JWKS_URL`                   | derived | Override the JWKS URL when internal-mesh routing differs from the public origin.                                                     |

See [docs/public/operate/auth/identity-service.md](auth/identity-service.md) for
the full operator narrative (anti-abuse knobs, key rotation, email
delivery).

#### Feature toggles & engine tuning

| Variable                                        | Default | Purpose                                                                                  |
|-------------------------------------------------|---------|------------------------------------------------------------------------------------------|
| `MEMQL_STEP_CACHE_ENABLED`                      | `false` | Cache automation step results.                                                           |
| `MEMQL_DEMO_MODE`                               | `false` | Affects webhook step behavior; used by demo deployments.                                 |
| `MEMQL_COGNITION_FIT_THRESHOLD`                 | `0.4`   | Float in `[0,1]`; cognition turn-fit cutoff. Higher = stricter "should I respond?" gate. |
| `MEMQL_QUERY_MAX_RESULTS`                       | `10000` | Per-query row cap.                                                                       |
| `MEMQL_QUERY_MAX_WINDOW`                        | `100`   | Query optimizer lookahead window.                                                        |
| `MEMORY_ENGINE_CACHE_SIZE`                      | `1000`  | Concept-schema cache size.                                                               |
| `MEMORY_ENGINE_CACHE_MAX_TTL`                   | `300`   | Cache entry TTL (seconds).                                                               |
| `MEMORY_ENGINE_SI_TOOL_LOOP_MAX_ITERATIONS`     | `10`    | Max SI tool-calling iterations per turn.                                                 |
| `MEMQL_DSL_PATH`                                | unset   | Optional on-disk root for the .memql tree. When set and `<root>/<typeName>` exists, that DSL type reads from disk instead of the embedded copy. Per-type partial overrides supported. |
| `MEMQL_POLICYTRACE_RETENTION_DAYS`             | `90`    | Retention window (days) for v1:platform:policyTrace rows. Surfaced by `purgeExpiredPolicyTraces` cron. |

#### STT / voice (only if Polyphon or streaming STT is enabled)

| Variable                       | Default          | Purpose                                                                          |
|--------------------------------|------------------|----------------------------------------------------------------------------------|
| `MEMQL_STT_PROVIDER`           | auto (`deepgram` when `MEMQL_DEEPGRAM_API_KEY` is set, else `openai-realtime`) | `deepgram` / `openai-realtime` / `openai-whisper`. |
| `MEMQL_STT_LANGUAGE`           | `en`             | Hard-pinned transcription language for the streaming chat-mic path (`AiTranscribeStreamStart`). One knob drives BOTH providers: expanded to `en-US` on the Deepgram stream URL and to `en` on the OpenAI Realtime session config. Overrides any client-supplied `language_hint` -- pinning English is what stops the wrong/mixed-language + short-word-hallucination failure mode. |
| `MEMQL_STT_MIN_CONFIDENCE`     | `0.6`            | Floor a streaming FINAL transcript's confidence must clear to be emitted. Deepgram exposes real per-alternative confidence (noise/silence hallucinations come back below this); OpenAI Realtime finals carry `1.0` and always pass, relying on server-VAD + the empty/denylist filters. Also gates a no-speech denylist of well-known silence hallucinations ("thank you", "thanks for watching", ...) so they're dropped only when confidence is low. `0` disables the confidence + denylist gates (empty-text drop still applies). |
| `MEMQL_DEEPGRAM_API_KEY`       | none             | Deepgram API key. Required for the Deepgram path. Auto-selects Deepgram as the default ASR + TTS provider when present. |
| `POLYPHON_DEEPGRAM_ASR_MODEL`  | `nova-3`         | Deepgram ASR model id.                                                           |
| `POLYPHON_DEEPGRAM_TTS_MODEL`  | `aura-2-thalia-en` | Default Deepgram TTS model id; per-voice form (e.g. `aura-2-thalia-en`) resolved from the canonical voice catalog when the agent has a voice assigned. |
| `POLYPHON_DEEPGRAM_TTS_VOICE_OVERRIDE` | unset    | Force every Deepgram TTS synthesis to a specific Aura-2 voice id (e.g. `aura-2-asteria-en`), bypassing the canonical-voice catalog. A/B-testing voices. |
| `POLYPHON_DEEPGRAM_LANGUAGE`   | `en-US`          | BCP-47 language tag for Deepgram requests.                                       |
| `POLYPHON_DEEPGRAM_ENDPOINTING_MS` | `500`        | Silence (ms) before Deepgram fires `is_final=true`. Doubles as the end-of-utterance trigger in the default mode. Lower = faster STT tail latency; higher = better tolerance for mid-sentence pauses (less splitting of one user turn into multiple agent turns). |
| `POLYPHON_DEEPGRAM_UTTERANCE_END_MS` | unset (off) | Set to a non-zero value (Deepgram minimum 1000) to opt into UtteranceEnd-driven EOU. Trades latency (>= 1000ms floor) for no-split tolerance of long pauses. Leave unset for fastest behavior. |
| `MEMQL_OPENAI_REALTIME_MODEL`  | empty            | Realtime model id; falls back to `POLYPHON_OPENAI_ASR_MODEL`.                    |
| `MEMQL_WHISPER_MODEL`          | `whisper-1`      | Used when `MEMQL_STT_PROVIDER=openai-whisper`.                                   |
| `POLYPHON_VOICE_PROVIDER`      | auto             | `deepgram` (default when `MEMQL_DEEPGRAM_API_KEY` is set) or `openai`. Consumed by the `/memql/audio` WebSocket path. |
| `POLYPHON_OPENAI_ASR_MODEL`    | none             | OpenAI ASR model for the `/memql/audio` path.                                    |
| `POLYPHON_OPENAI_TTS_MODEL`    | none             | OpenAI TTS model for the `/memql/audio` path.                                    |
| `POLYPHON_OPENAI_TTS_VOICE`    | none             | OpenAI TTS voice (`alloy`, `echo`, `nova`, ...).                                 |
| `POLYPHON_PREDICTION_ENGINE_URL` | none           | External Polyphon prediction engine; absent = embedded engine.                   |
| `VOICE_AGENT_TOKEN`            | unset            | Identity-issued `class="voice_agent"` JWT the Go voice-agent presents on `MemqlService.Stream`. When empty the agent self-bootstraps via `/node/bootstrap` (dev). See `docs/public/operate/auth/voice-agent-jwt.md`. |
| `MEMQL_VOICE_EXECUTOR`         | `realtime`       | Go voice-agent executor: `realtime` (OpenAI gpt-realtime speech-to-speech, the default since #483) or `cascade` (Deepgram STT -> cognition -> Deepgram TTS). Realtime degrades cleanly to the cascade when its preconditions fail (no `OPENAI_API_KEY` / persona build), logging the reason -- so a fresh run uses realtime and there is no silent cascade surprise. Set `cascade` to opt out. The active executor is logged loudly at session start (`voice-agent voice executor: ...`). |
| `MEMQL_VOICE_ROOM_NAME`        | unset            | LiveKit room the Go voice-agent joins (memQL convention: `polyphon-<spaceId>`). Falls back here when no `--room` flag is passed. |
| `MEMQL_AVATAR_VENDOR`          | `anam`           | Avatar vendor on the voice-agent side: `anam`, `simli`, or `none`.               |
| `ANAM_API_KEY`                 | unset            | Anam (CARA-3) API key. Required when avatar vendor=anam.                         |
| `SIMLI_API_KEY`                | unset            | Simli API key. Required when avatar vendor=simli.                                |

#### Infra metadata

| Variable             | Default                   | Purpose                                                                          |
|----------------------|---------------------------|----------------------------------------------------------------------------------|
| `MEMQL_ENVIRONMENT`  | `development`             | Stamped on `system.startup` events and metadata enrichment.                      |
| `MEMQL_REGION`       | `local` (cascades from `MEMQL_ENVIRONMENT`) | Region label for events / metadata.                            |
| `K_REVISION`         | `os.Hostname()`           | Cloud Run injects this; falls back to hostname when running off-Cloud-Run.       |
| `MEMQL_GEOIP_DB_PATH`| none                      | Path to a GeoIP database; absent = no GeoIP enrichment.                          |
| `VERSION`            | `dev`                     | Falls back to reading the `VERSION` file, then to literal `"dev"`.               |

#### WebSocket tuning (rarely overridden)

All in `component/server/memqlws/env.go`:

| Variable                              | Default            |
|---------------------------------------|--------------------|
| `MEMQL_WS_DIAL_TIMEOUT_MS`            | `10000`            |
| `MEMQL_WS_WRITE_TIMEOUT_MS`           | `30000`            |
| `MEMQL_WS_MAX_CONCURRENT_REQUESTS`    | `100`              |
| `MEMQL_WS_MAX_MESSAGE_BYTES`          | `67108864` (64 MB) |
| `MEMQL_WS_PING_INTERVAL_MS`           | `30000`            |

---

## Concept-stored config

This is the table to look at when you ask "where do I put a new API
key" or "where do I change the default model".

The authoritative manifest is
[`scripts/secrets/manifest.yaml`](../../scripts/secrets/manifest.yaml).
Every entry in the manifest is what `make secrets-init` will prompt
for and what `make secrets-seed` will push into the running memQL.

### Default global secrets (manifest)

Stored in `v1:platform:globalSecret`, sealed under `MEMQL_MASTER_KEY`.

| Name                          | Kind             | Purpose                                                                                                                                   |
|-------------------------------|------------------|-------------------------------------------------------------------------------------------------------------------------------------------|
| `OPENAI_API_KEY`              | `vendor_api_key` | Instance-wide OpenAI key. Used by chat / TTS / STT / Realtime providers unless a tenant overrides it.                                     |
| `ANTHROPIC_API_KEY`           | `vendor_api_key` | Instance-wide Anthropic key for Claude chat / vision providers.                                                                           |
| `IDENTITY_KEY_ENCRYPTION_KEY` | `integration`    | Master secret (>=16 bytes) wrapping the identity service's on-disk Ed25519 signing keypair. Required in production.                       |
| `EMAIL_AZURE_CLIENT_SECRET`   | `oauth_secret`   | Microsoft Graph client secret used by the **email integration**'s GraphSender. Legacy name `AZURE_CLIENT_SECRET` still accepted (fallback). |
| `ANAM_API_KEY`                | `integration`    | Anam avatar vendor key (server-side). Used by the direct/Guide avatar (`integrations/avatardirect`) and the voice-agent avatar.            |
| `SIMLI_API_KEY`               | `integration`    | Simli avatar vendor key (server-side). Used by the voice-agent avatar (direct-path Simli support lands in #293).                          |

### Default global variables (manifest)

Stored in `v1:platform:globalVariable`.

| Name                          | Default                              | Purpose                                                                                                                                |
|-------------------------------|--------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------|
| `IDENTITY_BASE_URL`           | none                                 | Public origin of the identity service (e.g. `https://auth.example.com`).                                                               |
| `IDENTITY_VERIFIER_BASE_URL`  | matches `IDENTITY_BASE_URL`          | Override only when internal-mesh routing differs from the public origin.                                                               |
| `EMAIL_AZURE_TENANT_ID`       | none                                 | Azure AD tenant id used by the **email integration**'s GraphSender. Legacy name `AZURE_TENANT_ID` still accepted (fallback).           |
| `EMAIL_AZURE_CLIENT_ID`       | none                                 | Azure AD application id used by the email integration's GraphSender. Legacy name `AZURE_CLIENT_ID` still accepted.                     |
| `EMAIL_SENDER`                | none                                 | Sender address for transactional mail (e.g. `no-reply@znas.io`). Legacy name `MAIL_SENDER` still accepted.                             |
| `EMAIL_FROM_NAME`             | `memQL`                              | Display name in the From header. Legacy name `MAIL_FROM_NAME` still accepted.                                                          |

### Variables consumed by the CoPresent frontend

These aren't in the manifest yet -- operators add them via
`make variable-set` -- but they're documented here because they live
in `v1:platform:globalVariable` and are read by the CoPresent runtime
config layer (`src/lib/publicConfig.tsx`):

| Name                            | Typical value          | Consumer                                                                       |
|---------------------------------|------------------------|--------------------------------------------------------------------------------|
| `VITE_OPENAI_MODEL`             | `gpt-5`                | Default chat model on the frontend.                                            |
| `VITE_OPENAI_REALTIME_MODEL`    | `gpt-realtime`         | Realtime voice model.                                                          |
| `VITE_OPENAI_STT_MODEL`         | `gpt-4o-transcribe`    | Speech-to-text model.                                                          |
| `VITE_OPENAI_TTS_MODEL`         | `tts-1-hd`             | Text-to-speech model.                                                          |
| `VITE_OPENAI_VOICE`             | `shimmer`              | TTS voice.                                                                     |
| `VITE_OPENAI_PROJECT_ID`        | `proj_...`             | OpenAI org / billing project id.                                               |
| `VITE_DEFAULT_LANGUAGE`         | `en-US`                | UI language.                                                                   |
| `VITE_ENABLE_ADMIN`             | `true` / `false`       | Admin panel feature flag.                                                      |
| `MEMQL_DEFAULT_CHAT_PROVIDER`   | `chat54Mini`           | Forward-looking; whitelisted but not yet read by a consumer.                   |
| `MEMQL_DEFAULT_STREAM_PROVIDER` | `stream54Mini`         | Same.                                                                          |
| `MEMQL_DEFAULT_TTS_PROVIDER`    | `tts1Hd`               | Same.                                                                          |
| `MEMQL_DEFAULT_USER_LANGUAGE`   | `en-US`                | Same.                                                                          |

The exact name on the memQL side has to match the entry in the
publicConfig whitelist
([`src/lib/publicConfig.tsx`](../../../copresent/src/lib/publicConfig.tsx))
exactly. To add a new one: add it to the whitelist, then
`make variable-set NAME=... VALUE=... SCOPE=global`.

### Per-tenant overrides

Anything in `v1:platform:globalSecret` / `v1:platform:globalVariable` can be
overridden per-tenant by writing the same `name` into
`v1:platform:partitionSecret` / `v1:platform:partitionVariable` with the tenant's partition
stamped on the row.

The resolver always tries the partition-scoped row first and falls
back to the global one. So a tenant with `OPENAI_API_KEY` in their
partition's `v1:platform:partitionSecret` will use their own key; everyone else
keeps using the platform default.

This is the BYOK ("bring your own key") path. The DSL surface is
`resolveSecret("OPENAI_API_KEY")` and `resolveVariable("...")`; see
`component/memql/sense/builtins.go` for the builtin docs.

---

## The yaml file (`~/.memql/dev-secrets.yaml`)

This is **operator-local, gitignored, and dev-only**. It is the
plaintext stash of values that get encrypted-and-pushed to memQL on
`make secrets-seed`.

Schema:

```yaml
masterKey: <64-hex-character master key>     # 32 bytes hex-encoded
secrets:
  - name: OPENAI_API_KEY
    scope: global
    kind: vendor_api_key
    value: sk-proj-...
  - name: ANTHROPIC_API_KEY
    scope: global
    kind: vendor_api_key
    value: sk-ant-...
  ...
variables:
  - name: IDENTITY_BASE_URL
    scope: global
    value: https://auth.example.com
  - name: VITE_OPENAI_MODEL
    scope: global
    value: gpt-5
  ...
```

The yaml only matters for the **dev-refresh workflow**. In
production:

- The `MEMQL_MASTER_KEY` env var is set explicitly on the deploy
  target.
- Secrets and variables are seeded once via Make targets pointing at
  the prod gRPC endpoint, after which they live in the database.

### Where the yaml lives in the bootstrap chain

1. `make dev-refresh` runs `scripts/dev/refresh.sh`.
2. `require_master_key` in `scripts/dev/lib.sh` calls
   `go run ./scripts/secrets master-key`, which reads
   `~/.memql/dev-secrets.yaml` and prints the `masterKey` field.
3. The refresh script exports it as `MEMQL_MASTER_KEY` before
   `docker compose up`, so every container has the key in env.
4. After the stack is up, the same script runs
   `go run ./scripts/secrets seed`, which encrypts each yaml entry
   under the master key and upserts the row into the right concept
   over gRPC.

### Make targets

All driven by `scripts/secrets/main.go`:

| Target                                                          | Purpose                                                                          |
|-----------------------------------------------------------------|----------------------------------------------------------------------------------|
| `make secrets-init`                                             | Interactive walk through the manifest. Generates a master key on first run, prompts only for empty entries on subsequent runs. |
| `make secrets-seed`                                             | Encrypt + push every entry from the yaml into the running memQL.                 |
| `make secrets-list`                                             | Print the manifest, scope, and whether each entry has a value locally.           |
| `make secret-set NAME=X VALUE=Y SCOPE=global`                   | One-off; doesn't touch the yaml.                                                 |
| `make variable-set NAME=X VALUE=Y SCOPE=global`                 | Same for plaintext variables.                                                    |
| `make secrets-export`                                           | Pull every active secret + variable from the running memQL, decrypt locally, merge into the yaml (memQL wins on conflict). Used to back state up before a `dev-refresh` wipes the database. |

`dev-refresh` does export -> wipe -> restart -> seed in one shot,
so the yaml stays in sync as long as you go through that target.

### Master-key resolution order (in process)

`component/secret/encryption.go` reads `MEMQL_MASTER_KEY` from the OS
env at first encrypt/decrypt call. There is no fallback. If absent
when an encrypted secret is accessed, the process logs a fatal error.
The yaml passthrough above is purely operator tooling -- it puts the
key into the env before `docker compose up`. Inside the container,
the value is just an env var.

For non-dev installs, set `MEMQL_MASTER_KEY` directly on the deploy
target (Cloud Run env, Kubernetes secret, etc.). The yaml is never
deployed.

---

## How the cluster wires (peer discovery)

In cluster mode (multiple node-typed binaries), each non-BFF node
needs to know how to reach BFF, and BFF needs to know how to reach
each worker:

- `MEMQL_PARENT_ADDRESS` -- set on every worker (cognition, agent,
  planner, voice). Tells the worker to dial BFF for outbound event
  forwarding.
- `MEMQL_WORKER_PEERS` -- set on BFF (and on cognition for its
  agent-only narrowing). Comma-separated `type=address` list. First-
  boot seed only; once peers register themselves into
  `v1:cluster:node` (a global concept), DB-based discovery takes
  over.

Both are bootstrap envelope vars -- they have to be in the env
before the gRPC server starts.

`docker-compose.full.yml` and `docker-compose.cluster.yml` have full
worked examples. The full compose is the BFF + cognition + agent +
planner shape; the cluster compose adds voice.

---

## Adding a new entry: decision tree

```
Is the value sensitive?
├── Yes → secret
│   ├── Tenant-overridable (BYOK)? → v1:platform:partitionSecret (default), with v1:platform:globalSecret as the global default
│   └── Instance-only?              → v1:platform:globalSecret only
└── No → variable
    ├── Tenant-overridable?          → v1:platform:partitionVariable (default), with v1:platform:globalVariable as the global default
    └── Instance-only?              → v1:platform:globalVariable only
```

If the value has to be available *before* memQL connects to its
database (i.e. it controls how memQL connects), it's a bootstrap
envelope var, not a concept entry. There's a strong bias against
adding new entries to the bootstrap envelope -- it requires a
deploy-config change every time it rotates.

### Adding a global secret

1. Append a row to `scripts/secrets/manifest.yaml` under `secrets:`.
2. `make secrets-init` (re-walks; only prompts for the new entry).
3. `make secrets-seed`.
4. Reference it from a provider/integration via `env("YOUR_NAME")` in
   `.memql` or `os.Getenv("YOUR_NAME")` in Go (the resolver chain
   works for both).

### Adding a global variable

1. Append a row to `scripts/secrets/manifest.yaml` under
   `variables:`, *or* set it ad-hoc with
   `make variable-set NAME=YOUR_NAME VALUE=... SCOPE=global`.
2. The DSL resolver returns it from `resolveVariable("YOUR_NAME")` or
   the same `env()` chain in provider auth.

### Adding a per-tenant (partition-scoped) entry

`make secret-set NAME=... VALUE=... SCOPE=partition PARTITION=acme`
or the variable equivalent. The same resolver chain finds it
automatically.

---

## Reference: file paths

| File                                                                          | What it tells you                                                              |
|-------------------------------------------------------------------------------|--------------------------------------------------------------------------------|
| [`scripts/secrets/manifest.yaml`](../../scripts/secrets/manifest.yaml)        | Authoritative list of dev-bootstrap secrets + variables.                       |
| [`scripts/secrets/main.go`](../../scripts/secrets/main.go)                    | The CLI that powers every `make secret-*` / `make variable-*` target.          |
| `scripts/dev/lib.sh`                                                          | `require_master_key`, `wait_for_memql`, etc. used by `dev-refresh`.            |
| `scripts/dev/refresh.sh`                                                      | The wipe -> restart -> reseed orchestrator behind `make dev-refresh`.          |
| `concepts/v1/platform/secret/concept.memql`                                   | Schema for global encrypted secrets.                                           |
| `concepts/v1/platform/variable/concept.memql`                                 | Schema for global plaintext variables.                                         |
| `concepts/v1/memql/secret/concept.memql`                                      | Schema for partition-scoped encrypted secrets.                                 |
| `concepts/v1/memql/variable/concept.memql`                                    | Schema for partition-scoped plaintext variables.                               |
| `component/secret/encryption.go`                                              | NaCl secretbox + `MEMQL_MASTER_KEY` resolution.                                |
| `component/memql/si_providers.go` (`resolveAuthPlaceholders`)                 | Provider-auth resolver (the env() / placeholder chain).                        |
| `component/memql/sense/builtins.go`                                           | DSL surface (`resolveSecret`, `resolveVariable`).                              |
| `component/config/config.go`                                                  | One-stop list of bootstrap env-var reads.                                      |
| `component/database/database.go`                                              | Database-tier env reads (DSN + tuning).                                        |
| `component/identity/config.go`                                                | Identity service env reads (the binary itself).                                |
| `component/identity/verifier/config.go`                                       | Per-node verifier env reads (bff/voice/cognition/agent/planner).               |
| `component/node/identity.go`                                                  | Node-identity env reads.                                                       |
| `component/server/memqlws/env.go`                                             | WebSocket tuning env reads.                                                    |
| `docker/docker-compose.full.yml`                                              | Worked example of every required bootstrap env var for the dev stack.          |
| `docker/docker-compose.cluster.yml`                                           | Same, for full cluster mode (adds voice).                                      |

---

## Operational notes

### Rotating a secret

```bash
# In the operator's local copy (dev):
make secret-set NAME=OPENAI_API_KEY VALUE='sk-proj-newvalue' SCOPE=global

# Or for prod, point the same target at the prod gRPC endpoint by
# setting MEMQL_GRPC_ENDPOINT in the calling shell.
```

The old row is soft-deleted (`active=false`); `lastUsedAt` /
`rotatedAt` get stamped on the new row. The next decrypt picks the
new value; nothing else has to restart.

### Backing up state before a wipe

```bash
make secrets-export
```

Pulls every active row from the running memQL, decrypts secrets
locally with the master key, and merges the result into the yaml.
Conflict resolution: memQL wins. Run this before any
`make dev-refresh` that resets the database.

### "Why is my provider giving 'no value' errors?"

Check the resolver chain in order:

1. Is the row in `v1:platform:globalSecret` /
   `v1:platform:globalVariable`?
   ```bash
   make secrets-list
   ```
   or in DSL:
   `getQuery("queryConfigSecret", { name: "OPENAI_API_KEY" })`.
2. Does the running memQL have `MEMQL_MASTER_KEY` set in env?
3. Is the master key the **same one** that encrypted the row? If
   you regenerated it, the existing rows are unreadable -- run
   `make secrets-seed` again to overwrite with the new key.

### Local override without polluting the yaml

`make secret-set` / `make variable-set` write directly to the
running memQL without modifying the yaml. Useful for one-off
experiments. Note that on the next `dev-refresh` the wipe-and-reseed
will replace the value with whatever's in the yaml -- export first
if you want to keep it.

---

## Migration history

The current shape is the result of an 8-phase env-var refactor
completed 2026-04-25. Decision summary:

- **Two concept trees** (`globalSecret` / `globalVariable` and
  `partitionSecret` / `partitionVariable`) so per-tenant BYOK overrides
  fall back cleanly to the platform default.
- **NaCl secretbox** (XSalsa20-Poly1305) over AES-GCM for the
  encrypted half because it has a smaller surface, no nonce-reuse
  pitfalls when keys aren't rotated, and the Go stdlib has no native
  AES-GCM with built-in random nonces.
- **OS-env fallback stays** because providers initialize eagerly at
  engine boot, before the seed step has populated concept storage.
  The fallback keeps the BFF alive through that bootstrap window. A
  lazy per-request resolver or a post-seed engine-reload hook would
  let us retire it.
