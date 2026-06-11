# memQL Service Dockerfile
# Multi-stage build for optimized image size
#
# Build with node type:
#   docker build .                                    # bff (default)
#   docker build --build-arg BUILD_TAGS=bff .         # bff (explicit)
#   docker build --build-arg BUILD_TAGS=cognition .   # cognition
#   docker build --build-arg BUILD_TAGS=agent .       # agent
#   docker build --build-arg BUILD_TAGS=planner .     # planner

# Stage 1: Builder
FROM golang:1.26-alpine AS builder

ARG BUILD_TAGS=""

# CGO_ENABLED selects the build mode. Default node types build CGO-free; the
# voice node sets CGO_ENABLED=1 (its LiveKit server-sdk-go + media-sdk pull a
# CGO libopus/opusfile/soxr dependency -- see docs/voice/
# 451-livekit-go-room-participation.md, Caveat 1).
ARG CGO_ENABLED=0

# Install build dependencies
RUN apk add --no-cache git gcc musl-dev bash curl

# Voice node CGO build deps: libopus/opusfile/soxr dev headers. No-op for the
# CGO-free node types (the conditional keeps the layer cheap when unused).
RUN if [ "${CGO_ENABLED}" = "1" ]; then \
        apk add --no-cache opus-dev opusfile-dev soxr-dev; \
    fi

# Set working directory
WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Pre-fetch the pinned standalone Tailwind binary into the exact path
# scripts/identity/build-css.sh probes (bin/tools is dockerignored, so the
# source COPY below never clobbers it). Early layer = the ~100MB download
# only re-runs on a version bump, never on source changes (memql#1351).
# Keep TAILWIND_VERSION in sync with the script's pin; a missed bump
# degrades gracefully (the script re-downloads, with retries).
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

# Copy source code
COPY . .

# Generate Go from .templ files BEFORE compiling. The identity binary
# (BUILD_TAGS=identity) needs the generated *_templ.go in
# component/identity/web/templ/. Other binaries skip this work via
# build tags but `go run` is cheap enough to always do — keeps the
# Dockerfile simple at the cost of a few seconds in non-identity builds.
RUN go run github.com/a-h/templ/cmd/templ generate -path component/identity/web/templ

# Compile Tailwind input -> static/app.css. Same script that runs
# locally; resolves the right Tailwind binary for the build container's
# platform (linux-x64 or linux-arm64) and downloads it on demand.
# Cached under bin/tools/ in the build layer.
RUN bash scripts/identity/build-css.sh

# Build the application with optional build tags. Build the whole main
# package ('.'), not 'main.go' alone -- the single-file form excludes
# sibling files in package main (e.g. subcommand_stub.go, which defines
# dispatchSubcommand called from main.go), breaking every node build.
# The -installsuffix cgo flag is only meaningful for static (CGO-free)
# builds, so it is omitted; CGO_ENABLED is the build arg (0 for default node
# types, 1 for the voice node).
RUN CGO_ENABLED=${CGO_ENABLED} GOOS=linux go build -tags "${BUILD_TAGS}" -a -ldflags="-s -w" -o memql .

# Build the health check binary (always CGO-free, for distroless containers)
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -ldflags="-s -w" -o healthcheck ./cmd/healthcheck

# Stage 2: Runtime (distroless for minimal attack surface) -- CGO-free nodes.
FROM gcr.io/distroless/base-debian12

WORKDIR /app

# Copy binaries from builder
COPY --from=builder /build/memql .
COPY --from=builder /build/healthcheck .

# DSL tree is embedded into the binary (see dsl/embed.go); the
# on-disk copy is kept only so MEMQL_DSL_PATH overrides work from
# inside the container if anyone wants them.
COPY --from=builder /build/dsl ./dsl
COPY --from=builder /build/VERSION ./VERSION

# Expose ports
EXPOSE 8085 50051

# Run the application
ENTRYPOINT ["/app/memql"]

# Stage 3: voice-runtime -- the voice node links libopus/opusfile/soxr
# dynamically, so distroless (no shared libraries) cannot run it. This stage
# uses alpine with the runtime shared libraries installed. Select it with
# `--target voice-runtime` (the voice compose service does so).
FROM alpine:3.21 AS voice-runtime

RUN apk add --no-cache opus opusfile soxr ca-certificates

WORKDIR /app

COPY --from=builder /build/memql .
COPY --from=builder /build/healthcheck .
COPY --from=builder /build/dsl ./dsl
COPY --from=builder /build/VERSION ./VERSION

EXPOSE 8085 50051

ENTRYPOINT ["/app/memql"]
