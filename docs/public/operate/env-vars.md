---
title: Environment Variables -- MemQL
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Environment Variables -- MemQL

**Audience:** engineers running MemQL locally or operating it in lab/prod.
**Last updated:** 2026-04-25 (post env-var refactor; Phase 8 complete)
**Companion doc:** the product frontend repo's env-vars doc covers the frontend side.

---

## The registry (source of truth)

`scripts/secrets/manifest.yaml` is the authoritative registry of **every**
environment variable MemQL reads (Epic 7 / memql#2104). One file drives three
consumers so they can never drift: the seal floor, the cockpit Configuration
screen, and boot-time fail-fast validation. Each entry carries `component`,
`scope` (`node` / `global` / `partition`), `kind`, `required` (node types that
need it at boot, `"all"` = every node), `default`, and `description`. The small
non-`optional` set at the top of each section is the **seal floor** — the
strict superset a developer `.env` must cover to seal; the full universe is
registered `optional: true` so cataloguing every var never breaks local
sealing.

A var read in code but absent from the registry — or a registry entry that
appears nowhere in the repo — fails CI via `make env-registry-check`
(`go run ./cmd/envscan -check`). "Nowhere in the repo" means outside the
registry itself: the check excludes **both** copies of the manifest (the
authored one and the embedded `component/envregistry/manifest.yaml` snapshot),
because an entry's own row is not a reference to it. Missing the second copy is
what left the reverse direction unsatisfiable until memql#2971 — with both in
the scan every name appeared at least twice, so the staleness threshold could
never select. A name also has to appear as a **whole word**: 87 entries are
legacy aliases that are substrings of their `MEMQL_`-prefixed replacement, and a
substring match kept each one looking referenced by the very name that replaced
it. After editing the registry, regenerate the embedded snapshot with
`make env-registry-sync`.

Two limits worth knowing before you trust a green result:

- **The scan reads `.go`, `.memql`, `.yaml`, `.yml` and `.env*` only.** A var
  referenced solely from a `.md`, a `.sh`, the `Makefile`, a `Dockerfile`, or
  TypeScript counts as **stale** and will red CI. Register it somewhere the scan
  reads, or drop it. (No entry is in that position today.)
- **It excludes a fixed list of registry copies**, not "any file that looks like
  the registry". A third copy of the manifest added anywhere would give every
  entry a self-reference again and silence the reverse direction, exactly as the
  embedded snapshot did. The check hard-fails if a listed copy goes missing, but
  it cannot see an unlisted one — so if you add a copy, add it to
  `registryFiles` in `cmd/envscan/scan/scan.go`.

## TL;DR

MemQL splits configuration into two tiers:

1. **Bootstrap values** -- a small set of OS environment variables the
   process must see *before* it can read anything else. Things like
   "where is Postgres", "what node am I", "what's the master encryption
   key". They arrive by ONE path (epic memql#3958): as KEYS on the
   `memql-secrets` Secret every node `envFrom`s. Locally `make up`
   (`scripts/k3d/seed-secrets.sh`) writes them; in the cloud External
   Secrets reconciles them from Key Vault. There is no encrypted-at-rest
   path for these -- they live in plain env inside the pod.

   > There used to be a second path: a genesis **envelope**, sealed under
   > `MEMQL_GENESIS_B64`, that each pod decrypted in-process at boot,
   > applying ~150 vars set-if-absent. It is gone, along with its sealing
   > CLI and its `.znas` format. If a runbook tells you to seal one, it is
   > describing a mechanism that no longer exists.
2. **Concept storage** -- everything else. API keys, OAuth client
   secrets, model defaults, feature flags, mail-sender addresses, and
   any tunable a tenant might want to override. These live in four
   MemQL concepts and are seeded with `go run ./scripts/secrets seed`
   rather than from env files.

The bootstrap envelope is intentionally tiny so that rotating an API
key, changing a default model, or adding a new tenant's BYOK
credential never requires a redeploy -- only a re-seed, or a direct
write of the `v1:platform:globalVariable` row.

> **There is no `secrets-*` / `variable-*` family of make targets, and
> there never has been.** This document described one for its whole life
> -- init, seed, list, export, one-off set -- and every citation of it sent
> an operator to a command that does not exist (memql#4405). The tool
> underneath is real and takes exactly two subcommands, `seed` and
> `health`; the sections below describe those. A new bad citation now fails
> the build (`TestMakeTargetCitationsNameRealTargets`).

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
| `MEMQL_`        | MemQL itself: master key, node identity, transport, engine tuning.                         | `MEMQL_MASTER_KEY`, `MEMQL_NODE_TYPE`, `MEMQL_GRPC_ADDRESS`, `MEMQL_DEFAULT_*`.   |
| `MEMORY_NODES_` | Database tier (the row store).                                                             | `MEMQL_DATABASE_DSN`.                                                     |
| `MEMQL_SI_`     | Synthetic-intelligence providers (LLM / STT / TTS). Vendor goes after the prefix.          | `MEMQL_AI_OPENAI_API_KEY`, `MEMQL_AI_ANTHROPIC_API_KEY`.                         |
| `EMAIL_`        | Email integration (Microsoft Graph or SMTP sender).                                        | `MEMQL_EMAIL_AZURE_TENANT_ID`, `MEMQL_EMAIL_SENDER`, `MEMQL_EMAIL_FROM_NAME`.                      |
| `IDENTITY_`     | In-house identity service (auth subsystem) -- both the service itself and the per-node verifier.   | `MEMQL_IDENTITY_BASE_URL`, `MEMQL_IDENTITY_VERIFIER_BASE_URL`, `MEMQL_IDENTITY_KEY_ENCRYPTION_KEY`.|
| `ANAM_` / `SIMLI_` | Avatar vendors (lip-synced video). Used by the voice-agent avatar and the direct/Guide avatar (`integrations/avatardirect` + `integrations/avatarvendor`). | `MEMQL_ANAM_API_KEY`, `MEMQL_SIMLI_API_KEY`. |
| `POLYPHON_`     | Polyphon voice helpers (room provider + /memql/audio path).                                | `MEMQL_POLYPHON_VOICE_PROVIDER`, `MEMQL_POLYPHON_LIVEKIT_URL`.                               |
| `MEMQL_SERVER_` | HTTP transport (listen address, timeouts, public path, CORS).                              | `MEMQL_SERVER_ADDRESS`, `MEMQL_SERVER_PUBLIC_PATH`.                                          |
| `MEMQL_SERVICE_`| Service-level metadata (logging, name).                                                    | `MEMQL_SERVICE_NAME`, `MEMQL_SERVICE_CAPABILITIES_LOGGING_LOG_LEVEL`.                        |

Frontend (`VITE_*` prefix is added by Vite to mark "safe to ship to
the browser"):

| Prefix              | Subsystem                                                                  | Example                                                                  |
|---------------------|----------------------------------------------------------------------------|--------------------------------------------------------------------------|
| `VITE_MEMQL_`       | Backend connection URLs (MemQL is the backend product name).               | `VITE_MEMQL_WS_URL`, `VITE_MEMQL_API_URL`.                               |
| `VITE_IDENTITY_`    | Identity-service metadata visible to the browser.                          | `VITE_IDENTITY_BASE_URL`.                                                |
| `VITE_OPENAI_`      | Direct browser-to-OpenAI calls (Realtime / STT / TTS model names).         | `VITE_OPENAI_REALTIME_MODEL`.                                            |
| `VITE_BYPASS_AUTH`  | Dev-only auth bypass.                                                      | -                                                                        |
| `VITE_ENABLE_ADMIN` | Admin panel feature flag.                                                  | -                                                                        |

### Anti-patterns to avoid

- **Vendor-only names without a component prefix.** `AZURE_TENANT_ID`
  is opaque -- Azure could mean storage, identity, OpenAI-on-Azure,
  or anything else. Always pair the vendor with the subsystem
  (`MEMQL_EMAIL_AZURE_TENANT_ID`).
- **Two prefixes for the same thing.** We had `MAIL_*` and
  `AZURE_*` for the same (email) integration; merging onto `EMAIL_*`
  with the vendor as the second segment removes that ambiguity.
- **The `MEMQL_` prefix where it's redundant.** Inside the MemQL
  repo, every var is "MemQL's" -- prefixing every one of them with
  `MEMQL_` is noise. Reserve `MEMQL_` for things that are about
  MemQL itself (master key, node identity, engine tuning), not for
  things MemQL happens to call (`MEMQL_OPENAI_API_KEY` reads cleaner than
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
  redundant inside the MemQL repo and the dev manifest already
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

Source file: `dsl/platform/concepts.memql` (declares all four --
`globalSecret`, `globalVariable`, `partitionSecret`,
`partitionVariable`).

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
`env("MEMQL_AI_OPENAI_API_KEY")`, the resolver in
`component/memql/ai_providers.go` (`resolveAuthPlaceholders`) walks:

1. `v1:platform:globalSecret`     -- `systemSecretResolver`
2. `v1:platform:globalVariable`   -- `systemVariableResolver`
3. OS env                   -- bootstrap-window fallback. See the note
                               on bootstrap order below.

#### Prefix elision

Provider `.memql` files reference `MEMQL_AI_<VENDOR>_...` while the manifest
seeds the SEAL-FLOOR form (`MEMQL_OPENAI_API_KEY`, `MEMQL_ANTHROPIC_API_KEY`,
...). To bridge that gap without renaming either side, every layer of the chain
tries **both** names in priority order:

```
authConceptLookupNames("MEMQL_AI_OPENAI_API_KEY")
  -> ["MEMQL_AI_OPENAI_API_KEY", "MEMQL_OPENAI_API_KEY"]
```

So a provider asking for `MEMQL_AI_OPENAI_API_KEY` will pick up a
value seeded as `MEMQL_OPENAI_API_KEY` automatically. The same elision
applies to the OS env fallback.

Only the `AI_` segment is dropped -- `MEMQL_` is part of the seal-floor name.
The deprecated `MEMQL_SI_` prefix elides the same way and additionally keeps
its historical bare form (`OPENAI_API_KEY`) as a last candidate, so a product
DSL bundle still declaring the old prefix does not regress.

**This section described the behaviour before the code had it** (memql#4338).
The elision was written against `MEMQL_SI_`, which no provider in the tree uses
-- so it fired for none of them, and a key seeded under the documented
seal-floor name was simply not found. `TestSealFloorSecretResolvesForAnMemqlAIPlaceholder`
(`component/memql/ai_provider_auth_lookup_test.go`) is what now pins the
mapping this block claims.

#### Why OS env stays around

Providers are loaded eagerly during engine initialization. On a
fresh `make down && make up`, the database starts empty, so when
providers first try to resolve their auth keys the concept storage
has no rows yet. The OS env fallback (populated from the k8s Secret
seeded by `make up` in dev, or from the deploy manifest in prod)
keeps providers alive through that bootstrap window until the concept
seed completes.

Future work to retire the OS env fallback cleanly: either lazy
per-request provider auth resolution, or a post-seed engine reload
hook so providers retry concept storage once seeding finishes.

#### Failure mode

A miss at every layer produces:

```
auth "apiKey" references MEMQL_AI_OPENAI_API_KEY but no value is in
concept storage or OS env. Tried name(s) MEMQL_AI_OPENAI_API_KEY,
MEMQL_OPENAI_API_KEY under v1:platform:globalSecret, v1:platform:globalVariable,
and the process env. Seed ANY of those names: put it in the node's environment
(locally `make secrets`; in a cluster, whichever secret store the deployment
reads), or store a v1:platform:globalSecret row under it. The last name listed
is the seal-floor form the manifest and docs/public/operate/env-vars.md use
```

#### The one exception: Anthropic's credential is optional at every layer

Six placeholders resolve to ABSENT instead of failing when no layer holds
them (`optionalAuthEnvNames`, memql#4334):

```
MEMQL_AI_ANTHROPIC_API_KEY
MEMQL_AI_ANTHROPIC_FEDERATION_RULE_ID
MEMQL_AI_ANTHROPIC_ORGANIZATION_ID
MEMQL_AI_ANTHROPIC_SERVICE_ACCOUNT_ID
MEMQL_AI_ANTHROPIC_WORKSPACE_ID
MEMQL_AI_ANTHROPIC_IDENTITY_TOKEN_FILE
```

They are Anthropic's credential, and it is the one case where an absence is a
legitimate configuration rather than a mistake -- in both directions. The four
federation ids are unset on every local cluster, and the API key is unset in
the cloud once the federation cutover finishes. Left non-optional, either state
would take every Claude provider out of the registry with a WARNING rather than
an error -- which, locally, is a log line nobody reads.

So the absence is not decided here. `anthropicCredential`
(`component/memql/ai_anthropic_federation.go`) is the one place that knows
which combination is meaningful, and it decides:

| What is set | Result |
|---|---|
| all four federation values | federate |
| none | require `MEMQL_AI_ANTHROPIC_API_KEY`, as before |
| both | federate; one warning that the key is ignored |
| one, two or three | **refuse to boot**, naming the missing names |

The seeding hint the resolver would have printed moves with the decision: the
no-credential error names `MEMQL_AI_ANTHROPIC_API_KEY` and the seeding line for
it.

Optionality is an ALLOW-LIST of exactly these six, not a new default; every
other placeholder still fails its provider with the message above.
`MEMQL_AI_ANTHROPIC_WORKSPACE_ID` is optional even when federating -- Anthropic
needs it only when the rule spans more than one workspace.

Runbook: [auth/anthropic-federation.md](auth/anthropic-federation.md).

For partition-scoped resolvers (DSL `resolveSecret(...)` /
`resolveVariable(...)`) the chain is:

1. `v1:platform:partitionSecret`        -- partition row
2. `v1:platform:globalSecret`     -- global row
3. (no env fallback for this path)

---

## The minimal install envelope

**Installing, starting, repairing, upgrading and uninstalling a MemQL cluster
require no AI provider credential, and make no call to any AI vendor.**

That is the guarantee, stated plainly because it used to be false in one
place: the install wizard listed an AI provider and a key file among its
required fields, and the install graph ran an authenticated models-list probe
against the vendor before it would touch the machine. Neither was ever an
engine requirement -- a provider whose key does not resolve registers as
*unavailable* and is skipped at selection, and nothing refuses boot over it.
Epic memql#4440 removed the requirement; this section is the contract it left
behind.

### What the guarantee covers

| Verb | Vendor calls with nothing configured | With a key supplied at install |
|---|---|---|
| install | none | ONE authenticated `GET /v1/models`, which spends no tokens |
| start | none | none |
| repair | none | the same one probe, only if a key is recorded or supplied |
| upgrade | none | none |
| uninstall | none | none |

Supplying a key at install still works and behaves exactly as before: it is
verified before anything on the machine changes, then seeded. Supplying
nothing skips the `providerKey` step -- reported as `skipped` with the reason
"no key supplied -- configure AI providers in the portal" -- and seeds
nothing.

### What is actually required

**Read it from the registry, not from here.** The authoritative set is the
`required:` axis in
[`scripts/secrets/manifest.yaml`](../../../scripts/secrets/manifest.yaml),
consumed at boot by
[`component/envregistry/bootvalidate.go`](../../../component/envregistry/bootvalidate.go)
keyed on `MEMQL_NODE_TYPE`. To ask it directly:

```bash
go run ./cmd/envscan -check          # every read, against the registry
```

As a shape rather than a list to keep in step: **every node type needs the row
store and the address it serves on**, `identity` adds its own public origin
(which becomes the JWT issuer, and nothing can derive it), and `voice` adds
LiveKit's three. Nothing else in the registry is required by any node type,
and **no AI variable is required by any of them**.

`MEMQL_MASTER_KEY` is not on the `required:` axis and is still effectively
needed by every real deployment: it decrypts sealed values at rest, so a node
that never reads an encrypted secret can boot without it and no realistic one
does. It is separate from `MEMQL_OPERATOR_KEY`, which authenticates
(memql#3519) -- see
[operator-credential.md](auth/operator-credential.md).

Everything AI-related is **portal-configured**: the models this cluster can
call, and how it authenticates to them, live at **Settings -> AI providers**
(owner-only). Workload identity federation is the recommended path for
Anthropic, and needs no key at rest at all --
[anthropic-federation.md](auth/anthropic-federation.md).

### How this stays true

Four gates, because a guarantee in prose is a guarantee until the first
plausible local reason to break it:

- `TestNoAIVariableIsRequiredByAnyNodeType` (`component/envregistry`) sweeps
  every `MEMQL_AI_*` / Anthropic / OpenAI entry against all nine node types.
- `TestNoAIVariableIsInTheSealFloor` keeps them out of the *other*
  requiredness axis. This is the one the audit found broken:
  `MEMQL_OPENAI_API_KEY` and `MEMQL_ANTHROPIC_API_KEY` carried no
  `optional: true` while every sibling did, so a developer could not seal a
  `.env` without a vendor key -- the same requirement, in the one place nobody
  looked.
- `TestMinimalEnvelopeIsWhatTheDocSays` pins the per-node-type set, so a node
  type gaining a required variable fails a test and has to be justified rather
  than discovered by an operator.
- The **install-e2e lane runs keyless by default**
  (`.github/workflows/install-e2e.yml`), with the keyed round trip kept beside
  it for the verify path. A future step that quietly demands a key breaks CI
  rather than an operator.

The two requiredness axes are easy to conflate and the manifest's own comment
spells them out: `required:` drives BOOT VALIDATION (which node types refuse
to start), `optional:` drives the SEAL FLOOR (what a developer's `.env` must
cover). An AI variable must be outside both.

---

## Bootstrap envelope (set in env, not in concepts)

These are read at process startup. Putting any of them in a concept
would be circular -- the process can't reach the concept without
them.

### Required to start

| Variable                             | Purpose                                                                                                     | Read by                                |
|--------------------------------------|-------------------------------------------------------------------------------------------------------------|----------------------------------------|
| `MEMQL_DATABASE_DSN`          | Postgres+TimescaleDB connection string. No default; the process exits if missing.                           | `component/database/database.go`       |
| `MEMQL_MASTER_KEY`                   | 32-byte hex key for NaCl secretbox. Required as soon as any encrypted secret is read; a binary that never decrypts (rare) can boot without it but every realistic deployment needs it. | `component/secret/encryption.go`       |

### Required when the matching feature is enabled

| Variable                       | Required when                            | Notes                                                                                                                                              |
|--------------------------------|------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------|
| `MEMQL_IDENTITY_BASE_URL`            | identity binary, when `MEMQL_IDENTITY_ENABLED=true` | Public origin (e.g. `https://auth.example.com`). Used as JWT `iss` and in outbound email links.                                                       |
| `MEMQL_IDENTITY_SIGNING_KEY_B64`     | identity binary running **>=2 replicas** | Shared base64-std 32-byte Ed25519 seed (#550) -- every replica derives the SAME key + JWKS. **REQUIRED for any multi-replica deployment**; without it each pod mints its own key, JWKS diverges, and ~50% of token verifications fail with `unknown kid` (the 2026-06-16 staging outage, #1515). `Config.Validate()` fail-fast refuses to boot without it unless the issuer is a LOOPBACK host (`localhost` / `127.0.0.1` / `::1` / `0.0.0.0` / `*.localhost` -- one process, no possible second replica) or `MEMQL_IDENTITY_ALLOW_EPHEMERAL_KEY=true`. A `*.local.<domain>` issuer is NOT exempt: it is a local cluster's front door, and exempting it is memql#3400. Generate: `make identity-signing-key`. Delivered as a **key on the `memql-secrets` Secret** and by no other route (memql#3960) -- the cloud declares it in Key Vault through `deploy/external-secrets/externalsecret-memql.yaml`, and `make secrets` generates and preserves it locally. It could previously also ride inside the genesis envelope (sealed and decrypted in-process at boot), and that arm was in fact the only *declared* cloud path, so the redundancy read the opposite way round from how it actually stood. |
| `MEMQL_IDENTITY_KEY_ENCRYPTION_KEY`  | identity binary in non-localhost prod (file-key mode) | Master secret (>=16 bytes) wrapping the on-disk Ed25519 signing keypair. Only the file-key (no-seed) path. Sourced from `v1:platform:globalSecret` of the same name in production.        |
| `MEMQL_IDENTITY_VERIFIER_BASE_URL`   | non-identity binaries, prod auth          | URL the per-node verifier fetches JWKS from. Empty -> dev no-auth identity (`local-dev@memql.local`).                                                  |
| `MEMQL_WORKER_PEERS`           | cluster mode, first boot of any dialing node | Comma-separated `type=host:port` seed list (e.g. `agent=agent:50055,cognition=cognition:50054,planner=planner:50056`). Dialable types: `agent`, `cognition`, `identity`, `planner`, `voice`, `workbench`; anything else is ignored with a boot-time WARN naming the entry (memql#3450). DB-based discovery via `v1:cluster:node` takes over once peers register. Without it the BFF can't find workers on first boot. |
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
| `MEMQL_NODE_RECONCILE_INTERVAL_SECONDS` | `3`            | Active topology reconciliation loop interval (#1874). How often the leader replica reaps superseded-deployment orphans + mesh-absent nodes from `v1:cluster:node`. Integer seconds; non-positive/invalid -> default. |
| `MEMQL_NODE_RECONCILE_GRACE_SECONDS`    | `20`           | Grace window before a node continuously ABSENT from the live mesh is retired (#1874). Rides over transient gossip-propagation gaps; tighter than `MEMQL_NODE_STALE_PRUNE_MINUTES` (the 30-min lazy backstop). Integer seconds; non-positive/invalid -> default. |

#### Transport

| Variable                  | Default       | Purpose                                                                            |
|---------------------------|---------------|------------------------------------------------------------------------------------|
| `MEMQL_GRPC_ADDRESS`      | `:50051`      | MemqlService gRPC listen address.                                                  |
| `MEMQL_SERVER_ADDRESS`    | `:8085`       | HTTP listen address. The SAME default on every node type -- this row used to claim per-binary defaults of bff `0.0.0.0:8088`, cognition `8086`, planner `8087`, agent `8089`, and no binary has ever listened on any of them (memql#3892). In Kubernetes the `containerPort` and Service must move with it. Legacy name `SERVER_ADDRESS` still accepted. |
| `MEMQL_SERVER_PUBLIC_PATH` | `/`           | Base path prefix for HTTP handlers. Legacy name `SERVER_PUBLIC_PATH` still accepted (memql#3831). |
| `MEMQL_SERVER_ALLOWED_ORIGINS` | `*`      | CORS allowed origins for the generic HTTP middleware (comma- or space-separated). `*` is served WITHOUT credentials. Identity has its own list -- see `MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS`. Legacy name `SERVER_ALLOWED_ORIGINS` still accepted. |
| `MEMQL_SERVER_READ_TIMEOUT_MS` | `15000`  | `http.Server` ReadTimeout. Legacy name `SERVER_READ_TIMEOUT_MS` still accepted. |
| `MEMQL_SERVER_READ_HEADER_TIMEOUT_MS` | `5000` | `http.Server` ReadHeaderTimeout -- the Slowloris bound. Legacy name `SERVER_READ_HEADER_TIMEOUT_MS` still accepted. |
| `MEMQL_SERVER_WRITE_TIMEOUT_MS` | `15000` | `http.Server` WriteTimeout. Legacy name `SERVER_WRITE_TIMEOUT_MS` still accepted. |
| `MEMQL_SERVER_IDLE_TIMEOUT_MS` | `60000`  | `http.Server` IdleTimeout. Legacy name `SERVER_IDLE_TIMEOUT_MS` still accepted. |
| `MEMQL_SERVER_SHUTDOWN_TIMEOUT_MS` | `5000` | Graceful-shutdown budget for the HTTP server, spent after the drain delay and the in-flight wait. Legacy name `SERVER_SHUTDOWN_TIMEOUT_MS` still accepted. |

#### Logging

| Variable                                         | Default | Purpose                                                                              |
|--------------------------------------------------|---------|--------------------------------------------------------------------------------------|
| `MEMQL_SERVICE_NAME`                             | `MemQL` | Logged on every record; useful for routing. Every deployed node sets the same value -- the "per-node `MemQL-bff`" this row used to claim is not what the manifests do (memql#3892). Legacy name `SERVICE_NAME` still accepted. |
| `MEMQL_SERVICE_CAPABILITIES_LOGGING_LOG_LEVEL`   | `info`  | Service-level log level (`debug`, `info`, `warn`, `error`). Legacy name `SERVICE_CAPABILITIES_LOGGING_LOG_LEVEL` still accepted. |
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
| `MEMORY_NODES_DATABASE_MAX_OPEN_CONNS`                | `10`        |
| `MEMORY_NODES_DATABASE_MAX_IDLE_CONNS`                | `1`         |
| `MEMORY_NODES_DATABASE_CONN_MAX_LIFETIME_MS`          | `3600000`   |
| `MEMORY_NODES_DATABASE_CONN_MAX_IDLE_TIME_MS`         | `120000`    |

#### Connection pooling: hybrid endpoint split (`DIRECT_DSN`)

Tiger Cloud PgBouncer transaction-mode pooling decouples client
connections from server backends, so a deploy surge no longer maps 1:1
to Postgres slots (epic memql#1925). Transaction-mode poolers recycle a
server backend *between statements*, which would drop a held
**session-scoped** resource -- session advisory locks (cognition
dispatch gate, cron leader, reconciler, planner admission) and the
migrator's lock. MemQL therefore runs a **hybrid endpoint split**:

| Variable                              | Default        | Purpose                                                                                                                                                                                                 |
|---------------------------------------|----------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `MEMQL_DATABASE_DSN`           | required       | Main pool. Point this at the **transaction pooler** in pooled environments -- it carries all bulk queries + mutations (the surge-killing multiplier).                                                   |
| `MEMORY_NODES_DATABASE_DIRECT_DSN`    | unset          | Optional second, **non-pooled** endpoint. Session-stateful work (advisory locks, leader election, migrations) resolves its handle via `DirectBunDB()` so it bypasses the pooler. Small fixed pool (max 4 open / 1 idle). |

When `DIRECT_DSN` is **unset**, `DirectBunDB()` falls back to the main
pool, so behaviour is identical to a single-pool deployment -- local /
dev without a pooler is unaffected (env-agnostic). `MAX_OPEN_CONNS` /
`MAX_IDLE_CONNS` govern only the main pool; the direct pool is bounded
to a small fixed size by design.

#### Auth (Identity service + per-node verifier)

For the identity binary (`-tags identity`):

| Variable                                  | Default                  | Purpose                                                                                                              |
|-------------------------------------------|--------------------------|----------------------------------------------------------------------------------------------------------------------|
| `MEMQL_IDENTITY_ENABLED`                        | `false`                  | On the **identity binary**: serve-auth toggle (default off; must be `true` for identity to serve). On **every other node** it is the master auth-enforcement toggle instead -- see the next table. |
| `MEMQL_IDENTITY_BASE_URL`                       | none                     | Public origin (used as JWT `iss` and in email links).                                                                |
| `MEMQL_IDENTITY_JWT_AUDIENCE`                   | `memql`                  | Value placed in the JWT `aud` claim.                                                                                 |
| `MEMQL_IDENTITY_ALLOW_EPHEMERAL_KEY`            | `false`                  | Opt into per-pod ephemeral file keys (no `MEMQL_IDENTITY_SIGNING_KEY_B64`). GENUINELY single-process deployments only -- with >=2 replicas it allows JWKS divergence, and nothing in the process can detect the replica count. The fail-fast guard (#1515, narrowed in memql#3400) requires this to boot any deployment whose issuer is not a loopback host and which has no shared seed. |
| `MEMQL_IDENTITY_KEY_DIR`                        | `var/identity/keys`      | On-disk Ed25519 keypair directory (file-key mode).                                                                   |
| `MEMQL_IDENTITY_KEY_ENCRYPTION_KEY`             | none (required in prod)  | Master secret (>=16 bytes) wrapping the private key (file-key mode).                                                 |
| `MEMQL_IDENTITY_REGISTRATION_MODE`              | `open`                   | `open` / `domain_restricted` / `invite_only` / `waitlist`.                                                           |
| `MEMQL_IDENTITY_AUTH_ACTIVITY_RETENTION_DAYS`   | `30`                     | Days of `v1:identity:authActivity` history kept before a daily job on the identity node **hard-deletes** the rest (memql#4330). Clamped to `[1, 365]`; an out-of-range value is silently clamped rather than refusing boot. Unlike `MEMQL_IDENTITY_AUDIT_LOG_RETENTION_DAYS`, whose sweep only COUNTS, this one really deletes -- `authActivity` is one row per refresh-token rotation and one per PAT-authenticated request, so it is two orders of magnitude larger than the audit log and its value decays in weeks. **Refresh-token reuse detection looks back exactly this far** (memql#4329): a replayed token is recognised by matching a retired-token hash one of these rows recorded, and once the row is pruned the replay is indistinguishable from a stale cookie. The default is chosen to exceed both `MEMQL_IDENTITY_SESSION_IDLE_DAYS` (14) and the 30-day refresh-token TTL, so a token older than the window is already dead on its own account -- **lowering it below either of those opens a real detection gap, and nothing warns about it.** Watch `memql_auth_activity_pruned_total`: a flat zero over more than a day on a cluster that authenticates anyone means the sweep is not running. |
| `MEMQL_DISCOVERY_GRPC_ENDPOINT`                 | the identity host + a scheme-appropriate port | The dial address published as `grpcEndpoint` in `GET /.well-known/memql-config.json`. **A bare `host[:port]`, never a URL** -- a scheme is read for its port and then dropped, and a value that cannot be read as a host falls back to the default. Set it to the FRONT DOOR (`api.<domain>:443`), the only host whose ingress carries gRPC to the bff; the default derives the identity host, which serves HTTP only. Declared in `deploy/k8s/base/identity.yaml` and patched per overlay, so a stale value in an operator's local environment cannot reach the wire (memql#3399). |
| `MEMQL_DISCOVERY_CLIENT_ID`                     | the first registered client | The OAuth `client_id` published as `clientId` in the same document.                                              |
| `MEMQL_DISCOVERY_CLUSTER_NAME`                  | the identity host        | The human-readable default name published as `clusterName` in the same document.                                     |

For every other binary (bff/voice/cognition/agent/planner/workbench/mcp) --
every node type except identity itself (the JWKS authority, which does not
verify against itself) and edge (not an auth boundary; it serves public
bytes to anonymous site visitors):

| Variable                                       | Default | Purpose                                                                                                                              |
|------------------------------------------------|---------|--------------------------------------------------------------------------------------------------------------------------------------|
| `MEMQL_IDENTITY_ENABLED`                            | `true`  | **Master auth toggle.** `true`/unset -> authentication is ENFORCED (the verifier below is required). Set explicitly `false`/`0`/`no`/`off` to DISABLE auth for troubleshooting: the node skips the verifier and admits every stream as a synthetic local-dev cluster owner (`local-dev@memql.local`), emitting a loud boot-time SECURITY warning and pinning the `memql_auth_enabled` gauge to 0. Present in every installation; **never set false in a cloud cluster.** |
| `MEMQL_IDENTITY_VERIFIER_BASE_URL`                   | empty   | Public origin of the identity service. Required when auth is enabled (the default). Blanking it FATALS a verifier-consuming node -- to run without auth use `MEMQL_IDENTITY_ENABLED=false`, not an empty URL. |
| `MEMQL_IDENTITY_VERIFIER_AUDIENCE`                   | `memql` | Value compared against the JWT `aud` claim.                                                                                          |
| `MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER`            | `BASE`  | Override for JWT `iss`. Defaults to `MEMQL_IDENTITY_VERIFIER_BASE_URL`.                                                                    |
| `MEMQL_IDENTITY_VERIFIER_JWKS_REFRESH_SECONDS`       | `300`   | Background JWKS refresh cadence.                                                                                                     |
| `MEMQL_IDENTITY_VERIFIER_JWKS_FETCH_TIMEOUT_SECONDS` | `10`    | Per-fetch HTTP timeout.                                                                                                              |
| `MEMQL_IDENTITY_VERIFIER_JWKS_URL`                   | derived | Override the JWKS URL when internal-mesh routing differs from the public origin.                                                     |

See [docs/public/operate/auth/identity-service.md](auth/identity-service.md) for
the full operator narrative (anti-abuse knobs, key rotation, email
delivery).

#### Feature toggles & engine tuning

| Variable                                        | Default | Purpose                                                                                  |
|-------------------------------------------------|---------|------------------------------------------------------------------------------------------|
| `MEMQL_DEMO_MODE`                               | `false` | Affects webhook step behavior; used by demo deployments.                                 |
| `MEMQL_COGNITION_FIT_THRESHOLD`                 | `0.4`   | Float in `[0,1]`; cognition turn-fit cutoff. Higher = stricter "should I respond?" gate. |
| `MEMQL_CLASSIFICATION_SHORTCIRCUIT`             | `true`  | Deterministic messageClassification short-circuit (#1329): in a 1-human/1-agent space, a TEXT turn with no ambiguous @-addressing skips the classification LLM call and dispatches to the single agent. Set `false`/`0`/`off` to force every turn through the LLM classifier (A/B latency measurement, or to restore text-ack suppression in 1:1 spaces). Voice turns always use the LLM classifier regardless. |
| `MEMQL_MEMORY_ENGINE_MAX_RESULTS`               | `500`   | Per-query row cap.                                                                       |
| `MEMQL_MEMORY_ENGINE_MAX_WINDOW`                | `5000`  | Query optimizer lookahead window.                                                        |
| `MEMQL_MEMORY_ENGINE_CACHE_MAX_ITEMS`           | `1024`  | Concept-schema cache size.                                                               |
| `MEMQL_CACHE_MAX_TTL`                           | `0`     | CEILING in seconds on any resolved query result-cache TTL. `0` = **no clamp**, which is the default -- it does NOT disable caching and does NOT mean entries never expire (a hint-free pure read still caches for 60s). |
| `MEMQL_TOOL_LOOP_MAX_ITERATIONS`                | `120`   | Max AI tool-calling iterations per turn. Shared by the engine-level tool loop and the agent-node streaming loop. |
| `MEMQL_DSL_PATH`                                | unset   | Optional on-disk root for the .memql tree. When set and `<root>/<typeName>` exists, that DSL type reads from disk instead of the embedded copy. Per-type partial overrides supported. |
| `MEMQL_MESH_OUTBOX_RETENTION`                  | `24h`   | Max-age watermark (Go duration string) for the mesh delivery substrate's `mesh_outbox` rows and stale `mesh_cursor` rows; an hourly per-node sweep deletes rows older than this. `0` or negative disables the sweep; an unparsable value falls back to the default. `mesh_key_seq` is never swept (seq-restart hazard). |

#### STT / voice (only if Polyphon or streaming STT is enabled)

| Variable                       | Default          | Purpose                                                                          |
|--------------------------------|------------------|----------------------------------------------------------------------------------|
| `MEMQL_STT_PROVIDER`           | `openai-realtime` | `openai-realtime` / `openai-whisper`. |
| `MEMQL_STT_LANGUAGE`           | `en`             | Hard-pinned transcription language for the streaming chat-mic path (`AiTranscribeStreamStart`). Drives the OpenAI Realtime session config (`en`). Overrides any client-supplied `language_hint` -- pinning English is what stops the wrong/mixed-language + short-word-hallucination failure mode. |
| `MEMQL_STT_MIN_CONFIDENCE`     | `0.6`            | Floor a streaming FINAL transcript's confidence must clear to be emitted. OpenAI Realtime finals carry `1.0` and always pass, relying on server-VAD + the empty/denylist filters. Also gates a no-speech denylist of well-known silence hallucinations ("thank you", "thanks for watching", ...) so they're dropped only when confidence is low. `0` disables the confidence + denylist gates (empty-text drop still applies). |
| `MEMQL_OPENAI_REALTIME_MODEL`  | empty            | Realtime model id; falls back to `MEMQL_POLYPHON_OPENAI_ASR_MODEL`.                    |
| `MEMQL_POLYPHON_OPENAI_VAD_SILENCE_MS` | `600`          | Trailing-silence window (ms) the OpenAI server VAD requires before declaring end-of-utterance on the streaming ASR path. Lower = snappier finals; higher = better tolerance for mid-sentence pauses. See `docs/public/operate/voice-eou-tuning.md`. |
| `MEMQL_POLYPHON_VOICE_LANGUAGE` | `en`            | BCP-47 language for the voice-agent's ASR sessions (the realtime path narrows it to the ISO-639-1 primary subtag). Legacy name `POLYPHON_VOICE_LANGUAGE` still accepted (memql#3834). |
| `MEMQL_WHISPER_MODEL`          | `whisper-1`      | Used when `MEMQL_STT_PROVIDER=openai-whisper`.                                   |
| `MEMQL_POLYPHON_VOICE_PROVIDER`      | `openai`         | Voice provider for the `/memql/audio` WebSocket path. |
| `MEMQL_POLYPHON_OPENAI_ASR_MODEL`    | none             | OpenAI ASR model for the `/memql/audio` path.                                    |
| `MEMQL_POLYPHON_OPENAI_TTS_MODEL`    | none             | OpenAI TTS model for the `/memql/audio` path.                                    |
| `MEMQL_POLYPHON_OPENAI_TTS_VOICE`    | none             | OpenAI TTS voice (`alloy`, `echo`, `nova`, ...).                                 |
| `MEMQL_POLYPHON_PREDICTION_ENGINE_URL` | none           | External Polyphon prediction engine; absent = embedded engine.                   |
| `VOICE_AGENT_TOKEN`            | unset            | Identity-issued `class="voice_agent"` JWT the Go voice-agent presents on `MemqlService.Stream`. When empty the agent self-bootstraps via `/node/bootstrap` (dev). See `docs/public/operate/auth/voice-agent-jwt.md`. |
| `MEMQL_VOICE_EXECUTOR`         | `realtime`       | Go voice-agent executor: `realtime` (OpenAI gpt-realtime speech-to-speech, the default since #483) or `cascade` (OpenAI STT -> cognition -> OpenAI TTS). Realtime degrades cleanly to the cascade when its preconditions fail (persona build etc.), logging the reason -- so a fresh run uses realtime and there is no silent cascade surprise. Set `cascade` to opt out. The active executor is logged loudly at session start (`voice-agent voice executor: ...`). |
| `MEMQL_VOICE_ROOM_NAME`        | unset            | LiveKit room the Go voice-agent joins (MemQL convention: `polyphon-<spaceId>`). Falls back here when no `--room` flag is passed. |
| `MEMQL_REALTIME_NOISE_REDUCTION` | `far_field`    | Server-side input noise reduction on the realtime voice session (`audio.input.noise_reduction`): `far_field` (laptop/conference mics -- the documented mitigation for speaker-echo phantom turns), `near_field` (headsets), or `off` (field omitted). Filters audio BEFORE the model's VAD, reducing turn-detection false positives. Layered with the `MEMQL_REALTIME_VAD_THRESHOLD` energy gate and the transcript filters. See `docs/public/operate/voice-realtime-ga.md` (#1431). |
| `MEMQL_REALTIME_TRANSCRIPT_MIN_CONFIDENCE` | `-1.0` | Mean per-token logprob floor a realtime input-transcription FINAL must clear to reach chat (the session requests `item.input_audio_transcription.logprobs`; requires the `gpt-4o-transcribe` model family). Finals WITHOUT logprobs always pass -- the signal is intermittently missing and absence never drops a real utterance. Raise toward `-0.5` to gate harder; set very low (e.g. `-100`) to effectively disable. Composes with the #1199 hallucination denylist (#1431). |
| `MEMQL_AVATAR_VENDOR`          | `anam`           | Avatar vendor on the voice-agent side: `anam`, `simli`, or `none`.               |
| `MEMQL_ANAM_API_KEY`                 | unset            | Anam (CARA-3) API key. Required when avatar vendor=anam.                         |
| `MEMQL_SIMLI_API_KEY`                | unset            | Simli API key. Required when avatar vendor=simli.                                |

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
| `MEMQL_WS_DIAL_TIMEOUT_MS`            | `5000`             |
| `MEMQL_WS_WRITE_TIMEOUT_MS`           | `10000`            |
| `MEMQL_WS_MAX_CONCURRENT_REQUESTS`    | `4`                |
| `MEMQL_WS_MAX_MESSAGE_BYTES`          | `5242880` (5 MB)   |
| `MEMQL_WS_PING_INTERVAL_MS`           | `30000`            |

---

#### Logs (the log store)

Every node type persists its log lines to the `log_line` hypertable and the
MemQL OS Logs app reads them (epic memql#4893). Runbook: [Logs](logs.md).

| Variable                            | Default                       | Purpose                                                                                                                |
|-------------------------------------|-------------------------------|------------------------------------------------------------------------------------------------------------------------|
| `MEMQL_LOGS_LEVEL`                  | `info`                        | The store's floor on this node: `debug`, `info`, `warn`, `error` or `off`. The console handler's level is untouched, so a node keeps printing what it printed. |
| `MEMQL_LOGS_MAX_LINES_PER_SECOND`   | `2000`                        | Per-node, per-second cap on stored lines (clamped 10..100000). Beyond it a line is dropped and counted on `memql_logs_dropped_total{reason="rate"}`; the node writes one stored warning per minute naming the drops, so the gap is visible in the Logs app. |
| `MEMQL_LOGS_RETENTION_DAYS`         | `30`                          | Days of lines kept before the nightly `logsRetentionSweep` archives a day to blob storage and then deletes it (clamped 1..365). No archive, no delete: with no container the sweep keeps every line and says so. |
| `MEMQL_LOGS_ARCHIVE_CONTAINER`      | `MEMQL_AZURE_BLOB_CONTAINER`  | The blob container the archive objects (`logs/<day>/<nodeType>.ndjson.gz`) land in. Empty means no archive is configured. |

## Concept-stored config

This is the table to look at when you ask "where do I put a new API
key" or "where do I change the default model".

The authoritative manifest is
[`scripts/secrets/manifest.yaml`](../../../scripts/secrets/manifest.yaml).
Every entry in the manifest is a name `go run ./scripts/secrets seed`
will look for in the `--env-file` you point it at, and push into the
running MemQL when it finds a value. Names absent from the env file are
skipped; the manifest is the allow-list, not the source of values.

### Default global secrets (manifest)

Stored in `v1:platform:globalSecret`, sealed under `MEMQL_MASTER_KEY`.

| Name                          | Kind             | Purpose                                                                                                                                   |
|-------------------------------|------------------|-------------------------------------------------------------------------------------------------------------------------------------------|
| `MEMQL_OPENAI_API_KEY`              | `vendor_api_key` | Instance-wide OpenAI key. Used by chat / TTS / STT / Realtime providers unless a tenant overrides it.                                     |
| `MEMQL_ANTHROPIC_API_KEY`           | `vendor_api_key` | Instance-wide Anthropic key for Claude chat / vision providers.                                                                           |
| `MEMQL_IDENTITY_KEY_ENCRYPTION_KEY` | `integration`    | Master secret (>=16 bytes) wrapping the identity service's on-disk Ed25519 signing keypair. Required in production.                       |
| `MEMQL_EMAIL_AZURE_CLIENT_SECRET`   | `oauth_secret`   | Microsoft Graph client secret used by the **email integration**'s GraphSender. Legacy name `AZURE_CLIENT_SECRET` still accepted (fallback). |
| `MEMQL_ANAM_API_KEY`                | `integration`    | Anam avatar vendor key (server-side). Used by the direct/Guide avatar (`integrations/avatardirect`) and the voice-agent avatar.            |
| `MEMQL_SIMLI_API_KEY`               | `integration`    | Simli avatar vendor key (server-side). Used by the voice-agent avatar (direct-path Simli support lands in #293).                          |

### Default global variables (manifest)

Stored in `v1:platform:globalVariable`.

| Name                          | Default                              | Purpose                                                                                                                                |
|-------------------------------|--------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------|
| `MEMQL_IDENTITY_BASE_URL`           | none                                 | Public origin of the identity service (e.g. `https://auth.example.com`).                                                               |
| `MEMQL_IDENTITY_VERIFIER_BASE_URL`  | matches `MEMQL_IDENTITY_BASE_URL`          | Override only when internal-mesh routing differs from the public origin.                                                               |
| `MEMQL_EMAIL_AZURE_TENANT_ID`       | none                                 | Entra tenant id used by the **email integration**'s GraphSender: the tenant that HOSTS THE SENDER MAILBOX (the Microsoft 365 tenant), not necessarily the AKS subscription's tenant (memql#4226; see [identity-service.md](auth/identity-service.md#email-delivery)). Legacy name `AZURE_TENANT_ID` still accepted (fallback). |
| `MEMQL_EMAIL_AZURE_CLIENT_ID`       | none                                 | Azure AD application id used by the email integration's GraphSender. Legacy name `AZURE_CLIENT_ID` still accepted.                     |
| `MEMQL_EMAIL_SENDER`                | none                                 | Sender address for transactional mail (e.g. `no-reply@acme.com`). Legacy name `MAIL_SENDER` still accepted.                             |
| `MEMQL_EMAIL_FROM_NAME`             | `MemQL`                              | Display name in the From header. Legacy name `MAIL_FROM_NAME` still accepted.                                                          |

### Variables consumed by the product frontend

These aren't in the manifest yet -- operators add them by writing the
`v1:platform:globalVariable` row directly (the `setGlobalVariable`
mutation) -- but they're documented here because they live in that
concept and are read by the product frontend's runtime config layer
(its publicConfig whitelist):

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

The exact name on the MemQL side has to match the entry in the
frontend's publicConfig whitelist exactly. To add a new one: add it to
the whitelist, then either append it to the manifest and re-seed, or
call `setGlobalVariable(name: "...", value: "...")` directly.

### Per-tenant overrides

Anything in `v1:platform:globalSecret` / `v1:platform:globalVariable` can be
overridden per-tenant by writing the same `name` into
`v1:platform:partitionSecret` / `v1:platform:partitionVariable` with the tenant's partition
stamped on the row.

The resolver always tries the partition-scoped row first and falls
back to the global one. So a tenant with `MEMQL_OPENAI_API_KEY` in their
partition's `v1:platform:partitionSecret` will use their own key; everyone else
keeps using the platform default.

This is the BYOK ("bring your own key") path. The DSL surface is
`resolveSecret("MEMQL_OPENAI_API_KEY")` and `resolveVariable("...")`; see
`component/memql/sense/builtins.go` for the builtin docs.

---

## The env file the seeder reads

`go run ./scripts/secrets seed --env-file <path>` is the whole
concept-seeding surface. It reads a plain `KEY=value` env file, keeps
only the names the manifest lists, encrypts each secret under
`MEMQL_MASTER_KEY`, and upserts the row into
`v1:platform:globalSecret` / `v1:platform:globalVariable` over gRPC
against the running MemQL.

```bash
# Requires MEMQL_MASTER_KEY in the environment. Default endpoint is
# https://bff.memql.localhost:443; override with MEMQL_GRPC_ENDPOINT.
go run ./scripts/secrets seed --env-file .env
go run ./scripts/secrets health     # gRPC handshake check: prints "ok"
```

The env file is **operator-local and gitignored**. It is a stash of
values, not a schema: the manifest decides which names mean anything,
and the row ids are derived (`secret-global-<slug>` /
`var-global-<slug>`) so a re-seed overwrites in place rather than
producing a second row.

> **This section used to describe a `~/.memql/dev-secrets.yaml` stash**
> with its own `masterKey:` / `secrets:` / `variables:` schema, plus
> init / list / export / one-off-set commands to manage it. The seeder
> reads a `.env` file and has never had those subcommands; nothing in
> the tree reads that yaml path (memql#4405).

In a cloud install:

- `MEMQL_MASTER_KEY` is set explicitly on the deploy target.
- Secrets and variables are seeded once against the cluster's gRPC
  endpoint, after which they live in the database.

### Where the env file sits in the bootstrap chain

The bootstrap values (including `MEMQL_MASTER_KEY`) are seeded onto the
`memql-secrets` Secret by `make up` (`scripts/k3d/seed-secrets.sh`) and
reach the pods via `envFrom`, so every pod has the key in env from first
boot. The env file is only used by the concept-seeding step:

1. `go run ./scripts/secrets seed --env-file <path>` reads it.
2. It encrypts each manifest-listed entry under the master key
   (resolved from the seeded `MEMQL_MASTER_KEY` env) and upserts the row
   into the right concept over gRPC against the running MemQL.

### Backing concept state up before a repave

There is no export command. `make down && make up` recreates the
database, so anything written directly with `setGlobalSecret` /
`setGlobalVariable` and never added to your env file is gone. Keep the
env file as the record: seed from it, and add to it anything you want to
survive a repave.

### Master-key resolution order (in process)

`component/secret/encryption.go` reads `MEMQL_MASTER_KEY` from the OS
env at first encrypt/decrypt call. There is no fallback. If absent
when an encrypted secret is accessed, the process logs a fatal error.
The seeded k8s Secret puts the key into the env before the pod's gRPC
server starts. Inside the pod, the value is just an env var.

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
- `MEMQL_WORKER_PEERS` -- set on every node that dials peers: BFF, and
  cognition + planner for their agent-only narrowing, and agent for its
  workbench-only narrowing under `MEMQL_WORKBENCH_REMOTE=1`. Comma-
  separated `type=address` list. First-boot seed only; once peers
  register themselves into `v1:cluster:node` (a global concept),
  DB-based discovery takes over. An entry whose type is not dialable
  (`agent`, `cognition`, `identity`, `planner`, `voice`, `workbench`)
  or is otherwise unparseable is ignored, and the node logs a WARN
  naming the entry -- so a typo in the seed list is visible in the boot
  log rather than presenting as a peer that never appears (memql#3450).
- `MEMQL_WORKBENCH_REMOTE` -- set on the agent to require that workbench
  tool calls run on a workbench node. It is an **assertion**, not a
  preference: with it set and no reachable workbench peer, a workbench
  call is REFUSED (`no_workbench_peer`) rather than run on the agent's
  own disk (memql#3506). Silently degrading is what hid memql#3450 for
  its whole life.
- `MEMQL_WORKBENCH_LOCAL_FALLBACK` -- the explicit opt-in that restores
  the old degrade-to-local behaviour for an unreachable workbench. Off by
  default, which is the whole safety property: a fallback reachable by
  the *absence* of configuration fires exactly when nobody meant it to.
  See [workbench-runbook.md](workbench-runbook.md).

Both are bootstrap envelope vars -- they have to be in the env
before the gRPC server starts.

`deploy/k8s/overlays/local` (over `deploy/k8s/base`) has full worked
examples -- the engine mesh (mcp + cognition + agent + planner +
voice + workbench + voice-agent), identity, and the local infra
(Postgres + Azurite). The product `bff` head and SPA live in the
downstream product pack's overlay, not here.

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

If the value has to be available *before* MemQL connects to its
database (i.e. it controls how MemQL connects), it's a bootstrap
envelope var, not a concept entry. There's a strong bias against
adding new entries to the bootstrap envelope -- it requires a
deploy-config change every time it rotates.

### Adding a global secret

1. Append a row to `scripts/secrets/manifest.yaml` under `secrets:`.
2. Put `YOUR_NAME=<value>` in the env file you seed from.
3. `go run ./scripts/secrets seed --env-file <path>`.
4. Reference it from a provider/integration via `env("YOUR_NAME")` in
   `.memql` or `os.Getenv("YOUR_NAME")` in Go (the resolver chain
   works for both).

### Adding a global variable

1. Append a row to `scripts/secrets/manifest.yaml` under `variables:`
   and seed it the same way, *or* write the row directly with the
   `setGlobalVariable(name: "YOUR_NAME", value: "...")` mutation.
2. The DSL resolver returns it from `resolveVariable("YOUR_NAME")` or
   the same `env()` chain in provider auth.

### Adding a per-tenant (partition-scoped) entry

Call `setPartitionSecret` / `setPartitionVariable` directly. The
seeder writes global rows only -- the manifest carries no
partition-scoped entries -- and the partition dimension itself is
being retired (#56), so treat these two concepts as legacy surface
rather than the place to put new config.

---

## Reference: file paths

| File                                                                          | What it tells you                                                              |
|-------------------------------------------------------------------------------|--------------------------------------------------------------------------------|
| [`scripts/secrets/manifest.yaml`](../../../scripts/secrets/manifest.yaml)        | Authoritative list of dev-bootstrap secrets + variables.                       |
| [`scripts/secrets/main.go`](../../../scripts/secrets/main.go)                    | The concept-seeding CLI: `seed --env-file <path>` and `health`. Nothing wraps it in a make target. |
| [`scripts/k3d/seed-secrets.sh`](../../../scripts/k3d/seed-secrets.sh)            | Seeds the bootstrap envelope into k8s Secrets (run by `make up` / `make secrets`). |
| `dsl/platform/concepts.memql`                                                 | Schemas for global + partition-scoped secrets and variables.                   |
| `component/secret/encryption.go`                                              | NaCl secretbox + `MEMQL_MASTER_KEY` resolution.                                |
| `component/memql/ai_providers.go` (`resolveAuthPlaceholders`)                 | Provider-auth resolver (the env() / placeholder chain).                        |
| `component/memql/sense/builtins.go`                                           | DSL surface (`resolveSecret`, `resolveVariable`).                              |
| `component/config/config.go`                                                  | One-stop list of bootstrap env-var reads.                                      |
| `component/database/database.go`                                              | Database-tier env reads (DSN + tuning).                                        |
| `component/identity/config.go`                                                | Identity service env reads (the binary itself).                                |
| `component/identity/verifier/config.go`                                       | Per-node verifier env reads (bff/voice/cognition/agent/planner).               |
| `component/node/identity.go`                                                  | Node-identity env reads.                                                       |
| `component/server/memqlws/env.go`                                             | WebSocket tuning env reads.                                                    |
| [`deploy/k8s/overlays/local`](../../../deploy/k8s/overlays/local)               | Worked example of every required bootstrap env var for the local cluster.       |

---

## Operational notes

### Rotating a secret

Put the new value in your env file under the same name and re-seed:

```bash
# Requires MEMQL_MASTER_KEY. For a cloud install, point the same command
# at that cluster by setting MEMQL_GRPC_ENDPOINT in the calling shell.
go run ./scripts/secrets seed --env-file .env
```

A single row can also be written directly with the `setGlobalSecret`
mutation, which is what the seeder itself calls.

The old row is soft-deleted (`active=false`); `lastUsedAt` /
`rotatedAt` get stamped on the new row. The next decrypt picks the
new value; nothing else has to restart.

#### The one exception: the campaign unsubscribe signing key

`MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET` does not rotate like the keys above,
because what it signs lives **outside** the cluster: an RFC 8058 unsubscribe
link sits in the recipient's mailbox for as long as they keep the message.
Replacing the value on its own invalidates every such link ever sent, which is
a compliance failure rather than a degraded feature.

It is therefore a **two-variable** rotation (memql#3458). Set
`MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET_PREVIOUS` to the outgoing value in the same
change, and then leave it set — it is a permanent second *reader* key, not a
migration window. An old link keeps working forever, until a **second**
rotation retires the key that signed it; the window is counted in rotations,
not days. The worker warns at boot if this deployment has already sent campaign
mail while holding only one key.

Full procedure and reasoning:
[campaign-sending.md](campaign-sending.md#rotating-the-unsubscribe-signing-key).

### Backing up state before a wipe

There is no export command, so the env file has to BE the backup. Before
any `make down && make up` (or `make up-refresh`), make sure every value
you care about is in the file you seed from -- a row written directly with
`setGlobalSecret` / `setGlobalVariable` and never added there does not
survive the repave.

> This section used to document an export subcommand that pulled every
> active row back out and merged it into a local yaml. It has never
> existed (memql#4405).

### "Why is my provider giving 'no value' errors?"

Check the resolver chain in order:

1. Is the row in `v1:platform:globalSecret` /
   `v1:platform:globalVariable`? Read it back through the
   `globalSecret` / `globalVariable` query, e.g. in DSL
   `globalVariable({name: "MEMQL_OPENAI_API_KEY"})`.
2. Does the running MemQL have `MEMQL_MASTER_KEY` set in env?
3. Is the master key the **same one** that encrypted the row? If
   you regenerated it, the existing rows are unreadable -- re-seed with
   `go run ./scripts/secrets seed --env-file <path>` to overwrite under
   the new key.

### Local override without touching the env file

`setGlobalSecret` / `setGlobalVariable` write directly to the running
MemQL without modifying the env file. Useful for one-off experiments.
Note that the next `make down && make up` recreates the database, and a
re-seed replaces the value with whatever is in the env file -- so put it
there too if you want it to last.

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
