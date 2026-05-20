package id

import "github.com/google/uuid"

// NewShortId returns a fresh opaque short identifier.
//
// Two use cases share this function:
//
//   - The {shortId} segment of a memQL canonical node id
//     ({partition}:{concept}:{shortId}), for INSTANCE rows --
//     anything user-created or runtime-created where the row is one
//     of many. CATALOG rows (agentRole, knowledgeDomain, nodeType --
//     a fixed-ish set keyed by stable slug) use Slugify(name)
//     instead.
//   - Correlation ids for in-flight wire messages (gRPC RequestId /
//     MessageId, attachment object names, etc.) -- the format is the
//     same, the use case is "I need a fresh opaque token nothing
//     else will collide with."
//
// Centralized so the format (currently a 36-char UUIDv4 string) can
// change later without touching every minting site. Every caller in
// memql core + integrations that needs a fresh opaque id should go
// through this function instead of calling uuid.NewString() or
// hex-encoding random bytes inline.
func NewShortId() string {
	return uuid.NewString()
}
