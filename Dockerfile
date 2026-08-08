# syntax=docker/dockerfile:1

# PORTAL_DIST_STAGE selects which stage the runtime copies the memQL Portal
# bundle from (memql#3314). It is a GLOBAL ARG -- declared before the first
# FROM -- because that is the only scope a `FROM ${VAR}` line can read.
#
# WHY A STAGE SELECTOR RATHER THAN AN UNCONDITIONAL COPY
#
# Only the bff serves the portal (app/transport_portal.go). A Dockerfile has
# no conditional COPY, so the alternatives were: build the SPA in every node
# image (a Node toolchain + npm install + vite build on all eight, for bytes
# seven of them never serve), or add a second runtime target and duplicate the
# runtime stage. Naming the SOURCE stage instead costs one indirection and
# leaves both runtime stages identical for every node type.
#
# BuildKit only builds stages that are actually referenced, so the default --
# portal-skip, an empty directory -- means a non-bff build never pulls the
# Node image and never runs npm. Its cost is one `mkdir` on a stage that was
# already built.
#
#   docker build --build-arg PORTAL_DIST_STAGE=portal-build ...   # with the SPA
#   docker build ...                                              # without (default)
#
# scripts/lib/engine_build_args.sh sets it for the bff on the local path;
# .github/workflows/build-engine-images.yml sets it for the release build.
# Those two are the only callers, and both are asserted by
# scripts/ci/portal_image_wiring_test.go.
ARG PORTAL_DIST_STAGE=portal-skip

FROM golang:1.26.4@sha256:68cb6d68bed024785b69195b89af7ac7a444f27791435f98647edff595aa0479 AS builder

# BUILD_TAGS controls which node type binary is compiled.
# Defaults to empty (BFF -- the default node type).
#
# Usage:
#   docker build .                                    # bff (default)
#   docker build --build-arg BUILD_TAGS=bff .         # bff (explicit)
#   docker build --build-arg BUILD_TAGS=cognition .   # cognition
#   docker build --build-arg BUILD_TAGS=agent .       # agent
#   docker build --build-arg BUILD_TAGS=planner .     # planner
#   docker build --build-arg BUILD_TAGS=voice .       # voice (CGO + libopus)
ARG BUILD_TAGS=""

# CGO_ENABLED selects the build mode. The default node types build CGO-free
# (static binaries, distroless runtime). The voice node is the exception:
# the Go voice-agent joins LiveKit rooms via server-sdk-go/v2 + its media-sdk,
# which pull a CGO libopus/opusfile/soxr dependency (see docs/voice/
# 451-livekit-go-room-participation.md, Caveat 1). The voice compose service
# passes CGO_ENABLED=1 alongside BUILD_TAGS=voice.
ARG CGO_ENABLED=0

WORKDIR /app

