package memoryNodes

import "fmt"

// Concept ID constants provide compile-time safe references to concept identifiers.
// These map directly to the filesystem layout under concepts/v1/{domain}/{entity}/
// where path segments are joined with colons to form the ID.
//
// Constants are grouped by domain and sorted alphabetically within each group.

// Cognition domain concepts (v1:cognition:*)
const (
	ConceptCognitionParticipant         = "v1:cognition:participant"
	ConceptCognitionParticipantPresence = "v1:cognition:participant:presence"
	ConceptCognitionSession             = "v1:cognition:session"
	ConceptCognitionSpace               = "v1:cognition:space"
	ConceptCognitionSpaceContext        = "v1:cognition:space:context"
	ConceptCognitionTextChunk           = "v1:cognition:text:chunk"
	ConceptCognitionTurnState           = "v1:cognition:turn:state"
	ConceptCognitionUtterance           = "v1:cognition:utterance"
)

// Data domain concepts (v1:data:*)
const (
	ConceptDataRecord = "v1:data:record"
	ConceptDataPolicy = "v1:data:policy"
	ConceptDataLog    = "v1:data:log"
)

// Harness domain concepts (v1:harness:*) -- the MemQL-native agent
// harness working-state spine (epic #590, foundational issue #582).
const (
	ConceptHarnessPlan        = "v1:harness:plan"
	ConceptHarnessStep        = "v1:harness:step"
	ConceptHarnessObservation = "v1:harness:observation"
)

// Authoring domain concepts (v1:authoring:*) -- the graph-stored
// substrate for planner-authored DSL bundles (epic #954). The construct
// row is the per-construct member of a bundle and the catalog's reuse
// unit (#957): a promoted construct grows a content vector in
// node_vectors keyed by its id so similarTo can rank near-matches.
const (
	ConceptAuthoringConstruct = "v1:authoring:construct"
)

// Platform domain concepts (v1:platform:*)
const (
	ConceptPlatformGlobalSecret      = "v1:platform:globalSecret"
	ConceptPlatformGlobalVariable    = "v1:platform:globalVariable"
	ConceptPlatformPartitionSecret   = "v1:platform:partitionSecret"
	ConceptPlatformPartitionVariable = "v1:platform:partitionVariable"
)

// Identity domain concepts (v1:identity:*)
const (
	ConceptIdentityUser        = "v1:identity:user"
	ConceptIdentityIdentity    = "v1:identity:identity"
	ConceptIdentityDelegation  = "v1:identity:delegation"
	ConceptIdentityAuthSession = "v1:identity:authSession"
)

// Cluster domain concepts (v1:cluster:*)
const (
	ConceptClusterNode       = "v1:cluster:node"
	ConceptClusterNodeType   = "v1:cluster:nodeType"
	ConceptClusterSpawnEvent = "v1:cluster:spawnEvent"
)

// MemQL internal concepts (v1:memql:*) -- engine-internal state, not
// user-facing config. User-facing secrets / variables live under
// v1:platform:* (see Platform domain above).
const (
	ConceptMemQLCheckpoint = "v1:memql:checkpoint"
)

// MemQL runtime concepts -- used in Go code but registered dynamically
// (no concept directory on disk).
const (
	ConceptMemQLAutomationRun  = "v1:memql:automation:run"
	ConceptMemQLAutomationStep = "v1:memql:automation:step"
	ConceptMemQLBackendUser    = "v1:memql:backend:user"
)

// AllFilesystemConcepts returns all concept IDs that have corresponding
// concept directories on disk. Used for startup validation.
func AllFilesystemConcepts() []string {
	return []string{
		// cognition
		ConceptCognitionParticipant,
		ConceptCognitionParticipantPresence,
		ConceptCognitionSession,
		ConceptCognitionSpace,
		ConceptCognitionSpaceContext,
		ConceptCognitionTextChunk,
		ConceptCognitionTurnState,
		ConceptCognitionUtterance,
		// data
		ConceptDataRecord,
		ConceptDataPolicy,
		ConceptDataLog,
		// platform
		ConceptPlatformGlobalSecret,
		ConceptPlatformGlobalVariable,
		ConceptPlatformPartitionSecret,
		ConceptPlatformPartitionVariable,
		// identity
		ConceptIdentityUser,
		ConceptIdentityIdentity,
		ConceptIdentityDelegation,
		// cluster
		ConceptClusterNode,
		ConceptClusterNodeType,
		ConceptClusterSpawnEvent,
		// memql (filesystem-backed only)
		ConceptMemQLCheckpoint,
	}
}

// ValidateConceptConstants checks that every filesystem-backed concept constant
// has a corresponding entry in the registry. Call after LoadConcepts to catch
// drift between constants and the concepts/ directory at startup.
func ValidateConceptConstants(registry Registry) error {
	var missing []string
	for _, id := range AllFilesystemConcepts() {
		if _, err := registry.Get(id); err != nil {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("concept constants reference %d unregistered concepts: %v", len(missing), missing)
	}
	return nil
}
