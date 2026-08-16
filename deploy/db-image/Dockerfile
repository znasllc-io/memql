# syntax=docker/dockerfile:1
#
# memQL database operand image -- PostgreSQL 16 + TimescaleDB Community + pgvector
# ==============================================================================
#
# The single database artifact every environment runs (epic memql#3842, task
# memql#3844). CloudNativePG calls this an "operand" image: the container the
# operator runs as a Postgres instance. Local k3d, staging, and production all
# run THIS image, digest-pinned per overlay exactly like the engine images --
# which is what makes the database layer stop being the one place environment
# parity bends.
#
# VERSION MATRIX
# --------------
#   PostgreSQL          16          (CNPG base, see CNPG_BASE below)
#   TimescaleDB         2.29.1      Community/TSL build, current
#   TimescaleDB         2.28.3      Community/TSL build, previous minor
#   pgvector            ships in the CNPG `standard` flavor -- not installed here
#
# WHY THE `standard` FLAVOR. CloudNativePG publishes `minimal`, `standard`, and
# the deprecated `system` images. `standard` = `minimal` + PGAudit + Postgres
# Failover Slots + **pgvector** + all locales + LLVM JIT, and is the flavor CNPG
# documents as the pairing for the Barman Cloud PLUGIN (`system` images carry
# in-tree Barman binaries and are deprecated -- memql#3845 uses the plugin, so
# `standard` is the correct half of that pair). pgvector therefore needs no
# install step here; it is already present, and the workflow's smoke test
# asserts it rather than trusting this comment.
#
# WHY COMMUNITY (TSL) AND NOT APACHE. The schema requires continuous aggregates
# (`code_invocation_1m` / `_1h`), compression policies, and a retention policy --
# all verified in component/database/memory-nodes/migrations/, and all absent
# from the Apache-2.0 build of TimescaleDB. That is also why Azure's managed
# PostgreSQL cannot host this schema. Licensing posture, and the reason no
# public file may call memQL a database:
# docs/internal/ops/timescaledb-license-compliance.md.
#
# WHY TWO TimescaleDB `.so` FILES -- the upgrade choreography
# -----------------------------------------------------------
# Postgres loads `timescaledb.so` (a thin loader), which dispatches to the
# VERSIONED `timescaledb-<catalog version>.so` matching what the DATABASE
# CATALOG currently says the extension version is. An upgrade is two steps that
# cannot be atomic:
#
#   1. every instance restarts onto the new image, while the catalog still says
#      the OLD version -- so the OLD versioned .so must still be on disk, or the
#      instance comes up unable to serve a single query;
#   2. `ALTER EXTENSION timescaledb UPDATE` runs (CNPG does this for you when
#      `Database.spec.extensions` moves), and only then is the new one live.
#
# Between (1) and (2) -- which spans a rolling restart, so minutes, and longer
# if it is paused -- a single-version image is a hard outage. Carrying N and N-1
# makes step (1) a no-op. This is the whole reason for the stash-and-restore
# dance in the RUN below, and it is asserted at BUILD time (the `test -f` lines)
# rather than left to be discovered during an upgrade.
#
# BUMPING TimescaleDB is a two-PR flow, deliberately:
#   PR 1 -- bump TIMESCALEDB_VERSION here (old becomes TIMESCALEDB_PREVIOUS_
#           VERSION), build, and pin the new digest in the overlays. Instances
#           restart; the catalog is untouched.
#   PR 2 -- bump the version in each overlay's `Database.spec.extensions`. CNPG
#           runs ALTER EXTENSION.
# Doing both in one PR asks the catalog to move while pods are still rolling.
#
# THE IMAGE TAG IS PARSED, NOT DECORATIVE
# ----------------------------------------
# CloudNativePG's Cluster webhook reads the tag on `spec.imageName` to derive
# the PostgreSQL major version -- it needs that to refuse an accidental major
# upgrade -- and REJECTS a tag it cannot parse. So the tag must BEGIN with the
# major. Measured against the v1.30.0 webhook:
#
#   16   16.15   16-dev   16.15-dev   16.15-ts2.29.1   16.15-standard-bookworm
#        -> accepted
#   dev      -> rejected: `spec.imageName: Invalid value: "memql-db:dev": invalid version tag`
#   v16.15   -> rejected, same message (the leading `v` is not tolerated)
#   0.1.0    -> rejected: "Unsupported PostgreSQL version" (parsed as major 0)
#
# This is why the local build tags `16-dev` rather than `dev`, and why a release
# tag reads like `16.15-timescaledb-2.29.1`. A tag that names only the product
# version builds an image no Cluster can reference, and the failure surfaces at
# `kubectl apply` during a bring-up rather than at build time.
#
# BUILD
#   Release (GitHub build server -> ACR):  .github/workflows/build-db-image.yml
#   Local dev (k3d):                       make db-image   [ -> scripts/db-image/build.sh ]
#
# Deployable images are built on the build server, never an operator laptop
# (CLAUDE.md, "Image builds"). The local path exists only to put the same
# Dockerfile into k3d for development.

# Digest-pinned for reproducibility; the tag is carried alongside so a human can
# read which release it is. Override to move the PostgreSQL minor.
ARG CNPG_BASE=ghcr.io/cloudnative-pg/postgresql:16.15-standard-bookworm@sha256:185531d51d94b1b7a6a530e92cc312e897132e1970a98f5bce896bec17a4c197

