#!/usr/bin/env bash
# Local build/import integrity shared by the k3d development path.
# Bash 3.2; no host-side JSON parser or containerd client is required.

function local_image_id_valid() {
    [[ "$1" =~ ^sha256:[0-9a-f]{64}$ ]]
}

# Capture the build's own result, rather than inspecting a mutable tag after
# another editor/terminal build could have replaced it. Caller owns build args.
function build_local_image() {
    local state rc
    state="$(mktemp -d "${TMPDIR:-/tmp}/memql-image-build.XXXXXX")" || return 1
    if docker build --iidfile "${state}/id" "$@" >&2; then
        LOCAL_IMAGE_BUILT_ID="$(cat "${state}/id")"
        rm -rf "$state"
        if ! local_image_id_valid "$LOCAL_IMAGE_BUILT_ID"; then
            printf 'ERROR: Docker build did not report a valid image ID.\n' >&2
            return 1
        fi
    else
        rc=$?
        rm -rf "$state"
        return "$rc"
    fi
}

function local_image_matches() {
    local image="$1" expected="$2" actual
    if ! local_image_id_valid "$expected"; then
        printf 'ERROR: No valid captured image ID for %s.\n' "$image" >&2
        return 1
    fi
    actual="$(docker image inspect --format '{{.Id}}' "$image")" || return 1
    if [[ "$actual" != "$expected" ]]; then
        printf 'ERROR: %s changed since this build (%s -> %s). Another update may be running; retry after it finishes.\n' "$image" "$expected" "$actual" >&2
        return 1
    fi
}

# Query actual containers, not a guessed server-0 or the configured node count.
# Include stopped nodes: success requires every server and agent to hold the
# image. A missing/unreadable inventory cannot prove an import succeeded.
function local_image_nodes() {
    local cluster="$1" inventory name role state found=0
    inventory="$(docker ps --all --filter "label=k3d.cluster=${cluster}" \
        --format '{{.Names}} {{.Label "k3d.role"}} {{.State}}')" || return 1
    while read -r name role state; do
        case "$role" in server|agent) ;; *) continue ;; esac
        if [[ "$state" != running ]]; then
            printf 'ERROR: Cluster node %s is %s; its image cannot be verified.\n' "$name" "$state" >&2
            return 1
        fi
        printf '%s\n' "$name"
        found=1
    done <<< "$inventory"
    if [[ "$found" == 0 ]]; then
        printf 'ERROR: No server or agent containers found for k3d cluster %s.\n' "$cluster" >&2
        return 1
    fi
}

function local_image_canonical_ref() {
    local image="$1" first
    # Docker's local refs may omit docker.io and the library namespace.
    if [[ "$image" != */* ]]; then image="docker.io/library/${image}"; fi
    first="${image%%/*}"
    case "$first" in *.*|*:*|localhost) ;; *) image="docker.io/${image}" ;; esac
    if [[ "$image" == docker.io/* && "${image#docker.io/}" != */* ]]; then
        image="docker.io/library/${image#docker.io/}"
    fi
    printf '%s\n' "$image"
}

function local_image_sha256() {
    if command -v sha256sum >/dev/null 2>&1; then sha256sum; else shasum -a 256; fi
}

