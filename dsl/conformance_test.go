package dsl

import (
	"io"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/pagination"
	"github.com/znasllc-io/memql/component/memql/dslfs"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// itoa is a small wrapper to make exemption-map line refs more readable.
func itoa(i int) string { return strconv.Itoa(i) }

// TestFilterIntrinsicsUseRowNamespace asserts that every filter predicate
// names a row intrinsic through the `row.` namespace -- `row.id`, not a bare
// `id`.
//
// Why the namespace exists (memql#2779): a filter mixes two field surfaces
// under one syntax. Payload properties are BARE (epic #2292 -- the concept is
// bound by the query signature), so before this rule a reader could not tell
// `id == args.x` (row envelope) from `status == args.x` (payload) without
// memorising the reserved-word list, and the two compile to completely
// different SQL. `row.` makes the envelope explicit, lines the filter surface
// up with the namespaces an author already writes (`actor.userId`,
// `args.spaceId`, `config.X`), and matches shape bodies, which have always
// projected `row.id` / `row.createdAt`.
//
// Detection is shared with the edit-time Cockpit rule via
// sense.ScanBareRowIntrinsics, deliberately: the first cut of this gate had
// its own detector built on splitPredicates, which splits on `&&` only -- so
// a bare intrinsic joined by `||` or wrapped in parens
// (`filter (row.id==args.a || id==args.b)`) passed CI green while the editor
// flagged it. dsl/telephony/queries.memql already carries a
// parenthesized-OR filter, so that hole was reachable, not theoretical. One
// detector, one answer.
//
// Scope: filter predicates only. A spec/trait body still reads its
// signature-bound fields BARE and rejects `row.*` outright (epic #2281) --
// the binding lives in the signature there, so the namespace would be
// redundant. Mutation insert/update blocks write `id:` / `createdAt:` as
// target keys rather than references, and are likewise untouched.
func TestFilterIntrinsicsUseRowNamespace(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}

	violations := 0
	for _, p := range paths {
		f, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		for _, hit := range sense.ScanBareRowIntrinsics(string(raw)) {
			violations++
			t.Errorf("%s:%d:%d  filter names the row intrinsic %q bare -- write `row.%s`",
				p, hit.Line, hit.Column, hit.Text, hit.Name)
		}
	}
	if violations > 0 {
		t.Errorf("found %d filter predicate(s) naming a row intrinsic bare; the `row.` namespace is the canonical form (memql#2779)", violations)
	}
}

// TestSortKeysUseRowNamespace is the ordering half of the rule above
// (memql#2786). #2779 taught compileSortField to resolve `row.<intrinsic>`
// but migrated nothing and gated nothing, so the tree carried 70 bare keys
// and no test would have noticed a 71st.
//
// The ambiguity is the same one that justified the filter rule: in
// `sort "id"` the key can be the row id or a payload property named `id`,
// and the two compile to completely different ORDER BY expressions -- a
// table column vs `payload #>> '{id}'`.
//
// Detection is shared with the edit-time Cockpit rule via
// sense.ScanBareRowIntrinsicSortKeys, for the same reason the filter gate
// shares ScanBareRowIntrinsics: two detectors drift, one does not.
//
// Scope is authored `.memql` only. The runtime and SDK sort surfaces accept
// bare keys from callers and keep working -- compileSortField still resolves
// them (locked by TestSortKeysWithoutRowNamespaceUnchanged) -- exactly as the
// filter gate leaves the runtime filter surface alone.
func TestSortKeysUseRowNamespace(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}

	violations := 0
	for _, p := range paths {
		f, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		for _, hit := range sense.ScanBareRowIntrinsicSortKeys(string(raw)) {
			violations++
			t.Errorf("%s:%d:%d  sort key names the row intrinsic %q bare -- write \"row.%s\"",
				p, hit.Line, hit.Column, hit.Text, hit.Name)
		}
	}
	if violations > 0 {
		t.Errorf("found %d sort key(s) naming a row intrinsic bare; the `row.` namespace is the canonical form (memql#2786)", violations)
	}
}

// TestFilterSyntaxCanonical asserts that every filter clause in the
// tree references payload fields via `payload.X`, never via
// `<conceptName>.X` or `?.<conceptName>.X`.
//
// Background: prior to the 2026-05 cleanup, filter clauses mixed
// five syntactic forms for the same operation -- payload.X,
// <conceptName>.X, ?.<conceptName>.X, ?.X, and trait/spec calls.
// The decision recorded in feature/dsl-improvements: payload.X is
// the only legal prefix for payload fields; intrinsics (id, concept,
// createdAt, createdBy, partition, type, schema) stay bare. The `?.`
// optional-chain prefix is itself retired tree-wide (#977; rejected
// by TestNoRetiredOperatorForms) -- arg-conditional predicates now use
// the `when(args.x) { <expr> }` guard. This test still rejects any
// `?.<conceptName>.` LHS so residual `?.` artifacts can't sneak a
// concept-name alias back in.
//
// This test parses each .memql file line-structure-only (no full
// parser), extracts filter clauses, and rejects any predicate whose
// LHS starts with `<conceptName>.` or `?.<conceptName>.` where
// <conceptName> is not "payload" / "args" / "actor" or one of the
// row intrinsics. (`caller` is retired in #221 -- the parser
// rejects the accessor; TestNoCallerVocabulary catches author drift
// earlier with a clear file:line.)
func TestFilterSyntaxCanonical(t *testing.T) {
	// Bare payload access (epic #2292): a filter predicates on payload
	// properties by BARE name -- the concept is bound by the query
	// signature. The explicit `payload.` prefix is the violation now;
	// intrinsics and the reserved engine heads (args / actor / now /
	// config / trace) stay as they are.
	type violation struct {
		file string
		line int
		text string
	}
	var violations []violation

	visitFilterPredicates(t, func(file string, lineno int, pred string) {
		head, _ := splitFilterRef(pred)
		if head == "payload" {
			violations = append(violations, violation{file, lineno, pred})
		}
	})

	if len(violations) > 0 {
		t.Errorf("found %d filter predicates using the removed `payload.` prefix (write the payload property by bare name):", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d  %s", v.file, v.line, v.text)
		}
	}
}