# ------------------------------------------------------------------------------
# Stage 1 -- fetch and dearmor the Timescale repository signing key.
#
# A separate stage so the FINAL image never gains `curl` and `gnupg`. Installing
# them into a production database image to fetch one file, then leaving them
# behind, adds reachable tooling to the most sensitive container in the cluster;
# purging them afterwards risks removing something the base shipped. Copying a
# single keyring file in avoids both.
# ------------------------------------------------------------------------------
FROM debian:bookworm-slim AS gpgkey

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl gnupg; \
    curl -fsSL https://packagecloud.io/timescale/timescaledb/gpgkey \
        | gpg --dearmor -o /timescaledb.gpg; \
    test -s /timescaledb.gpg

# ------------------------------------------------------------------------------
# Stage 2 -- the operand image.
# ------------------------------------------------------------------------------
FROM ${CNPG_BASE}

ARG PG_MAJOR=16
ARG TIMESCALEDB_VERSION=2.29.1
ARG TIMESCALEDB_PREVIOUS_VERSION=2.28.3

COPY --from=gpgkey /timescaledb.gpg /usr/share/keyrings/timescaledb.gpg

USER root

RUN set -eux; \
    codename="$(. /etc/os-release && echo "$VERSION_CODENAME")"; \
    echo "deb [signed-by=/usr/share/keyrings/timescaledb.gpg] https://packagecloud.io/timescale/timescaledb/debian/ ${codename} main" \
        > /etc/apt/sources.list.d/timescaledb.list; \
    apt-get update; \
    \
    pkg="timescaledb-2-postgresql-${PG_MAJOR}"; \
    # Resolve each upstream minor to its FULL Debian version. The packages carry
    # a distro+build suffix (2.29.1~debian12-1614) whose trailing number moves
    # on rebuilds, so pinning the literal string here would rot; resolving it
    # keeps the pin exact for THIS build while surviving upstream rebuilds. An
    # unresolvable version fails the build loudly rather than silently taking
    # whatever is newest.
    resolve() { apt-cache madison "$pkg" | awk -v p="$1~" 'index($3, p) == 1 { print $3; exit }'; }; \
    cur="$(resolve "${TIMESCALEDB_VERSION}")"; \
    prev="$(resolve "${TIMESCALEDB_PREVIOUS_VERSION}")"; \
    if [ -z "$cur" ]; then echo "ERROR: TimescaleDB ${TIMESCALEDB_VERSION} not available for ${codename}/pg${PG_MAJOR}" >&2; exit 1; fi; \
    if [ -z "$prev" ]; then echo "ERROR: TimescaleDB ${TIMESCALEDB_PREVIOUS_VERSION} not available for ${codename}/pg${PG_MAJOR}" >&2; exit 1; fi; \
    echo "INFO:  current=${cur} previous=${prev}"; \
    \
    # Install the PREVIOUS minor first and stash its versioned .so, because
    # installing the current one over it removes the old library as part of the
    # package upgrade. The CURRENT package supplies the N-1 -> N update SQL, so
    # only the .so needs preserving.
    apt-get install -y --no-install-recommends "${pkg}=${prev}"; \
    cp "/usr/lib/postgresql/${PG_MAJOR}/lib/timescaledb-${TIMESCALEDB_PREVIOUS_VERSION}.so" /tmp/timescaledb-previous.so; \
    \
    apt-get install -y --no-install-recommends "${pkg}=${cur}"; \
    mv /tmp/timescaledb-previous.so "/usr/lib/postgresql/${PG_MAJOR}/lib/timescaledb-${TIMESCALEDB_PREVIOUS_VERSION}.so"; \
    \
    # The acceptance criterion, asserted here rather than hoped for. A missing
    # N-1 library is invisible until an upgrade, at which point it is an outage.
    test -f "/usr/lib/postgresql/${PG_MAJOR}/lib/timescaledb.so"; \
    test -f "/usr/lib/postgresql/${PG_MAJOR}/lib/timescaledb-${TIMESCALEDB_VERSION}.so"; \
    test -f "/usr/lib/postgresql/${PG_MAJOR}/lib/timescaledb-${TIMESCALEDB_PREVIOUS_VERSION}.so"; \
    # pgvector comes from the `standard` base; assert it rather than assume it,
    # so a base-flavor change (standard -> minimal) fails here and not at the
    # first CREATE EXTENSION vector in a live cluster.
    test -f "/usr/share/postgresql/${PG_MAJOR}/extension/vector.control"; \
    \
    rm -rf /var/lib/apt/lists/*

# CNPG operand images run as uid 26 (postgres). Restored explicitly: the RUN
# above needed root, and leaving the image rooted would hand every database pod
# in every environment a root process.
USER 26

LABEL org.opencontainers.image.title="memql-db" \
      org.opencontainers.image.description="PostgreSQL 16 + TimescaleDB Community + pgvector operand image for CloudNativePG" \
      org.opencontainers.image.source="https://github.com/znasllc-io/memql" \
      io.memql.postgres.major="16" \
      io.memql.timescaledb.version="2.29.1" \
      io.memql.timescaledb.previous="2.28.3"
