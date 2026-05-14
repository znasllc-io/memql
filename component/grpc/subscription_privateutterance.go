package memql

import (
	"strings"

	"github.com/visionarys-io/memql/component/auth"
	memorynodes "github.com/visionarys-io/memql/component/database/memory-nodes"
	"github.com/visionarys-io/memql/component/events"
)

// shouldDropPrivateUtteranceForCaller is the per-event isolation gate for
// the two-thread chat model (Phase 1.3 of the chat architecture plan).
//
// The pre-insert guard in component/memql/cognition_privateUtterance_validation
// stamps every v1:cognition:privateUtterance row with a non-empty forUserId
// scoped to the authoring user. This function is the symmetric read-side
// gate: a stream session that subscribes to graph.node.created events
// covering the privateUtterance concept must never see rows whose forUserId
// belongs to a different human.
//
// Why filter here and not via topic rewrite? The graph topic shape is
// graph.node.{action}.{partition}.{concept}, so it carries partition and
// concept but not payload fields. The per-event rewriter
// (scopeGraphPatternToPartition) handles the partition slot. forUserId is
// only available in the event's flattened payload, so the gate has to be
// payload-aware. Doing it at delivery time keeps the rewriter simple and
// makes the filter trivially extensible to other per-row filter rules in
// later phases.
//
// Returns true to DROP the event for this caller, false to deliver.
//
// Cluster owners bypass the filter -- the admin / cockpit context relies
// on cross-user visibility, mirroring how IsClusterOwner bypasses the
// per-partition ACL in CanReadPartition.
//
// A privateUtterance event with empty/missing forUserId is dropped on
// principle: it should never have been written (the pre-insert guard
// rejects it), but if one slipped in via a bug or future migration, the
// safest behavior is to make it invisible to non-elevated readers rather
// than leak it as default-allow.
func shouldDropPrivateUtteranceForCaller(event events.Event, callerUserId string, access *auth.AccessContext) bool {
	if !isPrivateUtteranceTopic(event.Topic) {
		return false
	}
	if access != nil && access.IsClusterOwner() {
		return false
	}

	rawForUser, _ := event.Payload["forUserId"].(string)
	forUserId := strings.TrimSpace(rawForUser)
	if forUserId == "" {
		return true
	}

	caller := strings.TrimSpace(callerUserId)
	if caller == "" {
		return true
	}

	return forUserId != caller
}

// isPrivateUtteranceTopic returns true when the topic is a graph node
// event for the privateUtterance concept (the only concept we currently
// gate on). Graph topics are dot-separated and end with the concept id;
// a HasSuffix check is exact enough because concept ids in topics carry
// the literal concept name with no further suffix.
func isPrivateUtteranceTopic(topic string) bool {
	const suffix = "." + memorynodes.ConceptCognitionPrivateUtterance
	return strings.HasSuffix(topic, suffix)
}