// TestNoInlineTraitablePredicates asserts that no filter clause
// inlines a payload comparison that an existing trait spec covers.
// Today's traits in dsl/common/traits.memql:
//
//	isActiveRecord  ⇔ payload.active == true
//	isNotDeleted    ⇔ payload.deleted != true / payload.deleted == false
//	traitIsArchived      ⇔ payload.archived == true / payload.archivedAt != null
//	traitIsSaved         ⇔ payload.saved == true
//
// Authors must call the trait, not inline the comparison, so the
// definition of "active" / "deleted" / etc. lives in one place.
// Concept-specific predicates (payload.ownerUserId==args.userId)
// remain legal inline -- only the traitable predicates are
// rejected here.
func TestNoInlineTraitablePredicates(t *testing.T) {
	// Each pattern: matcher regex + suggested trait
	type rule struct {
		re   *regexp.Regexp
		hint string
	}
	// Under bare payload access (epic #2292) a filter predicates on a
	// payload property by bare name, so these traitable predicates match
	// the bare field (a leading word boundary anchors the field name and
	// avoids matching a longer identifier like `inactive`).
	rules := []rule{
		{regexp.MustCompile(`\bactive\s*==\s*true\b`), "isActiveRecord"},
		{regexp.MustCompile(`\bactive\s*!=\s*false\b`), "isActiveRecord"},
		{regexp.MustCompile(`\bdeleted\s*==\s*false\b`), "isNotDeleted"},
		{regexp.MustCompile(`\bdeleted\s*!=\s*true\b`), "isNotDeleted"},
		{regexp.MustCompile(`\bstatus\s*==\s*"active"`), "statusIsActive"},
		{regexp.MustCompile(`\bstatus\s*==\s*"archived"`), "statusIsArchived"},
		{regexp.MustCompile(`\bstatus\s*==\s*"saved"`), "statusIsSaved"},
		{regexp.MustCompile(`\bstatus\s*==\s*"pending"`), "statusIsPending"},
		{regexp.MustCompile(`\bstatus\s*==\s*"running"`), "statusIsRunning"},
		{regexp.MustCompile(`\bstatus\s*==\s*"cancelled"`), "isCancelled"},
		{regexp.MustCompile(`\bstatus\s*==\s*"completed"`), "isCompleted"},
		{regexp.MustCompile(`\bstatus\s*==\s*"inProgress"`), "isInProgress"},
		{regexp.MustCompile(`\bstatus\s*==\s*"open"`), "isOpen"},
		{regexp.MustCompile(`\bstatus\s*==\s*"scheduled"`), "isScheduled"},
		{regexp.MustCompile(`\bstatus\s*!=\s*"archived"`), "isNotArchived"},
		{regexp.MustCompile(`\bidentityType\s*==\s*"api_key"`), "identityIsApiKey"},
		{regexp.MustCompile(`\bidentityType\s*==\s*"worker_token"`), "identityIsWorkerToken"},
		{regexp.MustCompile(`\bdeletionScheduledAt\s*!=\s*""`), "isDeletionScheduled"},
	}

	type violation struct {
		file string
		line int
		text string
		hint string
	}
	var violations []violation

	visitFilterPredicates(t, func(file string, lineno int, pred string) {
		for _, r := range rules {
			if r.re.MatchString(pred) {
				violations = append(violations, violation{file, lineno, pred, r.hint})
			}
		}
	})

	if len(violations) > 0 {
		t.Errorf("found %d filter predicates that should use a trait spec:", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d  %s   → use %s", v.file, v.line, v.text, v.hint)
		}
	}
}

// TestNoShortIdConceptPrefix asserts that no .memql file constructs
// a node-id shortId with a concept-name (or sub-type discriminator)
// prefix. The canonical id format is `{partition}:{concept}:{shortId}`
// (`docs/public/concepts/identifiers.md`); the shortId should be the bare unique
// part (uuid / hash / slug), never `concat("<conceptName>-", ...)`
// or any equivalent string-concatenation pattern.
//
// Background (issue #53): pre-fix, the tree had `concat("ga-",
// hash(email))` for General Assistant agentIds and `concat("session-",
// id)` for sessionIds. Both duplicate information already in the
// canonical position of the id (concept name) or in a payload field
// (e.g. the agent's `role="assistant"` discriminator). The cleanup
// stripped the prefixes and moved the discrimination, where needed,
// into payload fields.
//
// The test fails on any new occurrence of `concat("<knownPrefix>-",
// ...)` in a .memql file. New names should be added to the list when
// they're identified as anti-patterns.
func TestNoShortIdConceptPrefix(t *testing.T) {
	bannedPrefixes := []string{
		// concept-name prefixes
		"agent-", "user-", "session-", "role-", "space-", "plan-",
		"task-", "delegation-", "partition-",
		// sub-type discriminators (move into payload fields instead)
		"ga-", "specialist-",
		// legacy identity-side prefixes (now redundant with `:user:` /
		// `:identity:` concept names + the polymorphic concept's
		// `identityType` discriminator field)
		"pat-", "wkr-", "wpc-", "sess-", "ml-", "ar-", "ac-",
	}
	type violation struct {
		file   string
		line   int
		text   string
		prefix string
	}
	var violations []violation
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	// Known exemptions tied to follow-up issues. Lines listed here use
	// the prefix legitimately for partition-naming (the partition row
	// stores per-user partition state); these go away wholesale with
	// the partitioning removal in issue #56. Until then they're an
	// acknowledged hole, not a regression.
	exemptions := map[string]bool{
		"identity/logic.memql:412": true, // partition name (issue #56 will remove the partition concept entirely)
		"identity/logic.memql:426": true, // partition lookup-by-name (same; issue #56)
	}
	for _, p := range paths {
		file, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(file)
		file.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		for lineno, line := range strings.Split(string(raw), "\n") {
			// Skip line comments + block-comment spans (line-level only).
			trimmed := strings.TrimLeft(line, " \t")
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			for _, prefix := range bannedPrefixes {
				needle := `concat("` + prefix
				if strings.Contains(line, needle) {
					locKey := p + ":" + itoa(lineno+1)
					if exemptions[locKey] {
						continue
					}
					violations = append(violations, violation{
						file:   p,
						line:   lineno + 1,
						text:   strings.TrimSpace(line),
						prefix: prefix,
					})
				}
			}
		}
	}
	if len(violations) > 0 {
		t.Errorf("found %d shortId-prefix anti-patterns (issue #53):", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d  %q in: %s", v.file, v.line, v.prefix, v.text)
		}
		t.Logf("\nThe canonical id format is `{partition}:{concept}:{shortId}`; the\nshortId should be the bare unique part (uuid/hash/slug), never\nprefixed with the concept name or a sub-type discriminator. If the\nprefix is a real discriminator (e.g. 'ga-' vs specialists), move it\ninto a payload field (e.g. agent.role='assistant').")
	}
}

