package pipelinesteps

// The three scripts a step's pod runs. No uid is shared by every container
// that touches the same files -- the clone and cache-prep run as root, the
// step as the step image's own user, root for one image and uid 1000 for the
// toolchain -- so each script says what it does about that (rulings R13, R13b,
// R15b):
//
//   - cache-prep opens up the step's cache directory -- its owner's, its
//     repository's, its run's trust's -- the one directory every uid of the
//     steps that share it has to be able to enter: world-writable and sticky,
//     so a uid can create its own tree there but never rename or replace
//     another uid's.
//   - the clone clears its umask, so the checkout in the pod's own scratch
//     volume is writable by whichever uid the step runs as.
//   - the step wrapper changes NO umask: the user's command inherits the one
//     the container starts with, because tools refuse files others can write
//     (OpenSSH a world-writable ~/.ssh/config, MySQL ~/.my.cnf). Instead it
//     gives each uid its own tree of the step's cache, so steps of different
//     uids never write into each other's files.
//
// The per-uid cache variables, and the git configuration BuildJob gives the
// step, are the cluster substrate's implementation details, not part of the
// MEMQL_* contract (pl.StepRequest.Environment), so a step must not rely on
// them.

// stepWrapper runs the command under its original umask, then writes a raw
// archive into the pod's export volume. No artifact bytes enter container logs.
// The collector mounts only this volume, read-only, and the runner reads it only
// after this container has terminated. A failed tar leaves no completed archive.
// The command's exit code remains its own; missing required artifacts are a
// separate runner failure. Globs have already passed artifactProblem.
const stepWrapper = `cd /workspace || exit 70
if [ "${MEMQL_CACHE_LOCKED:-0}" = 1 ]; then
  command -v flock >/dev/null 2>&1 || { echo "memql: cached step images must provide flock" >&2; exit 70; }
  # Parent holds the lease until the command and artifact export finish.
  # New users cannot overtake a collector waiting for existing users to drain.
  exec 8</memql-cache-gate 9</memql-cache-users || exit 70
  flock -x 8 && flock -s 9 && flock -u 8 || exit 70
fi
if [ -n "${MEMQL_CACHES:-}" ] && u=$(id -u 2>/dev/null) && [ -n "$u" ]; then
  for c in $MEMQL_CACHES; do
    case "$c" in
      go) export GOMODCACHE="/cache/go-u$u/mod" GOCACHE="/cache/go-u$u/build" ;;
      npm) export npm_config_cache="/cache/npm-u$u" ;;
    esac
  done
fi
if [ -n "${MEMQL_STEP_RUN_DEADLINE:-}" ]; then
  # The absolute run ceiling still applies after resource admission waits,
  # even if every runner disconnects. Refuse images missing these utilities.
  command -v timeout >/dev/null 2>&1 && command -v date >/dev/null 2>&1 || {
    echo "memql: queued steps require date and timeout" >&2; exit 70;
  }
  now=$(date +%s) || exit 70
  remaining=$((MEMQL_STEP_RUN_DEADLINE - now))
  [ "$remaining" -gt 0 ] || { echo "memql: run deadline passed before execution" >&2; exit 124; }
  timeout -k 10 "$remaining" /bin/sh -c "$MEMQL_STEP_COMMAND"
else
  /bin/sh -c "$MEMQL_STEP_COMMAND"
fi
rc=$?
if [ -n "${MEMQL_STEP_ARTIFACTS:-}" ]; then
  # This mask applies only to the exported archive, after the command ended.
  # The credential-free collector can read it regardless of the command uid.
  (umask 0022
   COPYFILE_DISABLE=1 tar -cf /memql-artifacts/pending.tar -- $MEMQL_STEP_ARTIFACTS 2>/dev/null &&
   chmod 0644 /memql-artifacts/pending.tar &&
   mv /memql-artifacts/pending.tar /memql-artifacts/completed.tar)
fi
exit $rc`

