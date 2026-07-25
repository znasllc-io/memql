package auth

import "time"

// The ONE canonical actor envelope (#2623). Before this table existed,
// four runtime resolvers exposed four different property sets and the
// docs described a fifth; every consumer now derives from here:
//
//   - query filters   (component/memql executor.resolveActorPath)
//   - mutation values (component/memql mutation_templates)
//   - logic bodies    (component/automations logic_runner seed)
//   - context-specs   (component/memql spec_evaluator buildSpecCtx)
//
// The dslspec reserved-identifier table mirrors this list for the
// editor surfaces, pinned by a two-directional drift test.
//
// Decisions (#2623, recorded in the PR):
//   - actor.now is IMPLEMENTED everywhere (RFC3339Nano UTC at eval
//     start): the canonical actorEnvelope @actor shape projects it and
//     used to resolve nil -- a live bug; in shape bodies the bare
//     reserved `now` is not addressable, so the field has a genuine
//     role there.
//   - actor.config.<key> and the doc-only `partitions` are DROPPED:
//     no resolver ever implemented them, the corpus never used them,
//     and bare `config.X` / `partition` are the reserved identifiers
//     for those reads -- one way to say it.
//   - isOwner remains a LEGACY ALIAS of isClusterOwner, accepted
//     uniformly on every surface (it was accepted on two of four,
//     which is the worst of both worlds); the canonical name is
//     isClusterOwner and the alias is marked as such in the table.
//   - IsClusterOwner() is the single owner-bit source; the logic seed
//     previously inlined `Role == RoleOwner`, a drift seed.

// ActorField describes one canonical actor-envelope property.
type ActorField struct {
	Name    string
	Doc     string
	AliasOf string // non-empty for legacy aliases
}

// ActorEnvelopeFields is the canonical property table, in
// documentation order.
var ActorEnvelopeFields = []ActorField{
	{Name: "userId", Doc: "The acting user's id."},
	{Name: "role", Doc: "Cluster role: owner / admin / writer / reader."},
	{Name: "identityId", Doc: "The credential row (token, magic-link, PAT)."},
	{Name: "isClusterOwner", Doc: "Bool short-circuit; bypasses the per-partition ACL."},
	{Name: "primaryEmail", Doc: "The acting user's primary email address."},
	{Name: "now", Doc: "RFC3339 timestamp at evaluation start (the shape-body spelling of the reserved `now`)."},
	{Name: "isOwner", Doc: "Legacy alias of isClusterOwner.", AliasOf: "isClusterOwner"},
}

// ActorEnvelopeCanonicalName resolves a field name through the alias
// table, returning ("", false) for names outside the envelope.
func ActorEnvelopeCanonicalName(name string) (string, bool) {
	for _, f := range ActorEnvelopeFields {
		if f.Name == name {
			if f.AliasOf != "" {
				return f.AliasOf, true
			}
			return f.Name, true
		}
	}
	return "", false
}

// ActorEnvelopeMap builds the seeded-map form of the envelope (logic
// bodies, context-specs): every canonical field plus the legacy alias
// key, so map-based and path-based surfaces expose the identical set.
// A nil AccessContext yields a DENYING envelope (owner bits false, empty
// identity), matching both the path resolver and AccessContext.
// IsClusterOwner() (memql#2801).
func ActorEnvelopeMap(ac *AccessContext) map[string]any {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if ac == nil {
		// Owner bits FALSE for an absent context (memql#2801) -- see the
		// isClusterOwner case in ActorEnvelopeValue. The two surfaces
		// must give the same answer or the gate depends on which one
		// evaluated it.
		return map[string]any{
			"userId": "", "role": "", "identityId": "", "primaryEmail": "",
			"isClusterOwner": false, "isOwner": false, "now": now,
		}
	}
	owner := ac.IsClusterOwner()
	return map[string]any{
		"userId":         ac.UserId,
		"role":           string(ac.Role),
		"identityId":     ac.IdentityId,
		"primaryEmail":   ac.PrimaryEmail,
		"isClusterOwner": owner,
		"isOwner":        owner,
		"now":            now,
	}
}

// ActorEnvelopeValue resolves one envelope path against an
// AccessContext -- the shared implementation behind the query-filter
// and mutation-value resolvers. ok is false for paths outside the
// envelope; the caller owns the error wording.
func ActorEnvelopeValue(ac *AccessContext, path string) (any, bool) {
	canonical, ok := ActorEnvelopeCanonicalName(path)
	if !ok {
		return nil, false
	}
	switch canonical {
	case "userId":
		if ac == nil {
			return "", true
		}
		return ac.UserId, true
	case "role":
		if ac == nil {
			return "", true
		}
		return string(ac.Role), true
	case "identityId":
		if ac == nil {
			return "", true
		}
		return ac.IdentityId, true
	case "primaryEmail":
		if ac == nil {
			return "", true
		}
		return ac.PrimaryEmail, true
	case "isClusterOwner":
		// Nil DENIES (memql#2801). `actor.isClusterOwner==true` is the
		// whole gate on the admin-tier constructs -- it compiles to a
		// bound `? = ?` fragment and nothing else bounds the query -- so
		// a permissive default returns every row instead of none.
		//
		// The old default called nil "dev mode", which it is not: with
		// MEMQL_IDENTITY_ENABLED=false the local-dev interceptor injects
		// a synthetic AccessContext (subject "local-dev", role "owner"),
		// so dev mode is non-nil and still resolves to owner here. Nil
		// means the context genuinely never arrived.
		//
		// IsClusterOwner() is already nil-safe (`ac != nil && ac.Role ==
		// RoleOwner`), so this defers to the canonical helper rather
		// than keeping a second, contradictory answer.
		return ac.IsClusterOwner(), true
	case "now":
		return time.Now().UTC().Format(time.RFC3339Nano), true
	}
	return nil, false
}

// ActorEnvelopeValidNames renders the accepted-path list for error
// messages, canonical names first, aliases last.
func ActorEnvelopeValidNames() string {
	out := ""
	for _, f := range ActorEnvelopeFields {
		if f.AliasOf != "" {
			continue
		}
		if out != "" {
			out += ", "
		}
		out += f.Name
	}
	for _, f := range ActorEnvelopeFields {
		if f.AliasOf != "" {
			out += ", " + f.Name + " (alias of " + f.AliasOf + ")"
		}
	}
	return out
}
