---
audience: internal
status: historical
area: ops
sinceVersion: "0.9.6"
owner: znas
---

# Stale defined-but-unused env-var cleanup (Epic 7 / 7.4, memql#2107)

Historical record of the workstream-7.4 sweep that removed env vars
which were **defined/set** somewhere in the deploy/config surface but
**read nowhere** in code and **absent from the registry**
(`scripts/secrets/manifest.yaml`).

## Method

1. Built the *defined* set by scanning every env name (uppercase
   tokens) across `deploy/k8s/**`, `.env.example`,
   `deploy/k8s/base/secret.example.yaml`,
   `scripts/k3d/seed-secrets.sh`, `docker/**`, and `releases/*`.
2. Built the *used* set from three sources:
   - `GOWORK=off go run ./cmd/envscan -list` (every statically
     detectable code read).
   - the registry names in `scripts/secrets/manifest.yaml` (288
     entries).
   - the 90 legacy-alias VALUES in
     `component/genesis/legacyalias.go`.
3. `defined MINUS used` produced 88 raw candidates.
4. Reviewed each candidate against the full read surface --
   crucially including the **prefix-composed** readers that
   `cmd/envscan` cannot see (it keys off literal `os.Getenv` /
   `env*(...)` / DSL `env(...)` args only):
   - `component/database/database.go` `DatabaseEnvKeys` under the
     `MEMORY_NODES_DATABASE_` prefix
     (`component/database/memory-nodes/database.go`).
   - `component/server/env.go` `ServerEnvKeys` under the `SERVER_`
     prefix (`component/server/server.go`).
   - `component/service/service.go` `ServiceEnvKeys` under the
     `SERVICE_` prefix (wired from `main.go` + `app/app.go`).
   - the `voice-agent` config's `get()` / `getRequired()` helper
     reads (`integrations/voice/agent/config.go`,
     `bootstrap.go`) -- also invisible to the literal-arg scanner.
   - inline `sh -c` scripts inside k8s `command:` blocks (e.g.
     `deploy/k8s/base/conn-monitor-cronjob.yaml`).

## Removed

One genuine orphan was found and removed.

### `MEMORY_NODES_DATABASE_AUTO_MIGRATE`

A redundant duplicate of `MEMORY_NODES_DATABASE_MIGRATE_ON_START`.
The DB env loader's `DatabaseEnvKeys` struct
(`component/database/database.go`) defines only
`MigrateOnStart: "MIGRATE_ON_START"` -- there is **no** `AUTO_MIGRATE`
key, so the `MEMORY_NODES_DATABASE_` prefix reader never resolves it,
and a repo-wide grep finds zero readers in any language. It was set
alongside the real `MIGRATE_ON_START` knob in every node manifest as
a no-op.

Removed the env entry from:

- `deploy/k8s/base/agent.yaml`
- `deploy/k8s/base/workbench.yaml`
- `deploy/k8s/base/bff-bluegreen.yaml` (2 occurrences)
- `deploy/k8s/base/planner.yaml`
- `deploy/k8s/base/cognition.yaml`
- `deploy/k8s/base/bff.yaml`
- `deploy/k8s/base/voice.yaml`
- `deploy/k8s/base/mcp.yaml`
- `deploy/k8s/base/identity.yaml` (was `true`)
- `deploy/k8s/base/migrate-job.yaml` (was `true`)

And updated the narrative references that paired it with the real
knob:

- `deploy/k8s/base/README.md` (migrations-run-once section)
- `deploy/k8s/base/identity.yaml` (header + inline comments)
- `scripts/deploy/aks-apply.sh` (header comment)
- `scripts/deploy/tiger-provision.sh` (operator note `echo`)

Migration gating is unchanged: `MEMORY_NODES_DATABASE_MIGRATE_ON_START`
remains the single live knob (identity + migrate-job `true`, all
other nodes `false`), exactly as before.

## Candidates reviewed and intentionally KEPT

Every other candidate is genuinely consumed or has a clear purpose
and was left in place:

- **Prefix-composed reader keys** (read, just not as literals):
  `SERVER_ADDRESS`, `SERVER_ALLOWED_ORIGINS`, `SERVICE_NAME`,
  `MEMORY_NODES_DATABASE_DIRECT_DSN` / `_MAX_OPEN_CONNS` /
  `_MAX_IDLE_CONNS` / `_MIGRATE_ON_START`. The bare
  `MAX_OPEN_CONNS` / `MAX_IDLE_CONNS` tokens appear only in
  documentation comments (`deploy/k8s/base/db-pool-config.yaml`
  explicitly notes the bare names are NOT read).