// cloneScript is the clone init container's entrypoint: a shallow fetch of
// exactly one commit, by its id, into /workspace, world-writable (see above).
// Declared full history fetches ancestors, tags and the comparison base here;
// arbitrary step commands never receive the clone token.
// Network failures retry the identical fetch at most three times within the
// existing Job deadline. No checkout or build command is retried.
//
// The token, when there is one, goes to GitHub as the password of user
// x-access-token in an http.extraheader for this one command, so it is never
// written to the repository's config, never part of a URL a git error would
// print, and never echoed. CLONE_URL, SHA and GIT_TOKEN arrive in the
// environment; BuildJob has already refused a SHA that is not a full object id
// and a clone URL that is not plain https.
const cloneScript = `umask 0000
set -eu
cd /workspace
git init -q .
fetch_pinned_commit() {
  case "${CLONE_HISTORY:-}" in
    "") set -- --depth=1 "$CLONE_URL" "$SHA" ;;
    full)
      set -- --tags "$CLONE_URL" "$SHA"
      if [ -n "${BASE_SHA:-}" ]; then set -- "$@" "$BASE_SHA"; fi
      ;;
    *) echo "memql: unsupported checkout history" >&2; return 2 ;;
  esac
  if [ -n "${GIT_TOKEN:-}" ]; then
    auth="$(printf 'x-access-token:%s' "$GIT_TOKEN" | base64 | tr -d '\n')"
    git -c "http.extraheader=AUTHORIZATION: basic ${auth}" fetch -q "$@"
  else
    git fetch -q "$@"
  fi
}
for attempt in 1 2 3; do
  if fetch_pinned_commit; then
    break
  else
    status=$?
  fi
  if [ "$attempt" -eq 3 ]; then
    exit "$status"
  fi
  echo "memql: pinned commit fetch failed; retrying ($attempt/3)" >&2
  sleep 2
done
if [ "${CLONE_HISTORY:-}" = full ]; then
  [ "$(git rev-parse --is-shallow-repository)" = false ] || { echo "memql: full history is incomplete" >&2; exit 1; }
fi
git checkout -q "$SHA"
echo "memql: checked out $SHA"`

// cachePrepScript is fixed platform code, with no credentials or checkout.
// It reclaims disposable caches only under an exclusive cross-pod file lock.
// The lock inodes are never replaced, including during cleanup. The step sees
// only its own scope plus read-only mounts of those two files.
const cachePrepScript = `set -eu
umask 0022
root=/cache-root
if [ "${CACHE_GC_PERCENT:-0}" = 0 ]; then
  umask 0000
  if ! mkdir -p "$CACHE_DIR" || ! chmod 1777 "$CACHE_DIR"; then
    echo "memql: cannot prepare the cache directory $CACHE_DIR" >&2
    exit 1
  fi
  exit 0
fi
locks="$root/.memql-locks"
[ ! -L "$locks" ] || { echo "memql: cache lock directory is a symlink" >&2; exit 70; }
mkdir -p "$locks"
for name in gate users; do
  file="$locks/$name"
  [ ! -L "$file" ] && { [ ! -e "$file" ] || [ -f "$file" ]; } || exit 70
  # Append opens the same inode; never truncate, rename or unlink a lock.
  (umask 022; : >>"$file")
  chmod 0644 "$file"
done
exec 8<"$locks/gate" 9<"$locks/users"
flock -x 8
pressure() {
  bytes=$(df -Pk "$root" | awk 'NR==2 {gsub(/%/, "", $5); print $5}')
  inodes=$(df -Pi "$root" | awk 'NR==2 {gsub(/%/, "", $5); print $5}')
  for used in "$bytes" "$inodes"; do
    case "$used" in ""|*[!0-9]*) echo "memql: cannot measure cache capacity" >&2; exit 70;; esac
  done
  [ "$bytes" -ge "$CACHE_GC_PERCENT" ] || [ "$inodes" -ge "$CACHE_GC_PERCENT" ]
}
# Validate each platform-owned ancestor. Step content is considered only below
# a scope root. find/rm never follow symlinks there; no writer holds a lease.
scopes() {
  for owner in "$root"/owners/*; do
    [ -d "$owner" ] || continue
    key=${owner##*/}
    case "$key" in *[!0-9a-f]*) continue;; esac
    [ "${#key}" -eq 24 ] || continue
    for repo in "$owner"/repos/*; do
      [ -d "$repo" ] || continue
      key=${repo##*/}
      case "$key" in *[!0-9a-f]*) continue;; esac
      [ "${#key}" -eq 24 ] || continue
      for trust in trusted untrusted; do
        scope="$repo/$trust"
        [ -d "$scope" ] || continue
        [ "$(realpath -e "$scope")" = "$scope" ] || { echo "memql: refusing linked cache scope" >&2; exit 70; }
        if [ "$1" = build ]; then
          find "$scope" -mindepth 2 -maxdepth 2 -type d -path "$scope/go-u*/build" -exec rm -rf -- {} +
        else
          find "$scope" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
        fi
      done
    done
  done
}
if pressure; then
  echo "memql: cache capacity threshold reached; waiting for active cached steps"
  flock -x 9
  # Another initializer may already have reclaimed space while we waited.
  if pressure; then
    scopes build
    if pressure; then scopes all; fi
    echo "memql: disposable cache reclaimed"
    if pressure; then echo "memql: cache volume remains above its capacity threshold" >&2; exit 70; fi
  fi
fi
case "$CACHE_DIR" in "$root"/owners/*/repos/*/trusted|"$root"/owners/*/repos/*/untrusted) ;; *) echo "memql: invalid cache directory" >&2; exit 70;; esac
mkdir -p "$CACHE_DIR"
[ "$(realpath -e "$CACHE_DIR")" = "$CACHE_DIR" ] || { echo "memql: refusing linked cache directory" >&2; exit 70; }
chmod 1777 "$CACHE_DIR"`
