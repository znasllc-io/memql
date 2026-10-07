package installation

import "time"

// Observation age begins before any external read, not when a verifier returns.
// This ceiling is a native gate; moving a preservation call earlier in a DSL
// recipe cannot make its result valid indefinitely. Digests remain stable over
// unchanged resources, and persisted digests cannot reconstruct this evidence.
func freshPreservationObservation(observed, now time.Time) bool {
	return !observed.IsZero() && !observed.After(now) && now.Sub(observed) <= time.Minute
}
