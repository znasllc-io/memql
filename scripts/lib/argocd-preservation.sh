#!/usr/bin/env bash
# Sourced by installation seeders. These functions handle only the two named
# preservation annotations; no Secret data or arbitrary annotations are read.

ARGO_PRESERVATION_RV=""
ARGO_PRESERVATION_SYNC="Prune=false"
ARGO_PRESERVATION_COMPARE="IgnoreExtraneous"
ARGO_PRESERVATION_CHANGED=false

function argocd_preservation_options() {
    local kind="$1" remaining="$2" required key value token result="" seen="," found=false
    [[ ${#remaining} -le 4096 ]] || return 3
    case "$kind" in
        sync) required="Prune=false" ;;
        compare) required="IgnoreExtraneous" ;;
        *) return 3 ;;
    esac
    if [[ -z "$remaining" ]]; then
        printf '%s' "$required"
        return 0
    fi
    while true; do
        token="${remaining%%,*}"
        token="${token#"${token%%[![:space:]]*}"}"
        token="${token%"${token##*[![:space:]]}"}"
        [[ "$token" =~ ^[A-Za-z][A-Za-z0-9]*(=[A-Za-z0-9_-]+)?$ ]] || return 3
        key="${token%%=*}"
        [[ "$seen" != *",${key},"* ]] || return 3
        seen="${seen}${key},"
        if [[ "$kind" == sync ]]; then
            [[ "$token" == *=* ]] || return 3
            value="${token#*=}"
            case "$key" in
                Prune) [[ "$value" == false ]] || return 3; found=true ;;
                Force|Replace) [[ "$value" == false ]] || return 3 ;;
            esac
        elif [[ "$key" == IgnoreExtraneous ]]; then
            [[ "$token" == IgnoreExtraneous ]] || return 3
            found=true
        fi
        [[ -z "$result" ]] || result="${result},"
        result="${result}${token}"
        [[ "$remaining" == *,* ]] || break
        remaining="${remaining#*,}"
    done
    [[ "$found" == true ]] || result="${result},${required}"
    printf '%s' "$result"
}

function argocd_preservation_load() {
    local raw rv sync compare
    raw="$(kubectl get secret "$2" --namespace="$1" \
        -o 'jsonpath={.metadata.resourceVersion}{"|"}{.metadata.annotations.argocd\.argoproj\.io/sync-options}{"|"}{.metadata.annotations.argocd\.argoproj\.io/compare-options}')" || return 5
    # The delimiter and newlines are not valid option tokens. A single named
    # metadata snapshot binds the options and optimistic write precondition.
    [[ "$raw" != *$'\n'* && "$raw" == *"|"*"|"* && "$raw" != *"|"*"|"*"|"* ]] || return 3
    rv="${raw%%|*}"
    raw="${raw#*|}"
    sync="${raw%%|*}"
    compare="${raw#*|}"
    [[ "$rv" =~ ^[0-9]+$ ]] || return 3
    ARGO_PRESERVATION_RV="$rv"
    ARGO_PRESERVATION_SYNC="$(argocd_preservation_options sync "$sync")" || return 3
    ARGO_PRESERVATION_COMPARE="$(argocd_preservation_options compare "$compare")" || return 3
    ARGO_PRESERVATION_CHANGED=false
    if [[ "$ARGO_PRESERVATION_SYNC" != "$sync" || "$ARGO_PRESERVATION_COMPARE" != "$compare" ]]; then
        ARGO_PRESERVATION_CHANGED=true
    fi
    return 0
}

function argocd_preservation_resource_version() {
    if [[ -n "$ARGO_PRESERVATION_RV" ]]; then
        # Client-side only. The seeder sends this entire merge-patch document;
        # client-side apply may drop an unchanged resourceVersion from its diff.
        kubectl patch --local --type=merge -f - \
            -p "{\"metadata\":{\"resourceVersion\":\"${ARGO_PRESERVATION_RV}\"}}" -o yaml
    else
        cat
    fi
    return $?
}

function argocd_preservation_manifest() {
    kubectl annotate --local --overwrite -f - \
        "argocd.argoproj.io/sync-options=${ARGO_PRESERVATION_SYNC}" \
        "argocd.argoproj.io/compare-options=${ARGO_PRESERVATION_COMPARE}" -o yaml \
        | argocd_preservation_resource_version
}

function argocd_preservation_write_secret() {
    if [[ -n "$ARGO_PRESERVATION_RV" ]]; then
        # Keep unmentioned operator-owned keys and metadata. An explicit merge
        # patch carries resourceVersion even when the client's current read
        # would otherwise consider it unchanged. Never use replace or force.
        kubectl patch secret "$2" --namespace="$1" --type=merge --patch-file=/dev/stdin
    else
        # A concurrent creator must cause a conflict, never receive credentials
        # generated after an earlier observation that this Secret was absent.
        kubectl create -f -
    fi
    return $?
}