# Read bytes from a never-started container of the captured immutable image,
# not its mutable tag. Keep the proof small and remove the temporary copies
# before importing. A subshell owns cleanup without replacing callers' traps.
function local_engine_image_proof() (
    local expected="$1" layers layer chain="" name metadata hash
    # EXIT after an explicit `exit` can unwind Bash 3.2 function locals before
    # invoking the trap. These two variables belong to this subshell instead.
    image_proof_container=""
    image_proof_state="$(mktemp -d "${TMPDIR:-/tmp}/memql-image-proof.XXXXXX")" || exit 1
    function cleanup_image_proof() {
        local status=$?
        trap - EXIT
        if [[ -n "$image_proof_container" ]]; then
            docker rm -v "$image_proof_container" >/dev/null || status=1
        fi
        rm -rf "$image_proof_state" || status=1
        exit "$status"
    }
    trap cleanup_image_proof EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    layers="$(docker image inspect --format '{{range .RootFS.Layers}}{{println .}}{{end}}' "$expected")" || exit 1
    while read -r layer; do
        local_image_id_valid "$layer" || exit 1
        if [[ -z "$chain" ]]; then chain="$layer"; else
            hash="$(printf '%s %s' "$chain" "$layer" | local_image_sha256)" || exit 1
            chain="sha256:${hash%% *}"
        fi
    done <<< "$layers"
    local_image_id_valid "$chain" || exit 1
    image_proof_container="$(docker create --pull=never --read-only --network none --entrypoint /app/memql "$expected")" || exit 1
    [[ "$image_proof_container" =~ ^[0-9a-f]{64}$ ]] || exit 1
    printf '%s\n' "$chain"
    for name in memql healthcheck; do
        docker cp "${image_proof_container}:/app/${name}" "${image_proof_state}/${name}" >&2 || exit 1
        [[ -f "${image_proof_state}/${name}" && ! -L "${image_proof_state}/${name}" && -s "${image_proof_state}/${name}" && -x "${image_proof_state}/${name}" ]] || exit 1
        metadata="$(stat -c '%s %a' "${image_proof_state}/${name}" 2>/dev/null)" \
            || metadata="$(stat -f '%z %Lp' "${image_proof_state}/${name}")" || exit 1
        [[ "$metadata" =~ ^[0-9]+\ [0-7]{3,4}$ ]] || exit 1
        hash="$(local_image_sha256 < "${image_proof_state}/${name}")" || exit 1
        [[ "${hash%% *}" =~ ^[0-9a-f]{64}$ ]] || exit 1
        printf '%s %s\n' "${hash%% *}" "$metadata"
    done
)

# Metadata can survive ENOSPC while committed snapshot files become empty.
# Mount the merged image read-only, bind its reported chain to Docker's exact
# image, then compare both executable files. Never run the imported executable.
# ctr's --rm unmount releases both its temporary snapshot view and its lease.
function local_engine_node_matches() {
    local node="$1" canonical="$2" proof="$3"
    docker exec "$node" sh -c '
        set -eu
        image=$1
        proof=$2
        root=$(mktemp -d "${TMPDIR:-/tmp}/memql-image-proof.XXXXXX")
        cleanup() {
            status=$?
            trap - EXIT
            if ctr -n k8s.io images unmount --rm "$root" >/dev/null; then
                rmdir "$root" || status=1
            else
                echo "ERROR: Could not release image verification mount $root." >&2
                status=1
            fi
            exit "$status"
        }
        trap cleanup EXIT
        trap "exit 130" INT
        trap "exit 143" TERM
        mounted=$(ctr -n k8s.io images mount "$image" "$root") || exit 1
        chain=$(printf "%s\n" "$mounted" | head -n 1)
        expected_chain=$(printf "%s\n" "$proof" | sed -n "1p")
        [ "$chain" = "$expected_chain" ] || { echo "ERROR: Mounted image chain changed." >&2; exit 1; }
        line=2
        for name in memql healthcheck; do
            file=$root/app/$name
            [ -f "$file" ] && [ ! -L "$file" ] && [ -s "$file" ] && [ -x "$file" ] || exit 1
            metadata=$(stat -c "%s %a" "$file" 2>/dev/null) || metadata=$(stat -f "%z %Lp" "$file") || exit 1
            if command -v sha256sum >/dev/null 2>&1; then hash=$(sha256sum < "$file"); else hash=$(shasum -a 256 < "$file"); fi
            actual="${hash%% *} $metadata"
            expected=$(printf "%s\n" "$proof" | sed -n "${line}p")
            [ "$actual" = "$expected" ] || { echo "ERROR: Imported /app/$name differs from the built image." >&2; exit 1; }
            line=$((line + 1))
        done
    ' sh "$canonical" "$proof" || {
        printf 'ERROR: Engine executable verification failed on %s.\n' "$node" >&2
        return 1
    }
}

