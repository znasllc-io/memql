# syntax=docker/dockerfile:1
#
# MemQL pipeline toolchain image -- the container a pipeline step runs in
# =======================================================================
#
# Epic memql#5478, issue memql#5496. A pipeline step is a Kubernetes Job whose
# main container runs the manifest's `pipeline.image`, with the step's shell
# command, in a fresh checkout at /workspace. For this repository's own
# pipeline that image is THIS one, named by digest in memql-package.yaml, so it
# carries what this repository's CI steps reach for:
#
#   Go        1.27.1   go.work's `toolchain` line, which overrides every go.mod
#   Node      22 LTS   node:22-bookworm-slim
#   protoc    33.4     scripts/dev/proto-gen.sh's PROTOC_VERSION
#   git, make, bash, curl, tar, gzip, unzip, jq, python3 (with PyYAML),
#   coreutils, ca-certificates, and postgresql-client (psql, pg_isready)
#
# The Postgres a db-gated step talks to is NOT built here. It is the
# ci-timescaledb mirror ci.yml already pins (TimescaleDB + pgvector), run as
# the step's sidecar, so the pipeline and the GitHub lanes test against one
# database image.
#
# THE VERSIONS ARE THE REPOSITORY'S, NOT THIS FILE'S. Go and protoc are written
# down where the repository pins them and again below, and
# scripts/ci/toolchain_image_workflow_test.go fails when the copies disagree:
# `toolchain` is a floor, so a newer Go here would be used as found and the
# pipeline would test on a Go nothing else runs. A bump moves both copies in
# one change; then the image is rebuilt and memql-package.yaml re-pinned,
# because the PINNED digest is what a pipeline runs, not this file.
#
# EVERY IMAGE IS A FROM LINE. Go and Node are copied out of their official
# images by STAGE name, never by `COPY --from=<image>`: Dependabot updates FROM
# lines and cannot see an image named inside a COPY, so a bump would be partial
# by construction (scripts/ci/spa_image_wiring_test.go records that split in
# the root Dockerfile). Node is copied rather than apt-installed because Debian
# 12 ships Node 18.
#
# THE USER. Steps run as uid 1000 (`memql`), never as root: least privilege
# for a command whose text is whatever the manifest says. USER is NUMERIC
# because Kubernetes can only verify runAsNonRoot against a number. /workspace
# (the checkout) and /cache (the go and npm cache volume) are writable by it
# when nothing is mounted there; in a Job both are volumes.
#
# git TRUSTS /workspace. The checkout is made by the Job's clone container,
# which runs as another uid, and git refuses a repository its user does not own
# ("detected dubious ownership"), which reddens every gate that runs
# `git ls-files`. The Job also passes this setting in the step's environment;
# carrying it here keeps git working however the image is run, for example over
# `docker run -v "$PWD:/workspace"` from a machine whose uid is not 1000.
#
# BUILD
#   Release (GitHub build server -> GHCR):  .github/workflows/build-toolchain-image.yml
#   Local check:   docker build -t memql-toolchain:local deploy/toolchain-image
#                  deploy/toolchain-image/smoke-test.sh memql-toolchain:local
#
# An image that runs in a cluster is built by the build server, never on an
# operator machine (CLAUDE.md, "Image builds"); the local build proves the
# Dockerfile and goes nowhere.

# Go: the official 1.27.1 distribution. Only /usr/local/go is taken; its
# binaries are statically linked, so they run on the bookworm base below
# whatever Debian release the golang image itself is built on.
FROM golang:1.27.1@sha256:512690a5660563b57d37ecc31129e7f136e831db2aed24a1dbeb8ad7380dc0fa AS go-dist

# Node 22 on bookworm, like the base below, so the copied binaries run against
# the C library they were built against.
FROM node:22-bookworm-slim@sha256:43ac6c60b8f89723f746e8a92ce91abd5017e627ce1ddfe4238355d3a30b772c AS node-dist

# protoc, fetched from its release and checked against a pinned SHA-256 per
# architecture before it is unpacked. The stage runs on the BUILD platform
# (it only downloads and unzips), so a cross-architecture build fetches the
# target's binary without emulating curl and unzip.
#
# The checksums are the SHA-256 of protoc-33.4-linux-{x86_64,aarch_64}.zip,
# measured from the downloaded assets and matching the digests GitHub's
# release API reports for them. Bumping PROTOC_VERSION means both: a stale
# checksum fails `sha256sum -c` below, loudly.
FROM --platform=$BUILDPLATFORM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251 AS protoc-dist