// TestNoCanonicalPatternOnArgs asserts that no args-block field in the
// ENGINE's embedded core tree carries a `@pattern("^v1:...")` -- a regex
// anchored on the canonical `{concept}:{shortId}` id form. Such a pattern
// FORCES the client to compose a canonical id just to call the construct,
// which is exactly the coupling the bare-ids client contract (#2438) is
// removing: canonicalization is server-side only, clients pass BARE ids.
//
// Scope (staged rollout -- #2438 workstream A): this gate runs against the
// engine's embedded tree ONLY. In the `dsl` test package no product carrier
// is registered, so Tree() is the pure engine tree. The carrier still ships
// ~55 canonical arg patterns; those are removed in A3 (#2442) as part of the
// carrier contract flip, at which point the same conformance (packs run it
// via DSL registration) gates them too. Keeping A1 engine-scoped lets this
// land ahead of the carrier sweep without a cross-repo lockstep.
//
// Detection is line-structure-only (no full parser): walk each .memql file,
// track `args { ... }` spans by brace depth, and flag any line inside such a
// span whose text carries `@pattern("^v1:` (optionally without the `^`
// anchor). The one engine-tree occurrence today -- spaceMedia.partitionId in
// dsl/common/queries.memql -- is EXEMPTED: its arg carries a v1:cognition:space
// id, whose `space` concept is PACK-OWNED (absent from the engine tree), so the
// engine cannot declare an @relationship to make a bare arg resolve; and
// relaxing it to a bare-only pattern ahead of the SPA cutover (A4 #2443) would
// reject the canonical ids the current SPA still sends. Its coordinated
// relaxation rides A2's partition_id->space_id rename bundle (#2441).
func TestNoCanonicalPatternOnArgs(t *testing.T) {
	// canonicalArgPattern matches an args-field @pattern whose regex source is
	// anchored on the canonical id form (`^v1:` or a bare `v1:` literal head).
	canonicalArgPattern := regexp.MustCompile(`@pattern\("\^?v1:`)
	// argsOpen matches the start of an `args { ... }` block (contextual
	// keyword `args` immediately followed by `{`).
	argsOpen := regexp.MustCompile(`\bargs\s*\{`)
	// constructName captures the enclosing query/mutation/logic name so the
	// exemption key is stable across line-number churn.
	constructName := regexp.MustCompile(`^\s*(?:query|mutation|logic|seed)\s+(?:\w+\s+)?(\w+)\s*\{`)

	// Exemptions keyed by `<file>::<construct>.<field>` -- line-number-free so
	// edits above the field don't silently drop the exemption.
	exemptions := map[string]string{
		"common/queries.memql::spaceMedia.partitionId": "carries a v1:cognition:space id; `space` is pack-owned (not in the engine tree) so no @relationship can canonicalize a bare arg here, and a bare-only pattern would reject the canonical ids the current SPA sends. Rides A2's partition_id->space_id rename bundle (#2441) + A4 SPA cutover (#2443).",
	}

	type violation struct {
		file  string
		line  int
		field string
		text  string
	}
	var violations []violation

	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	for _, p := range paths {
		file, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(file)
		file.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}

		depth := 0
		argsBaseDepth := -1 // -1 when not inside an args block
		construct := ""
		for lineno, line := range strings.Split(string(raw), "\n") {
			code := stripLineComment(line)
			if m := constructName.FindStringSubmatch(code); m != nil {
				construct = m[1]
			}
			// Enter an args span (the depth recorded is the pre-brace depth so
			// the matching close returns us to it).
			if argsBaseDepth == -1 && argsOpen.MatchString(code) {
				argsBaseDepth = depth
			}
			if argsBaseDepth != -1 && canonicalArgPattern.MatchString(code) {
				field := "?"
				if fm := regexp.MustCompile(`^\s*(\w+)`).FindStringSubmatch(code); fm != nil {
					field = fm[1]
				}
				key := p + "::" + construct + "." + field
				if _, ok := exemptions[key]; !ok {
					violations = append(violations, violation{p, lineno + 1, field, strings.TrimSpace(line)})
				}
			}
			depth += strings.Count(code, "{") - strings.Count(code, "}")
			if argsBaseDepth != -1 && depth <= argsBaseDepth {
				argsBaseDepth = -1
			}
		}
	}

	if len(violations) > 0 {
		t.Errorf("found %d args field(s) with a canonical-id @pattern (#2438: clients pass BARE ids -- drop the pattern or relax it to a bare-slug form):", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d  arg %q: %s", v.file, v.line, v.field, v.text)
		}
	}
}

// idBearingFieldExemptions lists concept payload fields that LOOK like a
// node-id foreign key (a `*Id` name whose @description names a `v1:ns:concept`
// row) but are deliberately NOT annotated with `@relationship`. Keyed by
// `<namespace>/<concept>.<field>`; the value is the one-line justification.
//
// Why a field lands here (see TestIdBearingFieldsDeclareRelationship):
//   - pack-owned-target: the target concept (space / client / knowledgeDomain /
//     document) is NOT in the engine tree, so the engine cannot declare an
//     @relationship to it. These flip in A2 (#2441, incl. the partition_id ->
//     space_id rename) once the wire owns both id directions.
//   - plain-fk-by-design: the field's own @description already declares it a
//     "plain string FK, not a @relationship" -- typically part of a composite
//     id (concat(deploymentId, ':', nodeType)) or a cross-concept grouping key
//     the parent/contains/alias/equals relationship types don't model.
//   - bare-by-contract-A0: fixed as a deliberate bare id in A0 (#2439).
//   - bare-raw-sql-reader: a live raw-SQL reader compares payload->>'field'
//     against a bare value; annotating (canonicalize-on-insert) would break it.
//     Reconciled in A2 with the reader.
//   - non-node-correlation-id: a random UUID / correlation id, not a node FK.
//
// This is the coverage map A2 (#2441) keys its outbound FK bare-ification on:
// annotated fields = stored canonical (bare-ify on egress); exempted fields =
// already bare / not a node id (leave alone) OR pending A2's coordinated flip.
var idBearingFieldExemptions = map[string]string{
	// --- pack-owned target: v1:cognition:space (rides A2 #2441 partition_id->space_id) ---
	"cognition/audioOverride.partitionId":     "pack-owned-target v1:cognition:space; A2 #2441",
	"cognition/videoOverride.partitionId":     "pack-owned-target v1:cognition:space; A2 #2441",
	"identity/invitation.partitionId":         "pack-owned-target v1:cognition:space; A2 #2441",
	"identity/user.activePartitionId":         "pack-owned-target v1:cognition:space; A2 #2441",
	"library/artifact.partitionId":            "pack-owned-target v1:cognition:space; A2 #2441",
	"library/generatedOutput.partitionId":     "pack-owned-target v1:cognition:space; A2 #2441",
	"library/documentVersion.partitionId":     "pack-owned-target v1:cognition:space; A2 #2441",
	"library/memory.partitionId":              "pack-owned-target v1:cognition:space; A2 #2441",
	"planner/plan.partitionId":                "pack-owned-target v1:cognition:space; A2 #2441",
	"planner/responsibility.scopePartitionId": "pack-owned-target v1:cognition:space; A2 #2441",
	// --- pack-owned target: v1:knowledge:* (moved to pack in decoupling P2) ---
	"knowledge/documentChunk.documentId": "pack-owned-target v1:knowledge:document; A2 #2441",
	// --- bare-by-contract, fixed in A0 (#2439) ---
	"knowledge/documentChunk.domainId":          "bare-by-contract-A0 (#2439); also pack-owned v1:knowledge:knowledgeDomain + bare raw-SQL readers",
	"knowledge/documentChunk.sourceUtteranceId": "bare-by-contract-A0 (#2439); replier/augment store bare",
	"knowledge/documentChunk.sourceAgentId":     "bare-by-contract-A0 (#2439); replier/augment store bare",
	// --- nested sub-field: @relationship binds top-level payload fields only ---
	// Both live inside utterance's closed `source { }` block. Every
	// @relationship in the tree names a top-level field (zero use a dotted
	// path), so the annotation cannot address these -- exempting is the only
	// way to describe them accurately instead of under-documenting them to
	// duck this gate (#2794).
	"cognition/utterance.agentId":                "nested-subfield-of-source; @relationship binds top-level fields only (#2794)",
	"cognition/utterance.feedbackAnnouncePlanId": "nested-subfield-of-source; dedup key for feedbackAnnouncementForPlan, not a graph edge (#2794)",
	// --- plain string FK by design (own @description says so) ---
	"deployment/deployment.clusterId":            "plain-fk-by-design (@description declares plain string FK)",
	"deployment/deploymentNodeSpec.deploymentId": "plain-fk-by-design; part of composite id concat(deploymentId, ':', nodeType)",
	"cluster/node.deploymentId":                  "plain-fk-by-design (@description declares plain string FK)",
	"library/documentVersion.documentId":         "plain-fk-by-design; cross-concept content-history grouping key (@description declares NOT an @relationship)",
	// --- non-node correlation ids (description mentions a concept but value is a random UUID) ---
	"cognition/request.callId":        "non-node-correlation-id (UUID correlating a client tool call, not a node FK)",
	"cognition/response.callId":       "non-node-correlation-id (UUID correlating a client tool call, not a node FK)",
	"worker/invocation.correlationId": "non-node-correlation-id (random ID linking to an auditEvent row, not a canonicalizable FK)",
	// --- non-node string key (looks like a v1:rbac:capability id but is a stable slug) ---
	"healing/healedOverride.baseConstructId": "non-node string key (stable construct slug e.g. 'deployStaging', FK-by-slug like rbac.capability.roleSlug); read + matched BARE at component/memql/healing_base_immutable_validation.go:64",
	// --- nested variant field: @relationship (top-level-only canonicalization) can't reach it ---
	"identity/identity.nodeId": "nested under payload.credentials.node_token @variant (not a top-level payload key); canonicalizeRelationshipFields walks top-level fields only, so a concept-level @relationship(field=\"nodeId\") is a structural no-op",
	// --- deliberate short-form storage by write-side normalization ---
	"forge/requestEvent.requestId": "bare-by-contract (#1859): recordRequestEvent/recordMentoredEvent store shortId(args.requestId) so the audit trail unifies whether the caller passes a canonical (automation) or short (tool) id; an @relationship would re-canonicalize on insert and re-split the trail (conf_1859_test asserts zero events under the canonical id)",
}