- **voice-agent helper reads** (`get`/`getRequired`, invisible to
  the literal-arg scanner): `MEMQL_GRPC_ADDR`, `MEMQL_AVATAR_VENDOR`,
  `MEMQL_VOICE_EXECUTOR`, `MEMQL_VOICE_AUTOJOIN`,
  `MEMQL_REALTIME_VAD_SILENCE_DURATION_MS`,
  `MEMQL_VOICE_AGENT_INSTANCE_ID`, `VOICE_AGENT_TOKEN`.
- **Other live Go reads** the registry omits by design but code
  consumes: `MEMQL_GENESIS_AUTOLOAD`, `MEMQL_GENESIS_B64`,
  `MEMQL_MASTER_KEY`, `MEMQL_HTTP_TLS_CA_FILE`, `MEMQL_DB_APP_NAME`,
  `MEMQL_WORKBENCH_ROOT`.
- **Inline-cronjob-script vars** (`conn-monitor-cronjob.yaml`
  `command:`): `WARN_PCT`, `CRIT_PCT`, `LEAK_MIN`,
  `PGCONNECT_TIMEOUT`, `DIRECT_DSN`.
- **Platform / runtime infra**: `SSL_CERT_DIR` (Go stdlib TLS cert
  dir; the comment documents it as the 0.9.0-image fallback for
  `MEMQL_HTTP_TLS_CA_FILE`), `NODE_IP`, `KUBELET_OR_WORKLOAD_IDENTITY_CLIENT_ID`,
  `AZURE_BLOB_CONNECTION_STRING`, `AZURE_TENANT_ID`.
- **Database / Postgres infra**: `POSTGRES_DB`, `POSTGRES_USER`,
  `POSTGRES_PASSWORD`, `POOLER_DSN`, `DATABASE_DSN`,
  `AZURITE_ACCOUNT` / `_CONN` / `_KEY`.
- **LiveKit / SIP / Telephony container infra**: `LIVEKIT_KEYS`,
  `LIVEKIT_API_KEY`, `LIVEKIT_API_SECRET`, `LIVEKIT_URL`,
  `LIVEKIT_PUBLIC_URL`, `LIVEKIT_PORT`, `SIP_LK_API_KEY`,
  `SIP_LK_API_SECRET`, `TELNYX_API_KEY`, `TELNYX_CONNECTION_ID`,
  `TELNYX_OUTBOUND_PROFILE_ID`.
- **Product frontend (SPA) container** (its own manifest, a
  separate image/runtime -- removing these would break the
  frontend): `APP_PORT`, `FRONTEND_PORT`, `NODE_ENV`,
  `CORS_ALLOWED_ORIGIN`, `NODE_AUTH_TOKEN`, `VITE_IDENTITY_BASE_URL`,
  `VITE_IDENTITY_CLIENT_ID`, `VITE_MEMQL_API_URL`, `VITE_MEMQL_WS_URL`.
- **Shell-script locals / operator knobs** (`seed-secrets.sh` and
  other `scripts/`): `BASH_SOURCE`, `SCRIPT_DIR`, `REPO_ROOT`,
  `CLUSTER_NAME`, `MEMQL_K3D_CLUSTER`, `MEMQL_K3D_NAMESPACE`,
  `MEMQL_GENESIS_FILE`, `LOCAL_DB_USER` / `_PASSWORD` / `_NAME`,
  `MEMQL_LOCAL_DB_USER` / `_PASSWORD` / `_NAME`.
- **Dockerfile build args**: `BUILD_TAGS`, `CGO_ENABLED`,
  `TAILWIND_VERSION`.
- **Placeholder tokens** in `secret.example.yaml` (not real env
  names): `REPLACE_WITH_64_HEX_CHAR_MASTER_KEY`,
  `REPLACE_WITH_BASE64_OF_SEALED_GENESIS_ENVELOPE`,
  `REPLACE_WITH_TIGER_CLOUD_DIRECT_DSN`,
  `REPLACE_WITH_TIGER_CLOUD_POOLER_DSN`.

### Candidate -- needs human confirmation

- `MEMQL_LOG_LEVEL` -- appears only as a **commented** example line in
  `.env.example` (`# MEMQL_LOG_LEVEL=debug`). The live logger keys are
  `CAPABILITIES_LOGGING_LOG_LEVEL` / `LOGGER_LEVEL` / `LOG_LEVEL`
  (`component/component.go`), so `MEMQL_LOG_LEVEL` is not actually read
  under that name. Left in place because it is an inert documentation
  example, not an active definition; flagged here in case the intent
  was to wire `MEMQL_LOG_LEVEL` as the canonical override.

## Verification

- `GOWORK=off go run ./cmd/envscan -check` -- OK (190 reads, 288
  registry entries, no drift).
- `GOWORK=off go build ./...` -- OK.
- `kubectl kustomize` renders cleanly for `deploy/k8s/base` and the
  `local` / `staging` / `prod` overlays.
