package pipelinerun

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// ids.go -- the ids this package derives, and the gate keys every
// read-modify-write is serialized on.

// PipelineIDFor is the id of a source's one pipeline (plan decision 12): a
// stable BARE id derived from the package's short id, so connecting twice is
// the same row and reconnecting is a new version of it rather than a second
// pipeline.
func PipelineIDFor(packageID string) string {
	return deriveID("pipelines.pipeline", bareID(packageID))
}

// RunIDFor is the id of attempt `attempt` of a run key, for one pipeline.
// Derived rather than minted so a second open of the SAME attempt lands on the
// same row -- whose lifecycle fields createPipelineRun declares @createOnly --
// instead of beside it: the gate is what keeps one head one run, and this is
// what holds if it ever did not. The PIPELINE is part of it because the run
// key is not: a disconnected predecessor's run of the same key is a different
// run, and must stay a different row.
func RunIDFor(pipelineID, runKey string, attempt int) string {
	return deriveID("pipelines.run", bareID(pipelineID)+"\x00"+runKey+"\x00"+strconv.Itoa(attempt))
}

// deriveID is a bare, hex short id: what a mutation's id argument takes (the
// engine canonicalizes on the way in), and safe for inputs that are
// themselves canonical ids full of colons.
func deriveID(kind, key string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + key))
	return hex.EncodeToString(sum[:16])
}

// The gate keys: THREE, and NO KEY IS EVER TAKEN WHILE ANOTHER IS HELD.
//
// The production gate holds a connection of the DIRECT database pool for its
// whole section -- a pool of four, one or two of which an agent's cron
// leaders already hold, whose waiters give up after five seconds
// (githubconnect.WithGate). A nested gate holds two of those connections,
// and two replicas nesting two keys in opposite orders wait on each other
// until both time out. So every section is flat, short and local: a fresh
// read and its writes, with GitHub asked before the gate is taken -- the one
// call a section makes to GitHub is open()'s check-run create, which open.go
// says why. fakes_test.go holds every test in this package to both rules.

// RepositoryGateKey serializes everything that writes a repository's
// pipeline row: a connect (whose "one pipeline per repository" check and the
// write it guards are one section), a disconnect, the poll's heads, and the
// driver's timings merge. ONE key for the row and for the repository,
// derived from the repository, because a repository has exactly one active
// pipeline: two keys -- one per row, one per repository -- were what a
// connect had to nest.
func RepositoryGateKey(repository string) string {
	return "pipelines.repository:" + normalizeRepository(repository)
}

// OpenGateKey serializes the opening of a run key: the dedup read and the
// create are one critical section, so a redelivered webhook and a poll for
// the same head cannot both find nothing and both open (Review Focus 1).
func OpenGateKey(runKey string) string { return "pipelines.open:" + runKey }

// RunGateKey serializes every read-modify-write of an opened run: the
// driver's claim, heartbeat and conclusion (Task 10b), a cancel request, and
// the recording of a check-run write made outside a drive. One key for all
// of them, so a cancel and a claim cannot both decide from the same stale
// row.
func RunGateKey(runID string) string { return "pipelines.drive:" + bareID(runID) }
