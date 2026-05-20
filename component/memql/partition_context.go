package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/id"
)

type partitionContextKey struct{}

// ContextWithPartition returns a new context carrying the given partition.
// Used for per-request partition override in multi-tenant (Cloud SaaS) mode.
func ContextWithPartition(ctx context.Context, partition string) context.Context {
	return context.WithValue(ctx, partitionContextKey{}, partition)
}

// PartitionFromContext extracts the partition from the context.
// Returns empty string if no partition is set.
func PartitionFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(partitionContextKey{}).(string); ok {
		return v
	}
	return ""
}

// resolvePartition returns the active partition for a request.
// Priority: context override > engine default > "default".
func (e *MemQLEngine) resolvePartition(ctx context.Context) string {
	if ctx != nil {
		if p := PartitionFromContext(ctx); p != "" {
			return p
		}
	}
	return e.Partition()
}

// partitionForConcept returns the partition a read/write should
// target for the given concept. Post-#56 every concept lives in one
// partition (id.DefaultPartition); the per-concept @scope distinction
// is gone. Kept as a single entry point during the multi-step
// partition removal -- once the partition column itself is dropped
// from the Postgres schema (later commit on this branch), every
// caller can just stop asking the question.
func (e *MemQLEngine) partitionForConcept(ctx context.Context, conceptName string) string {
	_ = conceptName
	return e.resolvePartition(ctx)
}

// CanonicalizeIdValue is the exported entry point for callers outside
// the memql package (e.g. the automations executor wiring its
// CanonicalIdResolver). Delegates to canonicalizeIdValue so the
// behavior matrix stays in one place.
func (e *MemQLEngine) CanonicalizeIdValue(ctx context.Context, value, conceptType string) (string, error) {
	return e.canonicalizeIdValue(ctx, value, conceptType)
}

// canonicalizeIdValue normalizes an id-shaped value to the canonical
// `<partition>:<conceptType>:<bareSlug>` form, using the concept's
// @scope to pick the right partition prefix.
//
// Shared by:
//   - canonicalId() builtin in mutation + automation runtime
//     (explicit caller opt-in for derived ids)
//   - canonicalizeRelationshipFields (insert-time auto-canon for
//     payload fields tagged with @relationship)
//
// Behavior:
//
//	value=""                                       -> "" (no-op for missing/optional ids)
//	bare slug                                      -> prepend the concept's partition + type
//	canonical id with matching concept             -> as-is
//	canonical id with matching concept, wrong part -> re-partition to the concept's expected partition
//	canonical id with a DIFFERENT concept          -> error (caller has the wrong type tag)
//
// The "wrong concept" branch errors loudly: silently rewriting it
// could mask real authoring bugs (passing a userId to canonicalId
// (..., "v1:cognition:space")). The other branches always succeed
// -- this is the contract callers depend on for stable id derivation.
func (e *MemQLEngine) canonicalizeIdValue(ctx context.Context, value, conceptType string) (string, error) {
	value = strings.TrimSpace(value)
	conceptType = strings.TrimSpace(conceptType)
	if value == "" {
		return "", nil
	}
	if conceptType == "" {
		return "", fmt.Errorf("canonicalId: concept type is required")
	}

	// Validate the concept exists. Catches typos early; the alternative
	// (silently building an id under an unknown concept name) makes
	// downstream bugs much harder to trace.
	if e.concepts == nil {
		return "", fmt.Errorf("canonicalId: concept registry not available")
	}
	c, err := e.concepts.Get(conceptType)
	if err != nil || c == nil {
		return "", fmt.Errorf("canonicalId: unknown concept %q", conceptType)
	}

	// Every concept lives in one partition post-#56 -- the @scope-
	// derived branch is gone. Honor the request envelope's partition
	// the same way as for any other write/read.
	_ = c
	expectedPartition := e.resolvePartition(ctx)

	if !id.HasPartition(value) {
		// Bare slug: compose canonical id.
		return id.BuildNodeId(expectedPartition, conceptType, value), nil
	}

	// Already partition-qualified. Parse to verify the concept matches.
	gotPartition, gotConcept, gotShortId, perr := id.ParseNodeId(value)
	if perr != nil {
		return "", fmt.Errorf("canonicalId: malformed id %q: %w", value, perr)
	}
	if gotConcept != "" && gotConcept != conceptType {
		return "", fmt.Errorf("canonicalId: id %q is under concept %q, expected %q (caller passed the wrong type tag)",
			value, gotConcept, conceptType)
	}
	if gotPartition == expectedPartition {
		return value, nil
	}
	// Same concept, wrong partition -- re-partition to the canonical
	// home. Tolerates legacy callers that handed us a "default:" id
	// for a global concept (or vice versa) that should now live in
	// _system / the envelope's partition.
	return id.BuildNodeId(expectedPartition, conceptType, gotShortId), nil
}

// canonicalizeRelationshipFields walks a payload object and rewrites
// every value that names a foreign key (per the concept's @relationship
// metadata) to the canonical form for that target concept. Mutates
// `payload` in place.
//
// Why insert-time auto-canon: queries on payload fields use the raw
// stored value (`payload.userId == "_system:v1:identity:user:user-abc"`).
// If two callers store the same logical reference under different
// shapes ("user-abc" vs canonical), `==` doesn't match across them.
// Canonicalizing on insert collapses the two forms into one stored
// value, so id== queries and content-addressed downstream ids stay
// consistent.
//
// Outgoing relationships (Direction="outgoing") are foreign-key
// references the row holds. Incoming relationships are reverse
// pointers from other concepts and don't get rewritten here.
//
// Missing fields, non-string values, and empty strings are skipped
// (relationships are optional unless @required is also declared).
func (e *MemQLEngine) canonicalizeRelationshipFields(ctx context.Context, conceptName string, payload map[string]any) error {
	if payload == nil || conceptName == "" || e.concepts == nil {
		return nil
	}
	c, err := e.concepts.Get(conceptName)
	if err != nil || c == nil {
		return nil
	}
	for _, rel := range c.Relationships {
		if !strings.EqualFold(strings.TrimSpace(rel.Direction), "outgoing") {
			continue
		}
		field := strings.TrimSpace(rel.Field)
		target := strings.TrimSpace(rel.TargetConcept)
		if field == "" || target == "" {
			continue
		}
		raw, ok := payload[field]
		if !ok {
			continue
		}
		switch v := raw.(type) {
		case string:
			if strings.TrimSpace(v) == "" {
				continue
			}
			canon, err := e.canonicalizeIdValue(ctx, v, target)
			if err != nil {
				return fmt.Errorf("canonicalize %s.%s: %w", conceptName, field, err)
			}
			if canon != v {
				payload[field] = canon
			}
		case []any:
			// Some relationship fields (e.g. groupIds) are arrays of
			// foreign keys. Canonicalize each entry.
			for i, entry := range v {
				if str, ok := entry.(string); ok && strings.TrimSpace(str) != "" {
					canon, err := e.canonicalizeIdValue(ctx, str, target)
					if err != nil {
						return fmt.Errorf("canonicalize %s.%s[%d]: %w", conceptName, field, i, err)
					}
					v[i] = canon
				}
			}
		}
	}
	return nil
}