// idBearingReferencesConcept detects the id-bearing-FK heuristic: a `v1:ns:concept`
// row reference inside a field @description. Matches the enumeration used to build
// the exemption table above.
var idBearingReferencesConcept = regexp.MustCompile(`v1:[a-z][a-zA-Z]*:[a-zA-Z]`)

// TestIdBearingFieldsDeclareRelationship asserts that every concept payload
// field that LOOKS like a node-id foreign key -- a `*Id` name (case-sensitive
// suffix; `*Ids` plurals are out of scope for this first pass) whose
// @description names a `v1:ns:concept` row -- EITHER carries an
// `@relationship(... field="<name>" ...)` annotation in its concept OR appears
// in idBearingFieldExemptions with a justification.
//
// Why (epic #2438 / A1 #2440): @relationship metadata is what lets the engine
// (1) canonicalize an inbound BARE id arg against the field's target concept
// (canonicalizeRelationshipComparisons) and (2) know, for A2 (#2441), which
// outbound payload fields to bare-ify at the wire seam. A `*Id` FK with neither
// an annotation nor an exemption is an unclassified gap -- it silently won't
// resolve bare inbound and A2 can't reason about its egress form.
//
// SEMANTICS WARNING for anyone MOVING a field from exempt -> annotated: adding
// @relationship changes runtime behavior -- inserts canonicalize the value and
// filters canonicalize the comparison RHS. Before annotating, confirm no
// raw-SQL/Go reader compares that payload field against a BARE value (grep
// `payload->>'<field>'` and `Payload["<field>"]`) and that the target concept
// EXISTS in the engine tree (pack-owned targets can't resolve). When in doubt,
// exempt with a note. See the A0 investigation + this PR's issue comment.
//
// Heuristic honesty: a `*Id` field whose @description omits the `v1:...` row
// reference evades this gate. That is an accepted false-negative for the first
// pass -- the description convention is near-universal in the tree, and the
// alternative (a hand-maintained allowlist of every id field) is more brittle.
func TestIdBearingFieldsDeclareRelationship(t *testing.T) {
	type field struct {
		file    string
		line    int
		ns      string
		concept string
		name    string
	}
	var flagged []field

	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	conceptStart := regexp.MustCompile(`^\s*concept\s+(\w+)\s*\{`)
	fieldStart := regexp.MustCompile(`^\s+(\w+)\s+\S`)
	relField := regexp.MustCompile(`@relationship\([^)]*field="(\w+)"`)
	descBody := regexp.MustCompile(`@description\("(.*)"\)`)

	exemptSeen := map[string]bool{}
	for _, p := range paths {
		if !strings.HasSuffix(p, "concepts.memql") {
			continue
		}
		ns := strings.SplitN(p, "/", 2)[0]
		file, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(file)
		file.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		lines := strings.Split(string(raw), "\n")
		for i := 0; i < len(lines); i++ {
			m := conceptStart.FindStringSubmatch(lines[i])
			if m == nil {
				continue
			}
			concept := m[1]
			// Accumulate the whole concept block by brace matching.
			depth := 0
			started := false
			var block []int // line indices in this block
			j := i
			for ; j < len(lines); j++ {
				depth += strings.Count(stripLineComment(lines[j]), "{") - strings.Count(stripLineComment(lines[j]), "}")
				if strings.Contains(lines[j], "{") {
					started = true
				}
				block = append(block, j)
				if started && depth == 0 {
					break
				}
			}
			// Set of fields declared with an @relationship in this concept.
			annotated := map[string]bool{}
			for _, li := range block {
				for _, rm := range relField.FindAllStringSubmatch(lines[li], -1) {
					annotated[rm[1]] = true
				}
			}
			// Flag id-bearing fields lacking an annotation + not exempted.
			for _, li := range block {
				code := stripLineComment(lines[li])
				fm := fieldStart.FindStringSubmatch(code)
				if fm == nil {
					continue
				}
				name := fm[1]
				if name == "Id" || !strings.HasSuffix(name, "Id") || strings.HasSuffix(name, "Ids") {
					continue
				}
				dm := descBody.FindStringSubmatch(lines[li])
				if dm == nil || !idBearingReferencesConcept.MatchString(dm[1]) {
					continue
				}
				key := ns + "/" + concept + "." + name
				if annotated[name] {
					continue
				}
				if _, ok := idBearingFieldExemptions[key]; ok {
					exemptSeen[key] = true
					continue
				}
				flagged = append(flagged, field{p, li + 1, ns, concept, name})
			}
			i = j
		}
	}

	if len(flagged) > 0 {
		t.Errorf("found %d id-bearing `*Id` field(s) that neither declare @relationship nor appear in idBearingFieldExemptions (#2440):", len(flagged))
		for _, f := range flagged {
			t.Errorf("  %s:%d  %s.%s -- annotate its concept with @relationship(field=%q, target=<concept>, ...) OR exempt with a reason", f.file, f.line, f.concept, f.name, f.name)
		}
	}
	// Keep the exemption table honest: a stale entry (field annotated, renamed,
	// or removed) should be pruned, not linger.
	for key := range idBearingFieldExemptions {
		if !exemptSeen[key] {
			t.Errorf("stale idBearingFieldExemptions entry %q -- the field no longer matches the id-bearing heuristic (annotated / renamed / removed?); prune it", key)
		}
	}
}