# When CGO is on (the voice node) we need the C toolchain headers for libopus,
# opusfile, and soxr at build time. The base golang image already carries gcc.
# This apt step is a no-op cost for the CGO-free node types (the conditional
# keeps it from running unless CGO_ENABLED=1).
RUN if [ "${CGO_ENABLED}" = "1" ]; then \
        apt-get update && \
        apt-get install -y --no-install-recommends \
            libopus-dev libopusfile-dev libsoxr-dev && \
        rm -rf /var/lib/apt/lists/*; \
    fi

COPY go.mod go.sum ./
# The `wire` tier is a set of NESTED modules that the root go.mod `replace`s by
# relative path (memql#3240). `go mod download` resolves those replaces, so the
# nested go.mod/go.sum must exist in this layer -- `COPY . .` below is far too
# late, and without these three lines the dependency-cache layer fails with
# "reading component/bus/gen/go.mod: no such file or directory".
#
# Manifests only, deliberately: copying the sources here would defeat the
# layer-caching this split-COPY exists for. The `go.*` glob is load-bearing --
# an L0 module with no external dependencies has NO go.sum, and Docker fails a
# COPY whose named source does not exist. Add a line per module as each tier
# lands (#3242..#3244).
COPY component/actions/go.* ./component/actions/
COPY component/architecture/go.* ./component/architecture/
COPY component/auth/go.* ./component/auth/
COPY component/automations/go.* ./component/automations/
COPY component/bus/go.* ./component/bus/
COPY component/bus/gen/go.* ./component/bus/gen/
COPY component/config/go.* ./component/config/
COPY component/database/go.* ./component/database/
COPY component/deploycontrol/go.* ./component/deploycontrol/
COPY component/events/go.* ./component/events/
COPY component/fileprocessor/go.* ./component/fileprocessor/
COPY component/genesis/go.* ./component/genesis/
COPY component/grpc/go.* ./component/grpc/
COPY component/grpc/gen/go.* ./component/grpc/gen/
COPY component/harness/go.* ./component/harness/
COPY component/healing/go.* ./component/healing/
COPY component/identity/go.* ./component/identity/
COPY component/identity/admin/go.* ./component/identity/admin/
COPY component/inbound/go.* ./component/inbound/
COPY component/language/go.* ./component/language/
COPY component/language/annotations/go.* ./component/language/annotations/
COPY component/language/ast/go.* ./component/language/ast/
COPY component/language/dslclause/go.* ./component/language/dslclause/
COPY component/mcp/go.* ./component/mcp/
COPY component/memql/go.* ./component/memql/
COPY component/metadata/go.* ./component/metadata/
COPY component/metrics/go.* ./component/metrics/
COPY component/node/go.* ./component/node/
COPY component/node/gen/go.* ./component/node/gen/
COPY component/observe/go.* ./component/observe/
COPY component/outbound/go.* ./component/outbound/
COPY component/planner/go.* ./component/planner/
COPY component/polyphon/go.* ./component/polyphon/
COPY component/provenance/go.* ./component/provenance/
COPY component/router/go.* ./component/router/
COPY component/safety/go.* ./component/safety/
COPY component/secret/go.* ./component/secret/
COPY component/server/go.* ./component/server/
COPY component/service/go.* ./component/service/
COPY component/worker/go.* ./component/worker/
COPY core/go.* ./core/
COPY docs/go.* ./docs/
COPY dsl/go.* ./dsl/
COPY integrations/go.* ./integrations/
COPY integrations/email/go.* ./integrations/email/
COPY integrations/openai/go.* ./integrations/openai/
COPY integrations/stt/go.* ./integrations/stt/
# BuildKit cache mounts (build-speed #1506): the module cache (/go/pkg/mod)
# and the Go build cache (/root/.cache/go-build) persist across builds, so a
# rebuild of an unchanged tree reuses downloaded modules + already-compiled
# packages instead of redoing both from scratch. Every `go` step below mounts
# the SAME two caches -- the modules fetched here must stay visible to the
# templ-generate + go-build steps, and cache mounts are unmounted after each
# RUN (they are NOT baked into an image layer), so a step without the mount
# would re-download. Requires BuildKit (docker buildx / DOCKER_BUILDKIT=1;
# `az acr build` runs BuildKit when the Dockerfile uses mount syntax).
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Pre-fetch the pinned standalone Tailwind binary into the exact path
# scripts/identity/build-css.sh probes (bin/tools is dockerignored, so the
# source COPY below never clobbers it). Doing this BEFORE the source copy
# keeps the ~100MB download in a layer that only invalidates on a version
# bump -- previously every source change re-downloaded it concurrently
# across all builder stages and one TLS hiccup killed the whole cluster
# build (memql#1351). Keep TAILWIND_VERSION in sync with the script's pin;
# a missed bump degrades gracefully (the script re-downloads, with retries).
ARG TAILWIND_VERSION=v3.4.17
RUN set -e; \
    case "$(uname -m)" in \
        x86_64) platform=linux-x64 ;; \
        aarch64|arm64) platform=linux-arm64 ;; \
        *) echo "unsupported arch $(uname -m)" >&2; exit 1 ;; \
    esac; \
    mkdir -p bin/tools; \
    curl -sSL --fail --retry 5 --retry-all-errors --retry-delay 2 \
        -o "bin/tools/tailwindcss-${platform}" \
        "https://github.com/tailwindlabs/tailwindcss/releases/download/${TAILWIND_VERSION}/tailwindcss-${platform}"; \
    chmod +x "bin/tools/tailwindcss-${platform}"

COPY . .

# Generate the identity web assets BEFORE compiling so the identity binary
# (BUILD_TAGS=identity) embeds them via //go:embed in component/identity/web:
#   1. templ -> component/identity/web/templ/*_templ.go
#   2. Tailwind -> component/identity/web/static/app.css  (gitignored, generated)
# This MUST mirror docker/memql.Dockerfile. The root Dockerfile (used by
# scripts/deploy/aks-deploy.sh) was missing these steps, so identity images
# shipped WITHOUT app.css -> /static/app.css 404 -> unstyled login page.
# Cheap (a few seconds) for non-identity node builds; kept unconditional so
# the two Dockerfiles stay in sync.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go run github.com/a-h/templ/cmd/templ generate -path component/identity/web/templ
RUN bash scripts/identity/build-css.sh

# Stamp VERSION file with build timestamp
RUN prefix=$(head -n1 VERSION | cut -d- -f1) && \
    echo "${prefix:-0.0.0}-$(date +%s)" > VERSION

# The CGO-free node types keep -ldflags="-s -w" for a stripped static binary.
# The voice node links against libopus dynamically; -s -w still applies. The
# healthcheck is always CGO-free.
#
# GOARCH follows TARGETARCH (the BuildKit-provided target-platform arch) rather
# than a hardcoded amd64, so the CGO voice node builds NATIVELY for the host:
# amd64 under staging's `az acr build --platform linux/amd64`, arm64 on an
# Apple-Silicon dev machine running the local parity cluster. A hardcoded
# amd64 cross-compile from arm64 fails the CGO voice build (gcc: unrecognized
# '-m64'); the CGO-free nodes cross-compile either way. Defaults to amd64 if
# TARGETARCH is somehow unset (preserves the prior behaviour).
ARG TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=${CGO_ENABLED} GOOS=linux GOARCH=${TARGETARCH:-amd64} go build -tags "${BUILD_TAGS}" -ldflags="-s -w" -o /app/bin/memql .
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} go build -ldflags="-s -w" -o /app/bin/healthcheck ./cmd/healthcheck

# --- memQL Portal SPA (memql#3314) ----------------------------------------
#
# A Node stage, entirely separate from the Go builder: nothing here touches
# the Go toolchain and nothing in the Go stages touches Node. That separation
# is the whole reason the bundle is served from a directory instead of being
# //go:embed'ed (component/portal/doc.go) -- every Go lane in CI, and every
# `go build ./...` on a developer machine, stays Node-free.
#
# bookworm-slim rather than alpine: scripts/portal/build.sh is a bash script
# (per the Makefile+shell convention in CLAUDE.md) and alpine ships no bash.
# Builder-only, so image size is irrelevant.
FROM node:22-bookworm-slim AS portal-build

WORKDIR /src

# Manifests first, sources second: the standard layer-caching split, and it
# matters more here than usual because `npm ci` is the slow step. The two
# `file:` dependencies' manifests come along because npm has to resolve them
# to install the portal at all -- a `file:` dep is a linked source tree, not a
# registry tarball.
COPY clients/portal/package.json clients/portal/package-lock.json ./clients/portal/
COPY sdk/ts/package.json ./sdk/ts/
COPY sdk/ts-viewkit/package.json sdk/ts-viewkit/package-lock.json ./sdk/ts-viewkit/

COPY sdk/ts ./sdk/ts
COPY sdk/ts-viewkit ./sdk/ts-viewkit
COPY clients/portal ./clients/portal
COPY scripts/portal ./scripts/portal

# The SAME script `make portal-build` runs, so the image bundle and a locally
# built one cannot diverge in how they were produced. Moved to /portal-dist so
# both alternatives of the PORTAL_DIST_STAGE selector expose the bundle at one
# path -- the runtime's COPY cannot branch on which stage it resolved to.
RUN bash scripts/portal/build.sh build && mv clients/portal/dist /portal-dist

# portal-skip is the empty alternative the PORTAL_DIST_STAGE selector resolves
# to by default. Derived FROM builder purely because that stage is already
# built -- it contributes one empty directory and pulls no additional image.
FROM builder AS portal-skip
RUN mkdir -p /portal-dist

FROM ${PORTAL_DIST_STAGE} AS portal-dist

# --- Runtime: CGO-free node types (default) use distroless. ---------------
FROM gcr.io/distroless/base-debian12 AS runtime

# Environment variables are injected by Cloud Run via service.yaml
# No ENV block needed here - Cloud Run overrides at the service level

WORKDIR /app

COPY --from=builder /app/bin/memql ./memql
COPY --from=builder /app/bin/healthcheck ./healthcheck
COPY --from=builder /app/VERSION ./VERSION
# DSL files (concepts, mutations, queries, specs, automations, prompts,
# providers, shapes, tools, policies) are //go:embed-baked into the
# binary at compile time. The on-disk copy is only needed if
# MEMQL_DSL_PATH is set at runtime to override the embedded tree
# (dev/per-deploy patches). Cloud Run runs from the embedded copy.
#
# The memQL Portal bundle is the opposite: NEVER embedded, always a directory,
# because embedding it would put a Node build in front of every Go build (see
# component/portal/doc.go). /app/portal is component/portal.DefaultDistDir;
# MEMQL_PORTAL_DIST overrides it. Empty for every node type except the bff --
# the handler answers 404 with an actionable message rather than failing boot.
COPY --from=portal-dist /portal-dist ./portal

EXPOSE 8085 50051

ENTRYPOINT ["./memql"]

# --- Runtime: voice node (CGO) needs the libopus shared libraries. --------
# The voice binary links libopus/opusfile/soxr dynamically, so distroless
# (no libc package manager, no shared libs) cannot run it. This stage uses
# debian-slim with the runtime shared libraries installed. Select it with
# `--target voice-runtime` (the voice compose service does so).
FROM debian:12-slim AS voice-runtime

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        libopus0 libopusfile0 libsoxr0 ca-certificates && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /app/bin/memql ./memql
COPY --from=builder /app/bin/healthcheck ./healthcheck
COPY --from=builder /app/VERSION ./VERSION
# Same portal copy as the distroless runtime above. Present in BOTH stages
# deliberately: the release workflow builds the CGO-free node types with no
# --target, which resolves to the LAST stage -- this one -- so a copy only in
# the distroless stage would ship no portal in the images that actually run.
COPY --from=portal-dist /portal-dist ./portal

EXPOSE 8085 50051

ENTRYPOINT ["./memql"]
