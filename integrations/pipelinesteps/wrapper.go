package pipelinesteps

// Both scripts begin by clearing the umask (ruling R13), because no uid is
// shared by every container that touches the same files. The clone runs as its
// image's user and the step as the step image's own -- root for one image,
// uid 1000 for the toolchain -- so the checkout has to be writable by whoever
// runs next. And one cache volume serves every step, root and non-root alike,
// so a file one step leaves there has to stay writable by the next. The
// checkout lives in the pod's own scratch volume; the cache was already
// writable by any step whose image runs as root.
//
// The umask, and the git configuration BuildJob gives the step, are the
// cluster substrate's implementation details, not part of the MEMQL_* contract
// (pl.StepRequest.Environment), so a step must not rely on either.

// stepWrapper is the step container's entrypoint (`/bin/sh -c stepWrapper`).
// It runs the manifest's command and, when artifacts are declared, frames them
// on stdout as base64 of a tar.gz between two marker lines, which the capture
// lifts out of the log. POSIX sh only; tar and base64 are needed only when
// artifacts are declared. The step's own exit status is what the container
// exits with, whatever the framing does: an image without tar loses its
// artifacts (a note), never its outcome.
//
// The umask is its first statement, so nothing it runs -- the command
// included -- creates a file under the mask the container started with.
//
// The command arrives in MEMQL_STEP_COMMAND rather than in the script text, so
// nothing about it is ever quoted, escaped or interpolated here.
const stepWrapper = `umask 0000
cd /workspace || exit 70
/bin/sh -c "$MEMQL_STEP_COMMAND"
rc=$?
if [ -n "${MEMQL_STEP_ARTIFACTS:-}" ]; then
  echo "$MEMQL_ARTIFACT_MARKER begin"
  # Word-split on purpose: the list is validated server-side (no spaces, no metacharacters)
  # and globs are allowed to expand.
  tar -czf - -- $MEMQL_STEP_ARTIFACTS 2>/dev/null | base64
  echo "$MEMQL_ARTIFACT_MARKER end"
fi
exit $rc`

// cloneScript is the clone init container's entrypoint: a shallow fetch of
// exactly one commit, by its id, into /workspace, world-writable (see above).
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
if [ -n "${GIT_TOKEN:-}" ]; then
  auth="$(printf 'x-access-token:%s' "$GIT_TOKEN" | base64 | tr -d '\n')"
  git -c "http.extraheader=AUTHORIZATION: basic ${auth}" fetch -q --depth=1 "$CLONE_URL" "$SHA"
else
  git fetch -q --depth=1 "$CLONE_URL" "$SHA"
fi
git checkout -q FETCH_HEAD
echo "memql: checked out $SHA"`