// TestRelationshipTargetsUseImports pins memql#1067: a concept's
// @relationship target must reference an imported concept by its bare name
// (resolved through a file-top `use <ns>.concepts.{ name }` import, or from
// the owning file for same-namespace targets) -- NOT a hardcoded canonical-ID
// string. The canonical-string form (`target="v1:..."`) is retired; this gate
// hard-fails any reintroduction so relationships stay on the same import model
// as mutations/queries/shapes. The engine resolves the bare name to the
// canonical id at load time (component/memql.LoadUnifiedConcepts).
func TestRelationshipTargetsUseImports(t *testing.T) {
	type violation struct {
		file string
		line int
		text string
	}
	var violations []violation

	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	for _, p := range paths {
		file, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(file)
		file.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		for lineno, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimLeft(line, " \t")
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if !strings.Contains(line, "@relationship") {
				continue
			}
			// Retired form: a quoted canonical-ID target (target="v1:...").
			if strings.Contains(line, `target="`) {
				violations = append(violations, violation{
					file: p,
					line: lineno + 1,
					text: strings.TrimSpace(line),
				})
			}
		}
	}
	if len(violations) > 0 {
		t.Errorf("found %d @relationship target(s) using the retired canonical-ID string form (memql#1067):", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d  %s", v.file, v.line, v.text)
		}
		t.Logf("\nUse the bare imported concept name instead of a canonical-ID string:\n" +
			"  use identity.concepts.{ user }\n" +
			"  @relationship(type=\"parent\", field=\"ownerUserId\", target=user, direction=\"outgoing\")\n" +
			"Same-namespace targets resolve from the owning file (no import needed).")
	}
}

