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

// RunIDFor is the id of attempt `attempt` of a run key. Derived rather than
// minted so a second open of the SAME attempt lands on the same row -- whose
// lifecycle fields createPipelineRun declares @createOnly -- instead of
// beside it: the gate is what keeps one head one run, and this is what holds
// if it ever did not.
func RunIDFor(runKey string, attempt int) string {
	return deriveID("pipelines.run", runKey+"\x00"+strconv.Itoa(attempt))
}

// deriveID is a bare, hex short id: what a mutation's id argument takes (the
// engine canonicalizes on the way in), and safe for inputs that are
// themselves canonical ids full of colons.
func deriveID(kind, key string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + key))
	return hex.EncodeToString(sum[:16])
}

// The gate keys. Three, and a fixed order between them, so no two replicas
// can each hold one the other is waiting for: a holder of a pipeline's key
// may take an open key (the poll opens runs), a holder of an open key takes
// no other, and a run's key is taken alone.

// OpenGateKey serializes the opening of a run key: the dedup read and the
// create are one critical section, so a redelivered webhook and a poll for
// the same head cannot both find nothing and both open (Review Focus 1).
func OpenGateKey(runKey string) string { return "pipelines.open:" + runKey }

// RunGateKey serializes every read-modify-write of an opened run: the
// driver's claim, heartbeat and conclusion (Task 10b) and a cancel request.
// One key for all of them, so a cancel and a claim cannot both decide from
// the same stale row.
func RunGateKey(runID string) string { return "pipelines.drive:" + bareID(runID) }

// PipelineGateKey serializes the read-modify-writes of a pipeline row: the
// poll's heads, a connect, a disconnect, and the timings merge (Task 10b).
func PipelineGateKey(pipelineID string) string { return "pipelines.pipeline:" + bareID(pipelineID) }
