# syntax=docker/dockerfile:1

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
RUN go mod download

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
RUN go run github.com/a-h/templ/cmd/templ generate -path component/identity/web/templ
RUN bash scripts/identity/build-css.sh

# Stamp VERSION file with build timestamp
RUN prefix=$(head -n1 VERSION | cut -d- -f1) && \
    echo "${prefix:-0.0.0}-$(date +%s)" > VERSION

# The CGO-free node types keep -ldflags="-s -w" for a stripped static binary.
# The voice node links against libopus dynamically; -s -w still applies. The
# healthcheck is always CGO-free.
RUN CGO_ENABLED=${CGO_ENABLED} GOOS=linux GOARCH=amd64 go build -tags "${BUILD_TAGS}" -ldflags="-s -w" -o /app/bin/memql .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /app/bin/healthcheck ./cmd/healthcheck

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

EXPOSE 8085 50051

ENTRYPOINT ["./memql"]
