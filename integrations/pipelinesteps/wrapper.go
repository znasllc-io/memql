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

// stepWrapper is the step container's entrypoint (`/bin/sh -c stepWrapper`).
// It runs the manifest's command and, when artifacts are declared, frames them
// on stdout as base64 of a tar.gz between two marker lines, which the capture
// lifts out of the log. POSIX sh only; tar and base64 are needed only when
// artifacts are declared, and id only when caches are. The step's own exit
// status is what the container exits with, whatever the framing does: an image
// without tar loses its artifacts (a note), never its outcome.
//
// MEMQL_CACHES names the declared caches. For each, the wrapper points the
// tool at a tree of the step's cache that belongs to the uid the step runs as
// (/cache/go-u<uid>, /cache/npm-u<uid>); a step declaring no cache keeps
// whatever its image sets. A step whose uid cannot be determined (no id in the
// image) runs uncached: guessing a uid would put it among another uid's files,
// where it fails rather than misses.
//
// The begin marker is printed after a newline (ruling R18): a command whose
// last output has no trailing newline would otherwise glue the marker to its
// last words, and the capture matches the marker as a whole line. One blank
// log line is the price.
//
// The command arrives in MEMQL_STEP_COMMAND rather than in the script text, so
// nothing about it is ever quoted, escaped or interpolated here.
const stepWrapper = `cd /workspace || exit 70
if [ -n "${MEMQL_CACHES:-}" ] && u=$(id -u 2>/dev/null) && [ -n "$u" ]; then
  for c in $MEMQL_CACHES; do
    case "$c" in
      go) export GOMODCACHE="/cache/go-u$u/mod" GOCACHE="/cache/go-u$u/build" ;;
      npm) export npm_config_cache="/cache/npm-u$u" ;;
    esac
  done
fi
/bin/sh -c "$MEMQL_STEP_COMMAND"
rc=$?
if [ -n "${MEMQL_STEP_ARTIFACTS:-}" ]; then
  printf '\n%s begin\n' "$MEMQL_ARTIFACT_MARKER"
  # Word-split on purpose: the list is validated server-side (no spaces, no metacharacters,
  # no glob that can reach . or ..) and globs are allowed to expand.
  COPYFILE_DISABLE=1 tar -czf - -- $MEMQL_STEP_ARTIFACTS 2>/dev/null | base64
  echo "$MEMQL_ARTIFACT_MARKER end"
fi
exit $rc`

// cloneScript is the clone init container's entrypoint: a shallow fetch of
// exactly one commit, by its id, into /workspace, world-writable (see above).
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
  if [ -n "${GIT_TOKEN:-}" ]; then
    auth="$(printf 'x-access-token:%s' "$GIT_TOKEN" | base64 | tr -d '\n')"
    git -c "http.extraheader=AUTHORIZATION: basic ${auth}" fetch -q --depth=1 "$CLONE_URL" "$SHA"
  else
    git fetch -q --depth=1 "$CLONE_URL" "$SHA"
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
git checkout -q FETCH_HEAD
echo "memql: checked out $SHA"`

// cachePrepScript is the cache-prep init container's entrypoint (ruling
// R15b). CACHE_DIR names the step's directory of the cache claim
// (CacheSubPath), whose root only this container mounts. It creates the
// directory, and those above it, then makes it world-writable and sticky
// (1777), or opens up one created narrower before -- kubelet creates a missing
// subPath root-owned with the claim root's mode -- so the step, which mounts
// only that directory, can enter it as whatever uid its image runs as and
// create its own uid's tree, while the sticky bit keeps one uid from renaming
// or replacing another uid's. Nothing else: no git, no token, no checkout. A
// directory that cannot be prepared fails the container with a line naming
// it, which FallbackToLogsOnError carries into terminated.message.
const cachePrepScript = `umask 0000
mkdir -p "$CACHE_DIR" && chmod 1777 "$CACHE_DIR" || { echo "memql: cannot prepare the cache directory $CACHE_DIR" >&2; exit 1; }`
