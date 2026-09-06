# MemQL Service Dockerfile
# Multi-stage build for optimized image size
#
# Build with node type:
#   docker build .                                    # bff (default)
#   docker build --build-arg BUILD_TAGS=bff .         # bff (explicit)
#   docker build --build-arg BUILD_TAGS=agent .       # agent
#   docker build --build-arg BUILD_TAGS=planner .     # planner

# Stage 1: Builder
FROM golang:1.26-alpine AS builder

ARG BUILD_TAGS=""

# MEMQL_RELEASE is the release tag this image is being cut at -- e.g. "v0.18.1".
# It is linked into the binary (core/buildinfo) and is the ONLY way a node can
# learn which release it is; unset means the node reports "dev" (memql#3998).
# scripts/release/release.sh passes its --version here.
ARG MEMQL_RELEASE=""

# MEMQL_COMMIT is the git revision this image was built from. It is linked into
# the binary beside MEMQL_RELEASE and is what answers "which SOURCE is
# executing" -- a question the release tag alone cannot, because a tag's image
# pins are written before that tag's own images exist, so manifests and
# binaries legitimately differ by one release (memql#4486). Unset means the
# node reports no commit, which callers render as unknown.
ARG MEMQL_COMMIT=""

# Install build dependencies
RUN apk add --no-cache git gcc musl-dev bash curl

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
# builds, so it is omitted; every node type is CGO-free.
RUN CGO_ENABLED=0 GOOS=linux go build -tags "${BUILD_TAGS}" -a -ldflags="-s -w -X github.com/znasllc-io/memql/core/buildinfo.release=${MEMQL_RELEASE} -X github.com/znasllc-io/memql/core/buildinfo.commit=${MEMQL_COMMIT}" -o memql .

# Build the health check binary (always CGO-free, for distroless containers)
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -ldflags="-s -w" -o healthcheck ./cmd/healthcheck

# Stage 2: Runtime (distroless for minimal attack surface). Every node type is
# CGO-free, so this is the only runtime stage and it is LAST -- a build with no
# --target resolves here.
FROM gcr.io/distroless/base-debian12

WORKDIR /app

# Copy binaries from builder
COPY --from=builder /build/memql .
COPY --from=builder /build/healthcheck .

# DSL tree is embedded into the binary (see dsl/embed.go); the
# on-disk copy is kept only so MEMQL_DSL_PATH overrides work from
# inside the container if anyone wants them.
COPY --from=builder /build/dsl ./dsl

# Expose ports
EXPOSE 8085 50051

# Run the application
ENTRYPOINT ["/app/memql"]
