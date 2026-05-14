# syntax=docker/dockerfile:1

FROM golang:1.26 AS builder

# BUILD_TAGS controls which node type binary is compiled.
# Defaults to empty (BFF -- the default node type).
#
# Usage:
#   docker build .                                    # bff (default)
#   docker build --build-arg BUILD_TAGS=bff .         # bff (explicit)
#   docker build --build-arg BUILD_TAGS=cognition .   # cognition
#   docker build --build-arg BUILD_TAGS=agent .       # agent
#   docker build --build-arg BUILD_TAGS=planner .     # planner
ARG BUILD_TAGS=""

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Stamp VERSION file with build timestamp
RUN prefix=$(head -n1 VERSION | cut -d- -f1) && \
    echo "${prefix:-0.0.0}-$(date +%s)" > VERSION

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags "${BUILD_TAGS}" -ldflags="-s -w" -o /app/bin/memql .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /app/bin/healthcheck ./cmd/healthcheck

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
