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

# Install build dependencies
RUN apk add --no-cache git gcc musl-dev bash curl

# Set working directory
WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

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

# Build the application with optional build tags
RUN CGO_ENABLED=0 GOOS=linux go build -tags "${BUILD_TAGS}" -a -installsuffix cgo -ldflags="-s -w" -o memql main.go

# Build the health check binary (for distroless containers)
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -ldflags="-s -w" -o healthcheck ./cmd/healthcheck

# Stage 2: Runtime (distroless for minimal attack surface)
FROM gcr.io/distroless/base-debian12

WORKDIR /app

# Copy binaries from builder
COPY --from=builder /build/memql .
COPY --from=builder /build/healthcheck .

# Copy necessary directories
COPY --from=builder /build/automations ./automations
COPY --from=builder /build/queries ./queries
COPY --from=builder /build/mutations ./mutations
COPY --from=builder /build/specs ./specs
COPY --from=builder /build/concepts ./concepts
COPY --from=builder /build/prompts ./prompts
COPY --from=builder /build/providers ./providers
COPY --from=builder /build/tools ./tools
COPY --from=builder /build/shapes ./shapes
COPY --from=builder /build/VERSION ./VERSION

# Expose ports
EXPOSE 8085 50051

# Run the application
ENTRYPOINT ["/app/memql"]
