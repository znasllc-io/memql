package memql

// payloadLookup is one payload lookup index (component/database's
// payload_lookup_index.go): ((payload->>'<field>'), concept, id, "createdAt"
// DESC), partial on concepts. Its range for one value of field is every id of
// those concepts that carries the value on some version, in id order.
type payloadLookup struct {
	index    string
	field    string
	concepts []string
	// coarse marks a field one value of which can cover most of the concept.
	// The run owner is the case: whoever owns the pipelines owns every
	// pipeline run, and every step boundary writes the run again, so that
	// owner's range is the run history (production, 2026-10-29). A coarse
	// lookup starts a read only when no finer one serves it, and is never
	// ANDed beside one: latestLookupKeys says why.
	coarse bool
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
	{index: "memory_nodes_recipient_digest_lookup_idx", field: "emailDigest", concepts: []string{"v1:campaigns:recipient"}},
	{index: "memory_nodes_consent_digest_lookup_idx", field: "emailDigest", concepts: []string{"v1:campaigns:consentEvent"}},
}

// payloadLookupFor returns the lookup index serving field on concept, if one
// does.
func payloadLookupFor(concept, field string) (payloadLookup, bool) {
	for _, lookup := range payloadLookups {
		if lookup.field != field {
			continue
		}
		for _, served := range lookup.concepts {
			if served == concept {
				return lookup, true
			}
		}
	}
	return payloadLookup{}, false
}
