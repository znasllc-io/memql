package memql

// payloadLookup is one payload lookup index (component/database's
// payload_lookup_index.go): ((payload->>'<field>'), concept, id, "createdAt"
// DESC), partial on concepts. Its range for one value of field is every id of
// those concepts that carries the value on some version, in id order.
type payloadLookup struct {
	index    string
	field    string
	concepts []string
	// partial narrows the index further, to the versions on which each of
	// these payload fields equals its value, so it holds a small slice of a
	// large concept: one version per succeeded run rather than every version
	// of every run. Such a lookup serves a read only when the read REQUIRES
	// every one of these equalities. Otherwise the read could return a row
	// the index does not hold, and the planner could not use the index
	// anyway, because the query would not imply its predicate.
	partial []payloadEquality
	// coarse marks a field one value of which can cover most of the concept.
	// The run owner is the case: whoever owns the pipelines owns every
	// pipeline run, and every step boundary writes the run again, so that
	// owner's range is the run history (production, 2026-10-29). A coarse
	// lookup starts a read only when no finer one serves it, and is never
	// ANDed beside one: latestLookupKeys says why. A newest-first feed it
	// decides is walked to its page instead, where a walk index serves the
	// field (payloadWalks, latestScanWalk).
	coarse bool
}

// payloadEquality is one payload field equal to one set string.
type payloadEquality struct {
	field, value string
}

// payloadLookups is every payload lookup index the migrations build. The
// executor never reads the catalog to learn them -- it trusts this list, as it
// trusts the latest-row index -- so TestPayloadLookupsAreTheMigratedLookupIndexes
// holds the list to the migrated catalog in both directions.
var payloadLookups = []payloadLookup{
	{index: "memory_nodes_email_provider_lookup_idx", field: "providerMessageId", concepts: []string{"v1:email:sendOperation"}},
	{index: "memory_nodes_work_journal_lookup_idx", field: "runId", concepts: []string{"v1:work:step", "v1:work:approval", "v1:work:modelCall", "v1:work:observation"}},
	{index: "memory_nodes_work_run_owner_lookup_idx", field: "ownerUserId", concepts: []string{"v1:work:run"}, coarse: true},
	{index: "memory_nodes_work_run_goal_lookup_idx", field: "goalId", concepts: []string{"v1:work:run"}},
	// The procedure corpus (workRunsForOwnerGoalSignature) reads one goal
	// signature's SUCCEEDED runs; partial on that status, so a run finished
	// once is one entry, not one per step boundary.
	{index: "memory_nodes_work_run_signature_lookup_idx", field: "goalSignature", concepts: []string{"v1:work:run"},
		partial: []payloadEquality{{"status", "succeeded"}}},
	// The Library's revision reads (workDocumentRevisionRequest and its two
	// siblings) name this one automation. A lookup over every automation
	// would let a common one -- a pipeline's -- drive a read with the history
	// of every owner, so the index holds only this one's runs.
	{index: "memory_nodes_work_run_revision_lookup_idx", field: "automationName", concepts: []string{"v1:work:run"},
		partial: []payloadEquality{{"automationName", "reviseLibraryDocument"}}},
	// The revocation check (authSessionByTokenHash) runs for verified HTTP
	// requests and user stream opens. A digest names one session, so its range
	// is that session's versions, however many sessions the cluster has.
	{index: "memory_nodes_auth_session_hash_lookup_idx", field: "tokenHash", concepts: []string{"v1:identity:authSession"}},
	{index: "memory_nodes_recipient_digest_lookup_idx", field: "emailDigest", concepts: []string{"v1:campaigns:recipient"}},
	{index: "memory_nodes_consent_digest_lookup_idx", field: "emailDigest", concepts: []string{"v1:campaigns:consentEvent"}},
}

// payloadLookupFor returns the lookup index serving term on concept for a read
// whose filter requires terms, if one does.
func payloadLookupFor(concept string, term payloadLookupTerm, terms []payloadLookupTerm) (payloadLookup, bool) {
	for _, lookup := range payloadLookups {
		if lookup.field != term.field || !servesConcept(lookup.concepts, concept) {
			continue
		}
		if requiresEqualities(terms, lookup.partial) {
			return lookup, true
		}
	}
	return payloadLookup{}, false
}

// payloadWalk is one newest-first walk index (component/database's
// payload_walk_index.go): ((payload->>'<field>'), concept, "createdAt" DESC,
// id), partial on concepts. Its range for one value of field is every version
// carrying the value, newest first, in exactly the order the engine pages
// (createdAt DESC, id ASC) -- the read latestScanWalk makes.
type payloadWalk struct {
	index    string
	field    string
	concepts []string
}

// payloadWalks is every walk index the migrations build, held to the catalog
// in both directions by TestPayloadWalksAreTheMigratedWalkIndexes.
var payloadWalks = []payloadWalk{
	{index: "memory_nodes_work_run_owner_walk_idx", field: "ownerUserId", concepts: []string{"v1:work:run"}},
}

// payloadWalkFor reports whether a walk index serves field on concept.
func payloadWalkFor(concept, field string) bool {
	for _, walk := range payloadWalks {
		if walk.field == field && servesConcept(walk.concepts, concept) {
			return true
		}
	}
	return false
}

func servesConcept(concepts []string, concept string) bool {
	for _, served := range concepts {
		if served == concept {
			return true
		}
	}
	return false
}

// requiresEqualities reports whether terms require every one of equalities:
// each one is a term on its field with that single value.
func requiresEqualities(terms []payloadLookupTerm, equalities []payloadEquality) bool {
	for _, equality := range equalities {
		required := false
		for _, term := range terms {
			if term.field == equality.field && len(term.values) == 1 && term.values[0] == equality.value {
				required = true
				break
			}
		}
		if !required {
			return false
		}
	}
	return true
}
