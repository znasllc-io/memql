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

// The gate keys, and the ONE order they are taken in, so no two replicas can
// each hold a key the other is waiting for:
//
//	repository  ->  pipeline  ->  open
//	run (alone)
//
// A connect holds a repository's key and then its pipeline's; the poll holds
// a pipeline's and opens runs under their open keys; an open takes no other
// key; a run's key is taken with no other held.

// RepositoryGateKey serializes connecting a repository, so two sources
// connecting one repository at once cannot both find it free (one pipeline
// per repository).
func RepositoryGateKey(repository string) string {
	return "pipelines.repository:" + normalizeRepository(repository)
}

// PipelineGateKey serializes the read-modify-writes of a pipeline row: the
// poll's heads, a connect, a disconnect, and the timings merge (Task 10b).
func PipelineGateKey(pipelineID string) string { return "pipelines.pipeline:" + bareID(pipelineID) }

// OpenGateKey serializes the opening of a run key: the dedup read and the
// create are one critical section, so a redelivered webhook and a poll for
// the same head cannot both find nothing and both open (Review Focus 1).
func OpenGateKey(runKey string) string { return "pipelines.open:" + runKey }

// RunGateKey serializes every read-modify-write of an opened run: the
// driver's claim, heartbeat and conclusion (Task 10b) and a cancel request.
// One key for all of them, so a cancel and a claim cannot both decide from
// the same stale row.
func RunGateKey(runID string) string { return "pipelines.drive:" + bareID(runID) }