function local_image_nodes_match() {
    local image="$1" expected="$2" canonical="$3" nodes="$4" proof="${5:-}"
    local node listing target config ready
    while read -r node; do
        listing="$(docker exec "$node" ctr -n k8s.io images ls "name==${canonical}")" || return 1
        target="$(printf '%s\n' "$listing" | awk -v ref="$canonical" '$1 == ref { print $3 }')"
        config="$(docker exec "$node" crictl inspecti -o go-template --template '{{.status.id}}' "$canonical")" || return 1
        # Classic Docker uses a config ID; Docker's containerd image store
        # uses the manifest/index digest. Neither is a tag or a partial hash.
        if ! local_image_id_valid "$target" || ! local_image_id_valid "$config" || \
            [[ "$expected" != "$target" && "$expected" != "$config" ]]; then
            printf 'ERROR: %s on %s does not match built image %s (target %s, config %s).\n' "$image" "$node" "$expected" "$target" "$config" >&2
            return 1
        fi
        ready="$(docker exec "$node" ctr -n k8s.io images check --quiet "name==${canonical}")" || return 1
        if [[ "$ready" != "$canonical" ]]; then
            printf 'ERROR: %s is not fully downloaded and unpacked on %s.\n' "$image" "$node" >&2
            return 1
        fi
        if [[ -n "$proof" ]]; then
            local_engine_node_matches "$node" "$canonical" "$proof" || return 1
        fi
        printf 'INFO: Verified %s on %s (%s).\n' "$image" "$node" "$expected" >&2
    done <<< "$nodes"
}

# One direct-mode import attempt, bounded. Direct mode feeds one `docker save`
# stream to every node; when one node's ctr stops reading early, k3d v5.9.0
# stops feeding the others and then waits on them forever (pkg/client/tools.go,
# loadImageFromStream). Any second node, such as the pipeline build node, makes
# that reachable, so an attempt that outlives LOCAL_IMAGE_IMPORT_LIMIT_SECONDS
# (default 600) is killed and reconciled like any other failed import.
function local_image_import_bounded() {
    local image="$1" cluster="$2" limit="${LOCAL_IMAGE_IMPORT_LIMIT_SECONDS:-600}" pid deadline
    k3d image import "$image" --cluster "$cluster" --mode direct >&2 &
    pid=$!
    deadline=$((SECONDS + limit))
    while kill -0 "$pid" 2>/dev/null; do
        if [[ "$SECONDS" -ge "$deadline" ]]; then
            printf 'WARNING: k3d image import of %s did not finish within %ss; stopping it.\n' "$image" "$limit" >&2
            kill -KILL "$pid" 2>/dev/null || true
            wait "$pid" 2>/dev/null || true
            return 124
        fi
        sleep 0.2 2>/dev/null || sleep 1
    done
    wait "$pid"
}

function import_local_image_verified() {
    local image="$1" cluster="$2" expected="$3" nodes after canonical attempt import_status
    local integrity="${4:-metadata}" proof=""
    local_image_matches "$image" "$expected" || return 1
    case "$integrity" in
        metadata) ;;
        engine)
            proof="$(local_engine_image_proof "$expected")" || {
                printf 'ERROR: Could not read executable proof from built image %s.\n' "$expected" >&2
                return 1
            } ;;
        *) printf 'ERROR: Unknown image integrity requirement.\n' >&2; return 1 ;;
    esac
    nodes="$(local_image_nodes "$cluster")" || return 1
    canonical="$(local_image_canonical_ref "$image")"

    # Docker can close its socket during a direct import after copying some or
    # all content. Reconcile the exact destination state before retrying: a
    # failed command is adoptable only when every unchanged node has the full,
    # unpacked content for this captured image ID. Otherwise one bounded,
    # idempotent import retry may complete the same image transfer.
    for attempt in 1 2; do
        if local_image_import_bounded "$image" "$cluster"; then
            import_status=0
        else
            import_status=$?
            printf 'WARNING: k3d image import returned %s for %s; reconciling node contents.\n' "$import_status" "$image" >&2
        fi

        local_image_matches "$image" "$expected" || return 1
        after="$(local_image_nodes "$cluster")" || return 1
        if [[ "$nodes" != "$after" ]]; then
            printf 'ERROR: Cluster nodes changed during import; retry against the current cluster.\n' >&2
            return 1
        fi
        if local_image_nodes_match "$image" "$expected" "$canonical" "$nodes" "$proof"; then
            local_image_matches "$image" "$expected" || return 1
            if [[ "$import_status" != 0 ]]; then
                printf 'INFO: Adopted complete image contents after import command failed.\n' >&2
            fi
            return 0
        fi

        if [[ "$attempt" == 2 ]]; then return 1; fi
        printf 'WARNING: %s is incomplete in the cluster; retrying the verified import once.\n' "$image" >&2
        sleep 1
    done
}
