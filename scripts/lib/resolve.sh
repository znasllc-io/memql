#!/usr/bin/env bash
#
# scripts/lib/resolve.sh
# ======================
#
# ONE definition of how this repository resolves a hostname to IPv4 addresses,
# shared by the two scripts that need to know:
#
#   scripts/install/verify-frontdoor.sh   the front-door DNS check
#   scripts/install/hosts-entries.sh      the probe that decides whether the
#                                         hosts block needs writing at all
#
# WHY THIS FILE EXISTS. It is the localtls.sh lesson applied before it bites.
# Two scripts independently spelling the same rule is how memql#3384 happened:
# both defaulted to the same certificate path, the path moved, and one of them
# went on reporting success. The hosts probe and the front-door verifier must
# agree about what "resolves to 127.0.0.1" means, because one of them decides
# whether to write the entry the other then checks.
#
# TESTING HOOK. MEMQL_RESOLVE_STUB names a directory of files, one per hostname,
# each holding the addresses that name resolves to. When it is set,
# resolve_addresses reads the directory instead of the network -- which is what
# makes the three-outcome probe testable without DNS. A missing file means the
# name does not resolve. Never set on a real run.
#
# This is NOT a capability script -- it declares functions and is sourced.

# Guard against double-sourcing (a caller may source it via two paths).
if [[ -n "${_MEMQL_RESOLVE_LIB_LOADED:-}" ]]; then
    return 0 2>/dev/null || exit 0
fi
_MEMQL_RESOLVE_LIB_LOADED=1

# resolver_tool -- names the resolver this machine actually has. getent is the
# Linux/glibc path; dig and host cover macOS, where getent does not exist.
function resolver_tool() {
    if [[ -n "${MEMQL_RESOLVE_STUB:-}" ]]; then printf 'stub';   return; fi
    if command -v getent &>/dev/null;            then printf 'getent'; return; fi
    if command -v dig    &>/dev/null;            then printf 'dig';    return; fi
    if command -v host   &>/dev/null;            then printf 'host';   return; fi
    printf ''
}

# resolve_addresses <host> -- prints each resolved IPv4 address on its own
# line. Empty output means the name did not resolve.
function resolve_addresses() {
    local host="$1"
    case "$(resolver_tool)" in
        stub)
            if [[ -f "${MEMQL_RESOLVE_STUB}/${host}" ]]; then
                grep -E '^[0-9]+(\.[0-9]+){3}$' "${MEMQL_RESOLVE_STUB}/${host}" | sort -u
            fi
            ;;
        getent) getent ahostsv4 "$host" 2>/dev/null | awk '{print $1}' | sort -u ;;
        dig)    dig +short A "$host" 2>/dev/null | grep -E '^[0-9]+(\.[0-9]+){3}$' | sort -u ;;
        host)   host -t A "$host" 2>/dev/null | awk '/has address/ {print $NF}' | sort -u ;;
    esac
    # Never propagate a non-zero status from the pipeline above: "did not
    # resolve" is an empty result, not an error, and every caller runs under
    # `set -e`.
    return 0
}
