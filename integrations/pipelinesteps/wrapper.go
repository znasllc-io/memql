package pipelinesteps

// stepWrapper is the step container's entrypoint (`/bin/sh -c stepWrapper`).
// It runs the manifest's command and, when artifacts are declared, frames them
// on stdout as base64 of a tar.gz between two marker lines, which the capture
// lifts out of the log. POSIX sh only; tar and base64 are needed only when
// artifacts are declared. The step's own exit status is what the container
// exits with, whatever the framing does: an image without tar loses its
// artifacts (a note), never its outcome.
//
// The command arrives in MEMQL_STEP_COMMAND rather than in the script text, so
// nothing about it is ever quoted, escaped or interpolated here.
const stepWrapper = `cd /workspace || exit 70
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
// exactly one commit, by its id, into /workspace.
//
// The token, when there is one, goes to GitHub as the password of user
// x-access-token in an http.extraheader for this one command, so it is never
// written to the repository's config, never part of a URL a git error would
// print, and never echoed. CLONE_URL, SHA and GIT_TOKEN arrive in the
// environment; BuildJob has already refused a SHA that is not a full object id
// and a clone URL that is not plain https.
const cloneScript = `set -eu
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
