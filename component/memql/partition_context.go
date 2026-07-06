package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/id"
)

type partitionContextKey struct{}

// ContextWithPartition returns a new context carrying the given partition.
// Carried on the wire by the gRPC envelope; the engine no longer
// derives storage scoping from it. Kept on the request ctx so legacy
// log fields keep populating until envelope.partition is dropped in
// #56 phase 8.
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

// resolvePartition returns the envelope's partition for a request,
// falling back to the engine default. No longer used by the storage
// layer; remaining callers are event-topic + log-field producers.
func (e *MemQLEngine) resolvePartition(ctx context.Context) string {
	if ctx != nil {
		if p := PartitionFromContext(ctx); p != "" {
			return p
		}
	}
	return e.Partition()
}

// partitionForConcept returns the request's active partition. Storage
// scoping no longer uses this -- only event-topic + log fields do.
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
// `<conceptType>:<bareSlug>` form.
//
// Shared by:
//   - canonicalId() builtin in mutation + automation runtime
//     (explicit caller opt-in for derived ids)
//   - canonicalizeRelationshipFields (insert-time auto-canon for
//     payload fields tagged with @relationship)
//
// Behavior:
//
//	value=""                              -> "" (no-op for missing/optional ids)
//	bare slug                             -> prepend the concept type
//	canonical id with matching concept    -> as-is
//	canonical id with a DIFFERENT concept -> error (caller has the wrong type tag)
//
// The "wrong concept" branch errors loudly: silently rewriting it
// could mask real authoring bugs (passing a userId to canonicalId
// (..., "v1:cognition:space")). The other branches always succeed
// -- this is the contract callers depend on for stable id derivation.
func (e *MemQLEngine) canonicalizeIdValue(ctx context.Context, value, conceptType string) (string, error) {
	_ = ctx
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
	_ = c

	// Bare slug (no colons): compose `concept:slug`.
	if !strings.ContainsRune(value, ':') {
		return id.BuildNodeId(conceptType, value), nil
	}

	// Already concept-qualified.
	if strings.HasPrefix(value, conceptType+":") {
		return value, nil
	}

	// Parse to detect "wrong concept" tag.
	gotConcept, _, perr := id.ParseNodeId(value)
	if perr != nil {
		return "", fmt.Errorf("canonicalId: malformed id %q: %w", value, perr)
	}
	if gotConcept != "" && gotConcept != conceptType {
		return "", fmt.Errorf("canonicalId: id %q is under concept %q, expected %q (caller passed the wrong type tag)",
			value, gotConcept, conceptType)
	}
	return value, nil
}

// shortIdValue is the inverse of canonicalizeIdValue: it strips the
// `<concept>:` prefix from a canonical node id and returns the trailing
// bare short id. Backs the shortId() DSL builtin (#1859). Delegates to
// the exported BareShortId primitive (wire_bareids.go, #2441) so the
// builtin and the wire-egress bare-ification share one implementation.
func (e *MemQLEngine) shortIdValue(value string) string {
	return BareShortId(value)
}

// canonicalizeRelationshipFields walks a payload object and rewrites
// every value that names a foreign key (per the concept's @relationship
// metadata) to the canonical form for that target concept. Mutates
// `payload` in place.
//
// Why insert-time auto-canon: queries on payload fields use the raw
// stored value (`payload.userId == "v1:identity:user:user-abc"`).
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