ARG PROTOC_VERSION=33.4
ARG PROTOC_SHA256_AMD64=c0040ea9aef08fdeb2c74ca609b18d5fdbfc44ea0042fcfbfb38860d35f7dd66
ARG PROTOC_SHA256_ARM64=15aa988f4a6090636525ec236a8e4b3aab41eef402751bd5bb2df6afd9b7b5a5
ARG TARGETARCH

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl unzip; \
    case "${TARGETARCH}" in \
        amd64) asset=linux-x86_64; sum="${PROTOC_SHA256_AMD64}" ;; \
        arm64) asset=linux-aarch_64; sum="${PROTOC_SHA256_ARM64}" ;; \
        *) echo "ERROR: no pinned protoc ${PROTOC_VERSION} checksum for ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL --retry 5 --retry-all-errors --retry-delay 2 -o /tmp/protoc.zip \
        "https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/protoc-${PROTOC_VERSION}-${asset}.zip"; \
    echo "${sum}  /tmp/protoc.zip" | sha256sum -c -; \
    unzip -q /tmp/protoc.zip -d /opt/protoc; \
    test -x /opt/protoc/bin/protoc; \
    test -f /opt/protoc/include/google/protobuf/timestamp.proto

# ------------------------------------------------------------------------------
# The image.
# ------------------------------------------------------------------------------
FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251

# bash, coreutils and tar are already in the base; they are named because the
# step contract depends on them (the step wrapper frames artifacts with
# `tar -czf - | base64`, hence gzip too). unzip is how scripts/dev/proto-gen.sh
# unpacks its own pinned protoc; without it the script falls back to the protoc
# below with a warning that its fetch failed. jq is what
# scripts/dev/stale_claims_test.go's gh stub evaluates the script's real --jq
# filters with, and the test FAILS without it (requireJQ) -- in the go-tests,
# path-routing and gate-inputs steps alike. GitHub's runner image carries jq,
# which is why ci.yml never installs it, and python3 for the same reason:
# scripts/k3d/up.sh's workload-wait narration reads the pod list with it
# (TestWaitNarratesWhatItIsWaitingOn FAILS without it), and the
# scripts/release/release-engine.sh tests and component/procedure/reference's
# parity harness skip without it. python3-yaml is the PyYAML
# scripts/deploy/conn-headroom-check.sh imports; its tests skip without it.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        bash \
        ca-certificates \
        coreutils \
        curl \
        git \
        gzip \
        jq \
        make \
        postgresql-client \
        python3 \
        python3-yaml \
        tar \
        unzip; \
    rm -rf /var/lib/apt/lists/*; \
    groupadd --gid 1000 memql; \
    useradd --uid 1000 --gid 1000 --create-home --home-dir /home/memql --shell /bin/bash memql; \
    mkdir -p /workspace /cache; \
    chown 1000:1000 /workspace /cache; \
    git config --system --add safe.directory /workspace

COPY --from=go-dist /usr/local/go /usr/local/go
COPY --from=node-dist /usr/local/bin/node /usr/local/bin/node
COPY --from=node-dist /usr/local/lib/node_modules /usr/local/lib/node_modules
# protoc finds the well-known types (google/protobuf/timestamp.proto and
# friends, which this repository's protos import) in ../include beside its
# binary, so the two land side by side under /usr/local.
COPY --from=protoc-dist /opt/protoc/bin/protoc /usr/local/bin/protoc
COPY --from=protoc-dist /opt/protoc/include /usr/local/include

RUN ln -sf /usr/local/lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm && \
    ln -sf /usr/local/lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx

# GOTOOLCHAIN is left at the distribution's default, `auto`, as on the GitHub
# lanes: were go.work's toolchain line ever ahead of this image, go would fetch
# the declared toolchain rather than quietly test on this one. HOME is stated
# rather than left to the runtime to derive from /etc/passwd; GOPATH follows it
# to /home/memql/go. LANG gives tools a UTF-8 locale, which the base leaves
# unset.
ENV HOME=/home/memql \
    LANG=C.UTF-8 \
    PATH=/usr/local/go/bin:/home/memql/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

WORKDIR /workspace

USER 1000:1000

# A broken copy fails the build here; deploy/toolchain-image/smoke-test.sh is
# the full check, and the release lane runs it before anything is pushed.
RUN go version && node --version && protoc --version && git --version && psql --version

LABEL org.opencontainers.image.title="memql-toolchain" \
      org.opencontainers.image.description="The container MemQL pipeline steps run in: Go, Node, protoc, git, make and the PostgreSQL client on Debian bookworm, as uid 1000" \
      org.opencontainers.image.source="https://github.com/znasllc-io/memql"
