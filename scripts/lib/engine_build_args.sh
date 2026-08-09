#!/usr/bin/env bash
#
# scripts/lib/engine_build_args.sh
# ================================
#
# THE single nodeType -> docker build-args mapping for ENGINE images
# (fix for memql#2379: build-image.sh apply mode built every node type as
# the bff-default binary because it passed no BUILD_TAGS).
#
# Shared by scripts/k3d/dev.sh (make dev inner loop) and
# scripts/deploy/build-image.sh (the deploy.buildImage capability backend)
# so the mapping cannot drift between the two local build paths. The
# AUTHORITATIVE staging/prod builds run on the GitHub build server and do
# not use this file.
#
# Contract: call engine_build_args_for_node <nodeType>; it sets
#   ENGINE_BUILD_ARGS  -- array of docker build args (BUILD_TAGS [+ CGO])
#   ENGINE_BUILD_TARGET -- Dockerfile target stage (runtime | voice-runtime)
#
# BUILD_TAGS selects which node-type binary the builder stage compiles
# (go build -tags <node>). The engine Dockerfile has two runtime stages:
# the default distroless `runtime` for CGO-free nodes and `voice-runtime`
# (debian + libopus) for voice, which needs CGO for LibOpus.
#
# PORTAL_DIST_STAGE selects where the runtime copies the memQL Portal bundle
# from (memql#3314). Only the bff serves the portal, so only the bff pays for
# the Node stage that builds it; every other node type takes the default
# empty stage and never pulls a Node image. See the global ARG at the top of
# the Dockerfile for why this is a stage selector rather than a flag.

function engine_build_args_for_node() {
    local node="$1"
    ENGINE_BUILD_ARGS=(--build-arg "BUILD_TAGS=${node}")
    ENGINE_BUILD_TARGET="runtime"
    if [[ "$node" == "voice" ]]; then
        ENGINE_BUILD_ARGS+=(--build-arg CGO_ENABLED=1)
        ENGINE_BUILD_TARGET="voice-runtime"
    fi
    if [[ "$node" == "bff" ]]; then
        ENGINE_BUILD_ARGS+=(--build-arg PORTAL_DIST_STAGE=portal-build)
    fi
}
