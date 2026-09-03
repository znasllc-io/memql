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
# Contract: call engine_build_args_for_node <nodeType> <sourceDir>; it sets
#   ENGINE_BUILD_ARGS  -- array of docker build args (BUILD_TAGS [+ CGO] +
#                         MEMQL_COMMIT)
#   ENGINE_BUILD_TARGET -- Dockerfile target stage
#                         (runtime | voice-runtime | workbench-runtime)
#
# <sourceDir> is the checkout the build context comes from, and it is REQUIRED
# rather than optional on purpose: under `set -u` a caller that forgets it fails
# at the call, and the alternative -- an image that silently carries no
# provenance -- is the exact bug this argument was added to fix (memql#4574).
#
# BUILD_TAGS selects which node-type binary the builder stage compiles
# (go build -tags <node>). The engine Dockerfile has two runtime stages:
# the default distroless `runtime` for CGO-free nodes and `voice-runtime`
# (debian + libopus) for voice, which needs CGO for LibOpus.
#
# PORTAL_DIST_STAGE selects where the runtime copies the MemQL Portal bundle
# from (memql#3314). Only the edge serves the portal (memql#3711 -- the
# portal is site #1, bundleRef file:///app/portal, resolved and served the
# same way as any other hosted site's bundle; component/portal, which used
# to serve it from the bff, is retired), so only the edge pays for the Node
# stage that builds it; every other node type takes the default empty stage
# and never pulls a Node image. See the global ARG at the top of the
# Dockerfile for why this is a stage selector rather than a flag.

# MEMQL_COMMIT is the git revision the image is built FROM, linked into the
# binary beside MEMQL_RELEASE (core/buildinfo). Without it a locally built node
# image carries no provenance at all, and the reason is worth stating because
# it looks like it should not be needed: `.dockerignore` excludes `.git`, so
# the Go toolchain's own `vcs.revision` stamping -- the fallback `Commit()`
# relies on for a plain `go build .` -- cannot fire inside the image build. The
# build arg is the ONLY source there, which is why the release workflow already
# passes it and why this file passing nothing left every `make dev` cluster
# answering the bare word "dev" with no way to say WHICH dev (memql#4574).
#
# MEMQL_RELEASE is deliberately NOT set here. Neither caller is cutting a
# release -- scripts/k3d/dev.sh builds a developer's checkout, and
# scripts/deploy/build-image.sh is the local/replay surface whose own header
# says the authoritative build runs on the build server -- and a binary that
# was not cut from a release must not name one.

# engine_build_commit <sourceDir> -- the revision to stamp, or "" when git
# cannot answer.
#
# EMPTY IS A REAL ANSWER AND IS PASSED THROUGH. Commit() documents "" as
# meaningful -- callers render it as unknown -- so a directory git cannot read
# produces an unstamped image rather than a fabricated sha or a failed build.
#
# The "-dirty" suffix matches what buildinfo.vcsCommit() produces for the
# toolchain path, and it is load-bearing for the same reason: a revision naming
# a commit whose contents were not what was built is the same confident-wrong
# answer that package exists to prevent. Here it is the normal case rather than
# the exception -- a developer rebuilding to test an edit has, by definition, an
# edit.
function engine_build_commit() {
    local dir="$1" sha=""
    sha="$(git -C "$dir" rev-parse HEAD 2>/dev/null || true)"
    if [[ -z "$sha" ]]; then
        printf ''
        return 0
    fi
    if [[ -n "$(git -C "$dir" status --porcelain 2>/dev/null)" ]]; then
        printf '%s-dirty' "$sha"
    else
        printf '%s' "$sha"
    fi
}

function engine_build_args_for_node() {
    local node="$1" source_dir="$2"
    ENGINE_BUILD_ARGS=(--build-arg "BUILD_TAGS=${node}")
    ENGINE_BUILD_TARGET="runtime"
    if [[ "$node" == "voice" ]]; then
        ENGINE_BUILD_ARGS+=(--build-arg CGO_ENABLED=1)
        ENGINE_BUILD_TARGET="voice-runtime"
    fi
    # The workbench is the node that RUNS SOMEBODY ELSE'S BUILD (epic
    # memql#4900), so it takes a stage carrying a Node toolchain, git, and a
    # non-root uid to run the command as. Every other node type would gain
    # nothing from those and a package manager it should not have.
    if [[ "$node" == "workbench" ]]; then
        ENGINE_BUILD_TARGET="workbench-runtime"
    fi
    if [[ "$node" == "edge" ]]; then
        ENGINE_BUILD_ARGS+=(--build-arg PORTAL_DIST_STAGE=portal-build)
    fi
    ENGINE_BUILD_ARGS+=(--build-arg "MEMQL_COMMIT=$(engine_build_commit "$source_dir")")
}
