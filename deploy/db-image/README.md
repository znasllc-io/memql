# memQL database operand image

PostgreSQL 16 + TimescaleDB Community + pgvector — the container CloudNativePG
runs as a Postgres instance in **every** environment (local k3d, staging,
production), digest-pinned per overlay exactly like the engine images.

Epic memql#3842, task memql#3844.

| | |
|---|---|
| Base | `ghcr.io/cloudnative-pg/postgresql:16.15-standard-bookworm`, digest-pinned |
| PostgreSQL | 16 |
| TimescaleDB | 2.29.1 (current) + 2.28.3 (N-1) — Community/TSL build |
| pgvector | ships in the `standard` base flavor (0.8.6) |
| Runs as | uid 26 |

## Why these choices

**`standard`, not `minimal` or `system`.** CloudNativePG's `standard` flavor is
`minimal` + PGAudit + Postgres Failover Slots + **pgvector** + all locales + JIT,
and it is the flavor CNPG documents as the pairing for the Barman Cloud
**plugin**. The `system` images carry in-tree Barman binaries and are deprecated.
memql#3845 uses the plugin, so `standard` is the correct half of that pair — and
pgvector needs no install step here because it is already present.

**Community (TSL), not Apache.** The schema requires continuous aggregates
(`code_invocation_1m` / `_1h`), compression policies, and a retention policy —
all verified in `component/database/memory-nodes/migrations/`, and all absent
from the Apache-2.0 build. That is also why Azure's managed PostgreSQL cannot
host this schema. Licensing posture:
[timescaledb-license-compliance.md](../../docs/internal/ops/timescaledb-license-compliance.md).

**Two versioned `.so` files.** Postgres loads `timescaledb.so`, a thin loader
that dispatches to the versioned `timescaledb-<catalog version>.so` matching
what the **database catalog** currently says. An upgrade is two steps that
cannot be atomic:

1. every instance restarts onto the new image while the catalog still says the
   **old** version — so the old library must still be on disk, or the instance
   comes up unable to serve a query;
2. `ALTER EXTENSION timescaledb UPDATE` runs (CNPG does this when
   `Database.spec.extensions` moves), and only then is the new one live.

Between (1) and (2) — a rolling restart, so minutes, longer if paused — a
single-version image is a hard outage. Carrying N and N-1 makes step (1) a
no-op.

## Bumping TimescaleDB — two PRs, deliberately

| PR | What moves | Effect |
|---|---|---|
| 1 | `TIMESCALEDB_VERSION` here (old one becomes `TIMESCALEDB_PREVIOUS_VERSION`); build; pin the new digest in the overlays | Instances restart. Catalog untouched. |
| 2 | The version in each overlay's `Database.spec.extensions` | CNPG runs `ALTER EXTENSION`. |

Doing both in one PR asks the catalog to move while pods are still rolling.

`TestDatabaseImageVersionsAgreeEverywhere` (`scripts/ci/`) fails the build if the
version pair drifts between the Dockerfile, the workflow, the build script, and
the smoke test — one fact with four copies is exactly the drift no reviewer
catches.

## Building

**Release** — GitHub build server only
(`.github/workflows/build-db-image.yml`, `workflow_dispatch`). OIDC → ACR, with
a GHCR mirror so a local install can pull without Azure credentials. The smoke
test **gates the push**: the image is built, loaded, exercised, and only then
pushed. An image that reached ACR broken is pinnable by an overlay, and an
Apache-build regression does not present as a failed pull — it presents as a
migration failing much later with an error naming a missing function.

**Local development** — never for deploys:

```bash
make db-image                 # build + smoke test
make db-image IMPORT=1        # also import into the local k3d cluster
make db-image SMOKE=0         # skip the smoke test (fast rebuild)
```

Both paths drive the same Dockerfile and the same smoke test, which is what
makes the local image a faithful stand-in rather than a lookalike.

## What the smoke test asserts

`./smoke-test.sh <image-ref> [pg-major] [current] [previous]`

1. Both extensions create.
2. `SHOW timescaledb.license` = `timescale`. An Apache build installs cleanly
   and creates the extension happily — it fails *later*, at the first migration.
   This is the one check that separates the two builds up front.
3. Continuous aggregate + compression + retention actually run. Not a version
   string, but the three features the schema requires.
4. **The N-1 → N upgrade choreography**: create the extension *at* the previous
   minor, serve a real query on it (proving the old `.so` is present and
   reachable through the loader), then `ALTER EXTENSION … UPDATE`.

CNPG operand images are driven by the instance manager rather than an entrypoint
that starts Postgres, so the test brings its own server up with `initdb` +
`pg_ctl` on a unix socket.
