#!/usr/bin/env bash
# Library: ports.sh -- one answer to "is this TCP port free, and what holds it?".
#
# Usage:   sourced, never executed. The caller owns `set -euo pipefail`.
#
# WHY ONE COPY. `install.detect` REPORTS whether the ingress ports are free and
# `scripts/k3d/up.sh` REFUSES on the same question before it binds them. Two
# probes would be two opinions about one machine, and the one that mattered
# would be whichever ran last -- the failure mode scripts/lib/docker.sh's header
# records for the docker probe.
#
# WHY IT EXISTS AT ALL. k3d publishes 80, 443 and 5432 on the HOST. A machine
# already serving one of them fails inside `k3d cluster create` with docker's
# "port is already allocated" and no interpretation -- and on macOS that is a
# common shape, because `brew services start postgresql` holds 5432 and is a
# normal thing for a developer's Mac to be doing. The same box is rare on a
# Linux dev machine, which is why this went unnoticed.
#
# Portability: bash 3.2 (stock macOS) -- no associative arrays, no `mapfile`,
#              no `${var,,}`. See scripts/lib/bash_portability_test.go.

# port_free <port> -- 0 when NOTHING is listening. The polarity is "available
# to bind", which is what both callers need to decide.
#
# TWO PROBES, because neither alone is right. `ss` sees every listening socket
# including ones bound to addresses this host cannot dial; the /dev/tcp connect
# is the only probe available where `ss` is not (macOS has none), and it is
# also the one that catches a loopback-only bind. A connect that SUCCEEDS is
# the unambiguous answer, so it runs even when ss said nothing.
function port_free() {
    local port="$1"
    if command -v ss &>/dev/null; then
        if ss -ltnH 2>/dev/null | grep -qE "[:.]${port}[[:space:]]"; then
            return 1
        fi
    fi
    if (exec 3<>"/dev/tcp/127.0.0.1/${port}") 2>/dev/null; then
        return 1
    fi
    return 0
}

# port_holder <port> -- best-effort "name (pid N)" of what is listening, or the
# empty string.
#
# BEST-EFFORT ON PURPOSE, and the caller must read an empty answer as "unknown"
# rather than "nothing". A refusal that can name the process turns "port 5432 is
# taken" into something the operator can act on without going and looking; a
# refusal that cannot is still correct, because port_free already decided.
function port_holder() {
    local port="$1" line=""
    if command -v lsof &>/dev/null; then
        # macOS and most Linux. -nP keeps it from resolving names, which is
        # what makes it fast enough to run on a preflight.
        line="$(lsof -nP -iTCP:"${port}" -sTCP:LISTEN 2>/dev/null | awk 'NR==2 {print $1 " (pid " $2 ")"}')"
    fi
    if [[ -z "$line" ]] && command -v ss &>/dev/null; then
        line="$(ss -ltnpH 2>/dev/null | grep -E "[:.]${port}[[:space:]]" | head -1 |
            sed -n 's/.*users:((\"\([^\"]*\)\",pid=\([0-9]*\).*/\1 (pid \2)/p')"
    fi
    printf '%s' "$line"
}
