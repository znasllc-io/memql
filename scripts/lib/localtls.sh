#!/usr/bin/env bash
#
# scripts/lib/localtls.sh
# =======================
#
# ONE definition of where the local front door's TLS material lives, shared by
# the two scripts that touch it:
#
#   scripts/install/mkcert-setup.sh   issues the pair (install.mkcert)
#   scripts/k3d/seed-secrets.sh       loads it into the cluster as a k8s Secret
#
# WHY THIS FILE EXISTS AT ALL. The bug it closes (memql#3384) was precisely a
# drifted default: both scripts independently defaulted to a dev.{crt,key} pair
# inside the repo's old `docker/` tree, which was deleted when the Compose local
# stack was retired (memql#2068 / #2088). The path could no
# longer resolve on any clean checkout, so seed-secrets skipped the secret with
# a WARN, traefik fell back to its own "TRAEFIK DEFAULT CERT" for both
# front-door hosts, and `make up` still printed a green summary. Two copies of
# a filesystem constant is how that happens; one copy is the fix.
#
# The location is under $HOME, not the repo: the pair is per-OPERATOR machine
# state (it is signed by that machine's mkcert CA and is only trusted there),
# it must survive `git clean`, and it must be shared by every checkout /
# worktree of the repo on that machine. $HOME/.memql/ is where the local
# cluster already keeps operator state (genesis.znas, worker.yaml).
#
# This is NOT a capability script -- it declares constants and is sourced.

# Guard against double-sourcing (a caller may source it via two paths).
if [[ -n "${_MEMQL_LOCALTLS_LIB_LOADED:-}" ]]; then
    return 0 2>/dev/null || exit 0
fi
_MEMQL_LOCALTLS_LIB_LOADED=1

# $HOME is set in every interactive and CI shell we support; the fallback only
# keeps `set -u` from aborting in a stripped environment.
_MEMQL_LOCALTLS_HOME="${HOME:-${TMPDIR:-/tmp}}"

# Where the front-door pair lives. Operators override per-invocation with
# MEMQL_LOCAL_TLS_CERT / MEMQL_LOCAL_TLS_KEY (which feed the --tls-cert /
# --tls-key defaults), not by editing this.
MEMQL_LOCAL_TLS_DIR="${_MEMQL_LOCALTLS_HOME}/.memql/certs"
MEMQL_LOCAL_TLS_DEFAULT_CERT="${MEMQL_LOCAL_TLS_DIR}/dev.crt"
MEMQL_LOCAL_TLS_DEFAULT_KEY="${MEMQL_LOCAL_TLS_DIR}/dev.key"

# The domain the local front door is served at (memql#3593).
#
# ONE value: the certificate SANs below, the hosts block, the two Ingress
# hostnames and identity's issuer are all derived from it. Kept in step with
# deploy/k8s/overlays/local, whose Ingresses carry the same apex as their
# committed default.
#
# Overridden per invocation with --domain. This variable is the flag's DEFAULT,
# not a second input path -- the capability contract gives cap_param no env
# tier, so a script resolves an env value and passes it as the default.
MEMQL_LOCAL_DOMAIN="${MEMQL_LOCAL_DOMAIN:-memql.localhost}"

# The names the certificate must carry. The wildcard covers cockpit. /
# identity. / anything else the local overlay adds; the apex is listed
# separately because a wildcard label does not match it.
MEMQL_LOCAL_TLS_HOSTNAMES="*.${MEMQL_LOCAL_DOMAIN},${MEMQL_LOCAL_DOMAIN}"

# The k8s Secret both local front-door ingresses name in their spec.tls.
#
# RENAMED from local-znas-tls (memql#3593). The old name embedded one company's
# domain in a secret every install creates. The name is a fact about the front
# door, not about whose domain it happens to serve.
MEMQL_LOCAL_TLS_SECRET="memql-front-door-tls"
