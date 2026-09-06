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
# Contract: call engine_build_args_for_node <nodeType> <sourceDir>; it REFUSES
# a nodeType outside ENGINE_NODE_TYPES (returns 1, message on stderr) and
# otherwise sets
#   ENGINE_BUILD_ARGS  -- array of docker build args (BUILD_TAGS +
#                         MEMQL_COMMIT)
#   ENGINE_BUILD_TARGET -- Dockerfile target stage
#                         (runtime | workbench-runtime)
#
# <sourceDir> is the checkout the build context comes from, and it is REQUIRED
# rather than optional on purpose: under `set -u` a caller that forgets it fails
# at the call, and the alternative -- an image that silently carries no
# provenance -- is the exact bug this argument was added to fix (memql#4574).
#
# BUILD_TAGS selects which node-type binary the builder stage compiles
# (go build -tags <node>). The engine Dockerfile has two runtime stages: the
# default distroless `runtime`, and `workbench-runtime` for the one node that
# runs somebody else's build command.
#
# SPA_DIST_STAGE selects where the runtime copies the MemQL OS bundle from
# (memql#3314; it was PORTAL_DIST_STAGE until epic memql#4984 retired the
# portal). Only the edge serves the OS shell (memql#4705 -- the OS is a site
# row, bundleRef file:///app/os, resolved and served the same way as any other
# hosted site's bundle), so only the edge pays for the Node stage that builds
# it; every other node type takes the default empty stage and never pulls a
# Node image. See the global ARG at the top of the Dockerfile for why this is
# a stage selector rather than a flag.

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

# ENGINE_NODE_TYPES is the closed set of node types this repo builds an image
# for -- THE list, in shell. `scripts/k3d/dev.sh` derives `VALID_NODES` from it
# rather than restating it, and `scripts/ci/node_type_lists_test.go` holds it
# against the four other places the same set is spelled out (the `app/build_*.go`
# files, `build_default.go`'s deny-list, the `build-engine-images.yml` release
# matrix, and the Deployments under `deploy/k8s/`).
#
# IT IS A LIST BECAUSE RETIRING A NODE TYPE IS THE DANGEROUS EDIT, not adding
# one (memql#5057). `app/build_default.go` claims every tag combination the
# named types do not, so deleting `app/build_voice.go` did not make
# `BUILD_TAGS=voice` an error -- it made it a spelling of the DEFAULT build. The
# image still built, still imported, and still carried the retired name; the
# only reason anyone found out was an unrelated Dockerfile stage that had gone
# in the same commit. Validating here is what turns that into a refusal.
ENGINE_NODE_TYPES=(identity bff mcp agent planner workbench edge)

# engine_is_node_type <name> -- 0 when the name is one of ENGINE_NODE_TYPES.
function engine_is_node_type() {
    local candidate="$1" known
    for known in "${ENGINE_NODE_TYPES[@]}"; do
        [[ "$known" == "$candidate" ]] && return 0
    done
    return 1
}

function engine_build_args_for_node() {
    local node="$1" source_dir="$2"
    # REFUSED, not defaulted. Both callers pass a node type they got from
    # somewhere else -- a `--node` parameter, a graph step, an out-of-tree script
    # pinned to an older node set -- and the wrong answer to an unknown one is an
    # image named after a node type that no longer exists.
    if ! engine_is_node_type "$node"; then
        echo "ERROR: '${node}' is not a node type this repo builds." >&2
        echo "       Known node types: ${ENGINE_NODE_TYPES[*]}" >&2
        echo "       A node type retired since the caller was written reaches" >&2
        echo "       here as an unknown name; it used to build as a plain BFF" >&2
        echo "       carrying the retired name (memql#5057)." >&2
        return 1
    fi
    ENGINE_BUILD_ARGS=(--build-arg "BUILD_TAGS=${node}")
    ENGINE_BUILD_TARGET="runtime"
    # The workbench is the node that RUNS SOMEBODY ELSE'S BUILD (epic
    # memql#4900), so it takes a stage carrying a Node toolchain, git, and a
    # non-root uid to run the command as. Every other node type would gain
    # nothing from those and a package manager it should not have.
    if [[ "$node" == "workbench" ]]; then
        ENGINE_BUILD_TARGET="workbench-runtime"
    fi
    if [[ "$node" == "edge" ]]; then
        ENGINE_BUILD_ARGS+=(--build-arg SPA_DIST_STAGE=spa-build)
    fi
    ENGINE_BUILD_ARGS+=(--build-arg "MEMQL_COMMIT=$(engine_build_commit "$source_dir")")
}