// TestPerRowAuthzClassification scans every query / mutation in the
// tree and classifies it into one of four buckets:
//
//   - owned:   filter references `actor.userId` (caller-scoped read)
//     or mutation insert/update stamps `ownerUserId`/`userId`
//     from `actor.userId` (caller-scoped write)
//   - admin:   filter or body references `actor.isClusterOwner` or
//     the equivalent `requiresClusterOwner` spec call
//   - public:  carries the `@public` annotation
//   - flagged: none of the above AND the construct references a
//     user-scope field (`payload.ownerUserId`,
//     `payload.userId`, etc.) -- a candidate for caller-
//     scoping that the author forgot
//   - other:   none of the above, no user-scope field referenced
//     (e.g. concept catalogs, cluster topology, system
//     metadata reads)
//
// The test HARD-FAILS on any flagged construct -- the per-domain
// follow-up PRs for #54 closed every existing gap, and any new
// user-scope read/write needs to either include an actor-check
// (`actor.userId` reference, or an `isClusterOwner` admin gate)
// or carry an explicit `@public` annotation acknowledging the
// intent.
func TestPerRowAuthzClassification(t *testing.T) {
	type counts struct {
		owned   int
		admin   int
		public  int
		flagged int
		other   int
	}
	byDomain := map[string]*counts{}
	type flag struct {
		file string
		line int
		name string
		kind string
	}
	var flagged []flag

	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	headerRe := regexp.MustCompile(`(?m)^[ \t]*(query|mutation)[ \t]+(?:[A-Za-z_][A-Za-z0-9_]*[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

	for _, p := range paths {
		file, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(file)
		file.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		src := string(raw)
		matches := headerRe.FindAllStringSubmatchIndex(src, -1)
		domain := strings.SplitN(p, "/", 2)[0]
		if byDomain[domain] == nil {
			byDomain[domain] = &counts{}
		}
		for _, m := range matches {
			openIdx := m[1] - 1
			closeIdx := matchingClose(src, openIdx)
			if closeIdx < 0 {
				continue
			}
			preambleStart := m[0]
			for k := m[0] - 1; k >= 0; k-- {
				lineStart := strings.LastIndexByte(src[:k], '\n') + 1
				line := strings.TrimSpace(strings.TrimRight(src[lineStart:k+1], "\r\n"))
				if strings.HasPrefix(line, "@") || strings.HasPrefix(line, "//") {
					preambleStart = lineStart
					k = lineStart - 1
					continue
				}
				if line == "" {
					break
				}
				break
			}
			preamble := src[preambleStart:m[0]]
			body := src[m[1]:closeIdx]
			kind := src[m[2]:m[3]]
			name := src[m[4]:m[5]]

			hasPublic := strings.Contains(preamble, "@public")
			hasAdmin := strings.Contains(body, "actor.isClusterOwner") ||
				strings.Contains(body, "requiresClusterOwner")
			hasOwner := strings.Contains(body, "actor.userId")
			referencesUserScope := strings.Contains(body, "payload.ownerUserId") ||
				strings.Contains(body, "payload.userId") ||
				strings.Contains(body, "payload.actorUserId") ||
				strings.Contains(body, "payload.targetId") ||
				strings.Contains(body, "payload.createdBy")
			lineNo := strings.Count(src[:m[0]], "\n") + 1

			switch {
			case hasPublic:
				byDomain[domain].public++
			case hasOwner:
				byDomain[domain].owned++
			case hasAdmin:
				byDomain[domain].admin++
			case referencesUserScope:
				byDomain[domain].flagged++
				flagged = append(flagged, flag{p, lineNo, name, kind})
			default:
				byDomain[domain].other++
			}
		}
	}

	var domains []string
	for d := range byDomain {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	t.Logf("\n=== Per-row authz classification (informational; see docs/public/operate/auth/per-row-authz-audit.md) ===")
	t.Logf("%-15s %5s %5s %5s %5s %5s",
		"domain", "owned", "admin", "public", "FLAG", "other")
	for _, d := range domains {
		c := byDomain[d]
		t.Logf("%-15s %5d %5d %5d %5d %5d",
			d, c.owned, c.admin, c.public, c.flagged, c.other)
	}
	t.Logf("")
	if len(flagged) > 0 {
		t.Errorf("found %d flagged constructs that reference user-scope fields without a caller-check or @public annotation:", len(flagged))
		for _, f := range flagged {
			t.Errorf("  %s:%d  %s %s", f.file, f.line, f.kind, f.name)
		}
		t.Logf("\nResolution options:\n" +
			"  (1) add a caller-scope filter: `args.X == actor.userId` (or the canonical caller-id check for the domain)\n" +
			"  (2) add an admin gate: reference `actor.isClusterOwner` or a `requiresClusterOwner` spec\n" +
			"  (3) add `@public` to the construct's annotations with a comment explaining why no caller-check applies\n" +
			"See docs/public/operate/auth/per-row-authz-audit.md for the bucket definitions + the audit history.")
	}
}

// matchingClose walks src from openIdx (position of `{`) and returns
// the index of the matching `}`. String + line-comment aware.
func matchingClose(src string, openIdx int) int {
	if openIdx < 0 || openIdx >= len(src) || src[openIdx] != '{' {
		return -1
	}
	depth := 0
	inString := false
	for i := openIdx; i < len(src); i++ {
		c := src[i]
		if inString {
			if c == '\\' && i+1 < len(src) {
				i++
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '/':
			if i+1 < len(src) && src[i+1] == '/' {
				nl := strings.IndexByte(src[i:], '\n')
				if nl < 0 {
					return -1
				}
				i += nl
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// visitFilterPredicates walks every .memql file in the unified tree,
// extracts filter-clause lines, splits on `;`, and invokes f for
// each predicate. (_reference/ is structurally outside the walk --
// see TestWalkedTreeHasNoUnderscorePaths.)
func visitFilterPredicates(t *testing.T, f func(file string, lineno int, pred string)) {
	t.Helper()
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	for _, p := range paths {
		file, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(file)
		file.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		walkFilterPredicates(p, string(raw), f)
	}
}

// walkFilterPredicates scans src line-by-line.
//
// Two contexts emit predicates:
//
//  1. Struct-form: a line beginning with `filter ` opens a clause
//     whose body runs across `&&`-joined predicates (the canonical
//     AND operator, #977; the `;` AND separator is retired and
//     rejected tree-wide by TestNoRetiredOperatorForms) on the same
//     line and indented continuation lines, terminating on a known
//     end keyword / annotation / blank line.
//
//  2. Procedural-form: a legacy `shape(concept;` call inside a `func`
//     body (the author-side procedural form is retired -- this branch
//     only matches any residual artifact). Predicates there are
//     `;`-separated between `concept;` and the closing `,` before the
//     shape name argument.
//
// @filter(...) annotations on automations are intentionally NOT
// walked: that annotation uses a different (event-trigger)
// evaluator that doesn't recognize trait spec calls, so the same
// rules don't apply.
func walkFilterPredicates(path, src string, emit func(file string, lineno int, pred string)) {
	inFilter := false
	inShapeCall := false
	for lineno, raw := range strings.Split(src, "\n") {
		line := raw
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		trim := strings.TrimSpace(line)

		// Procedural-form `shape(` body — emit each ;-piece until
		// we see the closing `,` + shape name + `)`.
		if inShapeCall {
			if strings.Contains(trim, ")") {
				inShapeCall = false
			}
			for _, p := range splitPredicates(strim(trim, ',')) {
				p = strings.TrimSpace(p)
				if p == "" || p == "concept" {
					continue
				}
				// drop the trailing shape-name string arg if it's on this line
				if strings.HasPrefix(p, "\"") {
					continue
				}
				emit(path, lineno+1, p)
			}
			continue
		}
		if m := procShapeCallStart(line); m {
			inShapeCall = !strings.Contains(trim, ")")
			continue
		}

		if trim == "" {
			inFilter = false
			continue
		}
		if strings.HasPrefix(trim, "filter ") || strings.HasPrefix(trim, "filter\t") {
			inFilter = true
			rest := strings.TrimSpace(strings.TrimPrefix(trim, "filter"))
			for _, p := range splitPredicates(rest) {
				if p != "" {
					emit(path, lineno+1, p)
				}
			}
			continue
		}
		if !inFilter {
			continue
		}
		end := false
		for _, kw := range []string{"shape", "insert", "update", "return", "concept", "args", "use", "}", ")"} {
			if strings.HasPrefix(trim, kw+" ") || trim == kw || strings.HasPrefix(trim, kw+"\t") {
				end = true
				break
			}
		}
		if strings.HasPrefix(trim, "@") {
			end = true
		}
		if end {
			inFilter = false
			continue
		}
		for _, p := range splitPredicates(trim) {
			if p != "" {
				emit(path, lineno+1, p)
			}
		}
	}
}

// procShapeCallStart returns true if the line opens a procedural-
// form `shape(concept;` call. We look for the `shape(` token
// followed (possibly across whitespace) by `concept;`.
func procShapeCallStart(line string) bool {
	idx := strings.Index(line, "shape(")
	if idx < 0 {
		return false
	}
	rest := strings.TrimSpace(line[idx+len("shape("):])
	return strings.HasPrefix(rest, "concept;") || rest == "concept"
}

// strim trims the trailing rune `r` off a string if present.
// Mirrors strings.TrimRight for a single byte.
func strim(s string, r byte) string {
	for len(s) > 0 && s[len(s)-1] == r {
		s = s[:len(s)-1]
	}
	return s
}

// splitPredicates decomposes a filter clause into the individual predicates
// the gates inspect, seeing through every layer of the Go boolean grammar
// (#977): `&&`, `||`, `!`, parens, plus `when(args.x) { ... }` guards.
//
// It splits on BOTH connectives and recurses into what it finds (memql#2787).
// Splitting on `&&` alone handed the gates a parenthesized group as ONE
// predicate, which hit them differently:
//
//   - TestFilterSyntaxCanonical was BLIND. It classifies by head, and
//     splitFilterRef returns "" for a predicate that does not start with an
//     identifier character, so `(a || payload.b)` was inspected by nothing.
//   - TestNoInlineTraitablePredicates still fired -- it regex-matches predicate
//     TEXT, not the head -- but reported the whole group rather than the
//     offending comparison, and it lost anything a `when(x) { }` guard was
//     OR-ed with, because unwrapWhenPredicate cuts at the last `}`.
//
// The corpus already carries the shape (dsl/telephony/queries.memql, a
// parenthesized OR), so the blind spot was live, not hypothetical.
//
// Recursion is what makes it total. Each step strictly shortens the predicate
// -- unwrap a guard, drop a `!`, peel balanced outer parens -- or splits it
// into shorter pieces, so it terminates; maxPredicateNesting is a backstop
// against a malformed clause, not a real bound.
//
// String literals still suppress splitting at every level: a connective inside
// a quoted value is data, not structure.
func splitPredicates(s string) []string {
	return appendSplitPredicates(nil, s, 0)
}

// maxPredicateNesting bounds the recursion. Nothing in the corpus approaches
// it; it exists so a malformed clause cannot spin.
const maxPredicateNesting = 64

func appendSplitPredicates(out []string, s string, depth int) []string {
	if depth > maxPredicateNesting {
		if p := strings.TrimSpace(s); p != "" {
			out = append(out, p)
		}
		return out
	}
	for _, raw := range splitTopLevelConnectives(s) {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		// Peel one layer per recursion, in the order the grammar nests them.
		if inner := unwrapWhenPredicate(p); inner != p {
			out = appendSplitPredicates(out, inner, depth+1)
			continue
		}
		if inner, ok := stripLeadingNot(p); ok {
			out = appendSplitPredicates(out, inner, depth+1)
			continue
		}
		if inner, ok := stripOuterParens(p); ok {
			out = appendSplitPredicates(out, inner, depth+1)
			continue
		}
		out = append(out, p)
	}
	return out
}

// splitTopLevelConnectives splits on `&&` and `||` at paren/brace/bracket
// depth 0, outside string literals. Both split identically: a violation is a
// violation whichever side of a disjunct it sits on.
func splitTopLevelConnectives(s string) []string {
	var raw []string
	depth := 0
	inStr := false
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inStr = !inStr
		case inStr:
			// skip
		case c == '(' || c == '{' || c == '[':
			depth++
		case c == ')' || c == '}' || c == ']':
			depth--
		case depth == 0 && (c == '&' || c == '|') && i+1 < len(s) && s[i+1] == c:
			raw = append(raw, s[start:i])
			i++
			start = i + 1
		}
	}
	return append(raw, s[start:])
}

// stripLeadingNot removes a negation prefix. `!=` is a comparison operator,
// never a leading negation, so it is left alone.
func stripLeadingNot(p string) (string, bool) {
	if !strings.HasPrefix(p, "!") || strings.HasPrefix(p, "!=") {
		return p, false
	}
	return strings.TrimSpace(p[1:]), true
}

// stripOuterParens peels a paren pair wrapping the WHOLE predicate. It declines
// when the opener closes early (`(a) == (b)`), which is a comparison between
// groups rather than one parenthesized predicate.
func stripOuterParens(p string) (string, bool) {
	if len(p) < 2 || p[0] != '(' || p[len(p)-1] != ')' {
		return p, false
	}
	depth := 0
	inStr := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '"':
			inStr = !inStr
		case inStr:
			// skip
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 && i != len(p)-1 {
				return p, false
			}
		}
	}
	if depth != 0 {
		return p, false
	}
	return strings.TrimSpace(p[1 : len(p)-1]), true
}

// unwrapWhenPredicate returns the inner predicate of a `when(args.x) { <inner> }`
// guard, or the predicate unchanged when it is not a guard.
func unwrapWhenPredicate(p string) string {
	if !strings.HasPrefix(p, "when(") {
		return p
	}
	open := strings.Index(p, "{")
	closeIdx := strings.LastIndex(p, "}")
	if open < 0 || closeIdx <= open {
		return p
	}
	return strings.TrimSpace(p[open+1 : closeIdx])
}

// splitFilterRef peels the leading identifier (and optional `?.`
// prefix) off a predicate. Returns (head, rest). For
// `?.user.role==args.role` returns ("user", ".role==args.role").
// For `isActiveRecord` returns ("isActiveRecord", "").
// For `id==args.userId` returns ("id", "==args.userId").
func splitFilterRef(pred string) (string, string) {
	s := pred
	if strings.HasPrefix(s, "?.") {
		s = s[2:]
	}
	end := 0
	for end < len(s) {
		c := s[end]
		if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9' && end > 0)) {
			break
		}
		end++
	}
	if end == 0 {
		return "", ""
	}
	return s[:end], s[end:]
}

func hasFilterOperator(pred string) bool {
	for _, op := range []string{"==", "!=", "<=", ">=", " has ", " in ", " not "} {
		if strings.Contains(pred, op) {
			return true
		}
	}
	for _, c := range pred {
		if c == '<' || c == '>' {
			return true
		}
	}
	return false
}

// TestNoCallerVocabulary is the #221 conformance guardrail: every
// loaded .memql file must use the canonical `actor.X` auth-context
// accessor and the `@actor` shape kind annotation. The retired
// `caller.X` / `@caller` spellings are rejected by both parsers, so
// any drift would already break loading -- but a static-grep test
// surfaces the violation with a clean file:line and a migration
// hint rather than relying on log-scraping the load failure.
//
// (_reference/ is structurally outside the walk -- see
// TestWalkedTreeHasNoUnderscorePaths.)
func TestNoCallerVocabulary(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	// Match the accessor or annotation, not the English word "caller"
	// in comments / @description text. We surface only the structural
	// forms that survive into the parsed body: `caller.<ident>` (any
	// position) and `@caller` (start of line / after whitespace).
	accessorRe := regexp.MustCompile(`(?:^|[^A-Za-z0-9_])caller\.[A-Za-z_]`)
	annotationRe := regexp.MustCompile(`(?m)^[ \t]*@caller\b`)

	type hit struct {
		file string
		line int
		form string
	}
	var hits []hit
	for _, p := range paths {
		file, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(file)
		file.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		// Strip /* */ block comments and // line comments so an
		// English `caller.` inside a @description literal or a doc
		// header doesn't trip the check. We do this at line granularity
		// since the DSL doesn't use /* */ blocks; line stripping is
		// enough.
		for i, line := range strings.Split(string(raw), "\n") {
			stripped := stripLineComment(line)
			if accessorRe.MatchString(stripped) {
				hits = append(hits, hit{p, i + 1, "caller.X"})
			}
			if annotationRe.MatchString(stripped) {
				hits = append(hits, hit{p, i + 1, "@caller"})
			}
		}
	}
	if len(hits) > 0 {
		sort.Slice(hits, func(i, j int) bool {
			if hits[i].file != hits[j].file {
				return hits[i].file < hits[j].file
			}
			return hits[i].line < hits[j].line
		})
		var sb strings.Builder
		sb.WriteString("epic #218 / #221: caller. and @caller are retired -- use actor. and @actor.\n")
		for _, h := range hits {
			sb.WriteString("  " + h.file + ":" + itoa(h.line) + " " + h.form + "\n")
		}
		t.Errorf("%s", sb.String())
	}
}

func stripLineComment(line string) string {
	inStr := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '"' && (i == 0 || line[i-1] != '\\') {
			inStr = !inStr
			continue
		}
		if !inStr && c == '/' && i+1 < len(line) && line[i+1] == '/' {
			return line[:i]
		}
	}
	return line
}

// TestQueryConceptMatchesShapeConcept asserts that every struct-form query
// is bound (in its `query <Concept> <name>` signature) to the SAME concept as
// the `@row` shape it projects.
//
// Background (memql#1634): a query's signature concept names the table the
// engine SCANS; the shape only projects fields. When the two disagree the
// query scans the wrong concept and returns [] for rows that were written
// under the other concept -- silently, with no load-time or parse-time error.
// The 2026-05 signature-bound-concept migration (commit f7b6ab3) SWAPPED the
// bindings for two planner queries: taskStateById became `query plan`
// (reads taskState via taskStateFull) and tasksForPlan became
// `query taskState` (reads task via taskFull). Both returned [] right after
// successful creates. This invariant catches that class at test time.
//
// Only `@row` (concept-bound) shapes are checked. `@actor`-only shapes carry
// no concept in their signature (single identifier after `shape`) and are
// skipped -- a query never scans an actor envelope.
func TestQueryConceptMatchesShapeConcept(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}

	readFile := func(p string) string {
		f, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		return string(raw)
	}

	// `shape <Concept> <name> {` -> concept-bound (@row / mixed). The
	// single-identifier `shape <name> {` form is @actor-only (no concept).
	shapeDeclRe := regexp.MustCompile(`^\s*shape\s+(\w+)\s+(\w+)\s*\{`)
	// `query <Concept> <name> {`
	queryDeclRe := regexp.MustCompile(`^\s*query\s+(\w+)\s+(\w+)\s*\{`)
	// `shape <shapeName>` clause inside a query body.
	shapeClauseRe := regexp.MustCompile(`^\s*shape\s+(\w+)\s*$`)

	// Exempted (query -> shape) pairs: a shape deliberately REUSED across two
	// sibling concepts with an identical field layout. The query's scan
	// concept is correct (a mutation writes rows under it); only the projection
	// borrows a shape whose signature names the other sibling. This is NOT the
	// #1634 wrong-scan bug. variableFull projects {key,value,...} shared by
	// platform's globalVariable + partitionVariable.
	sharedShapeExempt := map[string]string{
		"globalVariable":  "variableFull",
		"globalVariables": "variableFull",
	}

	// Pass 1: global shapeName -> concept (concept-bound shapes only).
	shapeConcept := map[string]string{}
	for _, p := range paths {
		if !strings.HasSuffix(p, "shapes.memql") {
			continue
		}
		for _, line := range strings.Split(readFile(p), "\n") {
			if m := shapeDeclRe.FindStringSubmatch(stripLineComment(line)); m != nil {
				shapeConcept[m[2]] = m[1]
			}
		}
	}

	// Pass 2: walk queries; compare each query's concept to its shape's concept.
	type violation struct {
		file, query, queryConcept, shape, shapeConcept string
	}
	var violations []violation
	for _, p := range paths {
		if !strings.HasSuffix(p, "queries.memql") {
			continue
		}
		var curQuery, curConcept string
		for _, raw := range strings.Split(readFile(p), "\n") {
			line := stripLineComment(raw)
			if m := queryDeclRe.FindStringSubmatch(line); m != nil {
				curConcept, curQuery = m[1], m[2]
				continue
			}
			m := shapeClauseRe.FindStringSubmatch(line)
			if m == nil || curQuery == "" {
				continue
			}
			shapeName := m[1]
			sc, ok := shapeConcept[shapeName]
			if !ok {
				// @actor-only shape or unknown -> nothing to compare.
				curQuery = ""
				continue
			}
			if sc != curConcept && sharedShapeExempt[curQuery] != shapeName {
				violations = append(violations, violation{p, curQuery, curConcept, shapeName, sc})
			}
			curQuery = ""
		}
	}

	for _, v := range violations {
		t.Errorf("%s: query %q is bound to concept %q but projects @row shape %q bound to concept %q -- the query will scan %q and return [] for rows written under %q (memql#1634)",
			v.file, v.query, v.queryConcept, v.shape, v.shapeConcept, v.queryConcept, v.shapeConcept)
	}
}

// TestPaginationAuthoringRule is the pagination authoring gate (epic 5,
// issue 5.1 / memql#1965). The rule: every LIST-RETURNING query (a shape
// projecting a row set without a unique-key `id ==` filter) must declare
// how it is bounded -- `paginate` / `sort`, a `count` aggregate, or an
// explicit `@unbounded("reason")` opt-out.
//
// SEQUENCING (owner decision, 2026-06-22): the repo-wide HARD-FAIL was
// COUPLED to the issue 5.3 backfill. 5.3 (memql#1967) marked every list
// query in the tree (`paginate`/`sort` or `@unbounded("reason")`) and, in
// the same merge, flipped this gate from report-only to ENFORCING. So the
// test now HARD-FAILS on any unmarked-list finding -- a freshly-authored
// list query that declares no bound trips it. It asserts three things:
//
//  1. every list-returning query in the tree declares its bound (the
//     repo-wide hard-fail; zero unmarked-list findings allowed),
//  2. the classifier DETECTS a freshly-authored unmarked list query
//     (proves the enforcement mechanism is present and correct), and
//  3. the @unbounded-marked set is internally consistent (every mark
//     carries a non-empty reason).
//
// The audit report (scripts/audit-pagination) reads the SAME classifier,
// so its UNMARKED list and this gate can never drift.
func TestPaginationAuthoringRule(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}

	var findings []pagination.QueryFinding
	for _, p := range paths {
		if !strings.HasSuffix(p, "queries.memql") {
			continue
		}
		f, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		findings = append(findings, pagination.ScanSource(p, string(raw))...)
	}
	pagination.SortFindings(findings)

	byClass := map[pagination.Classification]int{}
	var unmarked []pagination.QueryFinding
	for _, f := range findings {
		byClass[f.Class]++
		if f.Class == pagination.UnmarkedList {
			unmarked = append(unmarked, f)
		}
		// (3) consistency: an @unbounded mark must carry a reason. The
		// rewriter rejects a reason-less @unbounded at load time, so this
		// is a belt-and-suspenders check on the classifier's capture path.
		if f.Class == pagination.UnboundedMarked && f.UnboundedReason == "" {
			t.Errorf("%s:%d query %q is @unbounded but carries no reason -- @unbounded(\"reason\") requires a non-empty justification", f.File, f.Line, f.Name)
		}
	}

	// Classification breakdown (for the test log).
	t.Logf("\n=== Pagination audit (memql#1965; ENFORCING since the 5.3 backfill) ===")
	t.Logf("%-16s %d", "single-row", byClass[pagination.SingleRow])
	t.Logf("%-16s %d", "aggregate", byClass[pagination.Aggregate])
	t.Logf("%-16s %d", "bounded-list", byClass[pagination.BoundedList])
	t.Logf("%-16s %d", "unbounded-marked", byClass[pagination.UnboundedMarked])
	t.Logf("%-16s %d", "unmarked-list", byClass[pagination.UnmarkedList])

	// (1) Repo-wide HARD-FAIL: every list-returning query must declare its
	// bound. A new unmarked list query (no `paginate`/`sort`, no
	// `@unbounded("reason")`) fails here. Backfilled to zero by issue 5.3.
	for _, f := range unmarked {
		t.Errorf("%s:%d query %q is an unmarked list query -- declare a bound: add `sort`/`paginate` "+
			"(a consumer-facing or growable list) or `@unbounded(\"reason\")` (a legitimate full-set read). "+
			"See authoring-rules.md #23; run `go run ./scripts/audit-pagination --unmarked` for the live list.",
			f.File, f.Line, f.Name)
	}

	// (2) Prove the gate's enforcement mechanism: a freshly-authored
	// unmarked list query MUST be detected. The repo-wide assertion above
	// relies on this classifier; the synthetic query locks its correctness
	// in even if the tree ever reached zero list queries.
	newQuery := `query widget queryEveryWidget {
  filter  payload.kind=="gizmo"
  shape   widgetFull
}`
	got := pagination.ScanSource("synthetic/queries.memql", newQuery)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 finding for the synthetic query, got %d", len(got))
	}
	if !got[0].IsUnmarkedList() {
		t.Fatalf("checker FAILED to detect a new unmarked list query: classified as %s, want unmarked-list -- the pagination authoring gate is broken", got[0].Class)
	}
}

// Compile-time guarantee that fs is referenced.
var _ fs.FS = (fs.FS)(nil)
