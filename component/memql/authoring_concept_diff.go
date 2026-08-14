package memql

// authoring_concept_diff.go -- classifying a concept SCHEMA CHANGE on re-promote
// (memql#3757, design section 7.3; sibling of the promote anchor memql#3746).
//
// Re-promoting a concept whose schema changed is a MIGRATION wearing a different
// hat. The promote path itself is cheap -- a concept is a string value in the
// `concept` column and there is no DDL to run -- but that says nothing about the
// rows already written. So this file answers the question the promote path could
// not: given the version this cluster is running and the version being promoted
// over it, what does the change do to data that already exists?
//
// # Why per-row `schema` helps and does not solve
//
// Every MemoryNodes row carries the SCHEMA IT WAS WRITTEN UNDER (the `schema`
// JSONB column, whose `$id` is the concept id). That is a real and unusual
// advantage: prior rows stay valid against their own schema, and a query written
// against the new shape sees an ABSENT field rather than a corrupt one. It is
// what makes an additive change safe to land with nothing else to do.
//
// It does not make every change safe, and reading it as if it did is the trap
// this file exists to close. A field REMOVED from the definition is still on
// every row that carries it, and the constructs that project or filter it now
// name a field the concept does not declare. A field whose TYPE changed reads
// back as the old type from every old row. A field that became REQUIRED is
// absent from every row already written and from every mutation that writes the
// concept -- and `@default` is never applied on insert (memql#2960), so the only
// mechanism that would fill it is `??` in a mutation body somebody has to go and
// write. A NARROWED enum leaves rows holding a value the concept no longer
// admits.
//
// # The rule
//
// Additive lands. Breaking is refused, naming the field, a REAL row count and
// the REAL constructs that reference it. An explicit override lands it anyway
// and is audited as an override. This follows the house rule memql#3625 set for
// tools: a silent degrade is not permissive, it is confidently wrong.
//
// # Where it runs, and where it deliberately does not
//
// It runs on the ORIGINATING node, inside promoteConceptIntoLiveRegistry, BEFORE
// the candidate is merged, before the reviewable rows are persisted and before
// the cross-node broadcast -- so a refused change never reaches a peer, and a
// refusal leaves the live registry exactly as it was.
//
// It does NOT run on the two REPLAY paths, and that is a decision rather than an
// omission:
//
//   - Boot re-hydration walks every durable-promote bundle. A re-promote writes
//     a NEW bundle row rather than editing the old one, so the walk replays the
//     versions in sequence -- and classifying the second against the first would
//     let a legitimately-overridden breaking change refuse to come back after a
//     restart. The decision was already made, once, by an operator.
//   - Cross-node propagation replays a promote a PEER already classified. Re-
//     classifying there could only produce a peer that disagrees with the node
//     that took the decision, which is a split registry -- the exact failure the
//     broadcast exists to prevent.
//
// Both say so by passing conceptPromoteReplay(); the strict classification is
// the DEFAULT, so a new call site that says nothing gets the gate rather than a
// bypass.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// --- the classified change vocabulary -------------------------------------

// The change kinds this classifier reports. The set is closed: a change it
// cannot name is one it cannot classify, and the flatten/compare below is
// written so that every difference it can see lands in exactly one of these.
//
// The BREAKING four are the ones the design fixed (section 7.3). The rest are
// reported because the editor (memql#3763) renders the whole diff, not only the
// refusal -- a promote that lands still tells the author what it changed.
const (
	ConceptChangeFieldAdded          = "field_added"
	ConceptChangeFieldRemoved        = "field_removed"          // BREAKING
	ConceptChangeFieldTypeChanged    = "field_type_changed"     // BREAKING
	ConceptChangeFieldRequiredAdded  = "field_required_added"   // BREAKING
	ConceptChangeFieldRequiredDroppd = "field_required_dropped" // relaxing
	ConceptChangeEnumWidened         = "enum_widened"
	ConceptChangeEnumNarrowed        = "enum_narrowed" // BREAKING
	ConceptChangeDescriptionChanged  = "description_changed"
	ConceptChangeRelationshipAdded   = "relationship_added"
	ConceptChangeRelationshipRemoved = "relationship_removed"
	ConceptChangeNodeTypeChanged     = "node_type_changed"
)

// ConceptSchemaChange is ONE classified difference between the concept version a
// cluster is running and the one being promoted over it.
//
// It is a STRUCTURED value, not a rendered line, because two consumers read it
// and neither may parse prose out of an error string: the SDK wraps it
// (memql#3760) and the editor renders it (memql#3763). Summary text is derived
// FROM this, in renderConceptSchemaDiff, and never the other way round.
type ConceptSchemaChange struct {
	// Concept is the canonical concept id the change belongs to.
	Concept string `json:"concept"`
	// Field is the payload field, dotted for a field inside a nested block
	// ("preferences.theme"). Empty for a concept-level change (description,
	// node type, a relationship).
	Field string `json:"field,omitempty"`
	// Kind is one of the ConceptChange* constants above.
	Kind string `json:"kind"`
	// Breaking is the classification. It is the ONLY thing the gate reads.
	Breaking bool `json:"breaking"`
	// Was / Now render the two sides of the change in the concept's own
	// vocabulary ("string" -> "number", "a|b|c" -> "a|b"), not JSON Schema's.
	Was string `json:"was,omitempty"`
	Now string `json:"now,omitempty"`
	// RowsAffected is a REAL count of rows this change lands on, measured
	// against the live table -- never an estimate and never a placeholder.
	// Meaningful only when RowCountKnown is true.
	RowsAffected int64 `json:"rowsAffected"`
	// RowCountKnown reports whether the count could be taken at all. A node
	// with no database (a unit-test engine, a binary that never connected)
	// cannot count, and a checker that hides what it could not examine turns
	// its own answer into a claim about the tool rather than about the data --
	// so the absence is carried explicitly and rendered explicitly.
	RowCountKnown bool `json:"rowCountKnown"`
	// ReferencedBy names the loaded constructs this change reaches, as
	// "kind:name" ("query:spaceParticipants"). Read off the live registries,
	// so it reports what THIS cluster actually has loaded.
	ReferencedBy []string `json:"referencedBy,omitempty"`
	// Detail is the one-line human reason, already carrying the counts.
	Detail string `json:"detail,omitempty"`
}

// ConceptSchemaDiff is the whole classification of one re-promote.
type ConceptSchemaDiff struct {
	Concept string `json:"concept"`
	// Breaking is true when ANY change is breaking.
	Breaking bool `json:"breaking"`
	// Overridden records that the promote landed a breaking change because the
	// caller asked for it explicitly. It is what the audit event keys on.
	Overridden bool                  `json:"overridden"`
	Changes    []ConceptSchemaChange `json:"changes,omitempty"`
	// Summary is the rendered block -- the same text the refusal error carries.
	Summary string `json:"summary,omitempty"`
}

// ConceptSchemaBreakingError is the refusal. It carries the whole diff so the
// gRPC layer can put the classification on the reply STRUCTURALLY instead of
// handing a client a prose blob to regex.
type ConceptSchemaBreakingError struct {
	Diff ConceptSchemaDiff
}

func (e *ConceptSchemaBreakingError) Error() string {
	return fmt.Sprintf(
		"promoting concept %q would BREAK rows already written under it (memql#3757). Re-promote with allow_breaking to land it anyway.\n%s",
		e.Diff.Concept, e.Diff.Summary)
}

// --- the per-promote posture ----------------------------------------------

// conceptPromoteGate is the per-promote posture toward a schema change, plus the
// sink the classified diffs land in.
//
// It is threaded rather than stored on the engine because it is a property of
// ONE call by ONE operator, not of the node: two promotes racing on the same
// engine must not see each other's override.
type conceptPromoteGate struct {
	// replay marks this promote a replay of a decision already taken (boot
	// re-hydration, cross-node propagation). No classification runs at all --
	// see the file header for why re-deciding is worse than not deciding.
	replay bool
	// allowBreaking is the explicit operator override. It lands a breaking
	// change and marks the diff Overridden, which is what gets audited.
	allowBreaking bool

	mu    sync.Mutex
	diffs []ConceptSchemaDiff
}

// conceptPromoteReplay returns the gate the two replay paths pass.
func conceptPromoteReplay() *conceptPromoteGate { return &conceptPromoteGate{replay: true} }

func (g *conceptPromoteGate) record(diff ConceptSchemaDiff) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.diffs = append(g.diffs, diff)
}

// collected returns the diffs recorded so far, in order.
func (g *conceptPromoteGate) collected() []ConceptSchemaDiff {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]ConceptSchemaDiff, len(g.diffs))
	copy(out, g.diffs)
	return out
}

// --- the gate the promote path calls --------------------------------------

// gateConceptSchemaChange classifies a re-promote and decides whether it lands.
//
// prior is the concept currently in the live registry (which, at this call site,
// is by construction a previously PROMOTED one -- a core id is refused before we
// get here). candidate is the compiled version being promoted over it.
//
// A FIRST promote never reaches this function: promoteConceptIntoLiveRegistry
// calls it only inside the branch where the registry already holds the id. That
// is the acceptance criterion "a first promote is not run through the classifier
// at all", satisfied structurally rather than by a nil check that a later edit
// could quietly drop.
//
// Returns a *ConceptSchemaBreakingError when the change is breaking and no
// override was given. Any other error is a failure to classify, which is also a
// refusal -- classifying is the only thing standing between an operator and a
// migration they did not know they were running, so failing OPEN would be worse
// than failing at all.
func (e *MemQLEngine) gateConceptSchemaChange(ctx context.Context, gate *conceptPromoteGate, owner string, prior, candidate *memorynodes.Concept) error {
	if gate != nil && gate.replay {
		return nil
	}
	diff, err := classifyConceptSchemaChange(prior, candidate)
	if err != nil {
		return fmt.Errorf("classify schema change: %w", err)
	}
	if len(diff.Changes) == 0 {
		// A re-promote of an identical schema. Nothing to say and nothing to
		// record: the diff exists to describe a CHANGE, and an empty one on the
		// reply would read to a client as "we looked and could not tell".
		return nil
	}

	// The counts and the referencing constructs are measured only now, over the
	// changes that actually exist. Doing it inside the classifier would make the
	// classifier need a database, and its rules are worth testing without one.
	e.enrichConceptSchemaDiff(ctx, &diff)

	if diff.Breaking && gate != nil && gate.allowBreaking {
		diff.Overridden = true
	}
	diff.Summary = renderConceptSchemaDiff(diff)
	gate.record(diff)

	if diff.Breaking && !diff.Overridden {
		return &ConceptSchemaBreakingError{Diff: diff}
	}
	if diff.Overridden {
		e.auditConceptSchemaOverride(ctx, owner, diff)
	}
	return nil
}

// --- classification (pure: no database, no registries) --------------------

// classifyConceptSchemaChange diffs two compiled concepts and classifies every
// difference. It reads ONLY the two values, so the rules are testable without a
// cluster.
func classifyConceptSchemaChange(prior, candidate *memorynodes.Concept) (ConceptSchemaDiff, error) {
	if prior == nil || candidate == nil {
		return ConceptSchemaDiff{}, fmt.Errorf("both the promoted version and the candidate are required")
	}
	conceptId := strings.TrimSpace(candidate.Name)
	diff := ConceptSchemaDiff{Concept: conceptId}

	priorFields, err := flattenConceptFields(prior)
	if err != nil {
		return ConceptSchemaDiff{}, fmt.Errorf("read the promoted version's schema: %w", err)
	}
	candidateFields, err := flattenConceptFields(candidate)
	if err != nil {
		return ConceptSchemaDiff{}, fmt.Errorf("read the candidate's schema: %w", err)
	}

	for _, path := range sortedFieldPaths(priorFields, candidateFields) {
		was, inPrior := priorFields[path]
		now, inCandidate := candidateFields[path]
		switch {
		case inPrior && !inCandidate:
			// BREAKING. The rows still carry it and the constructs still name
			// it; the definition is the only thing that forgot.
			diff.Changes = append(diff.Changes, ConceptSchemaChange{
				Concept: conceptId, Field: path, Kind: ConceptChangeFieldRemoved,
				Breaking: true, Was: was.declaredType(),
			})
		case !inPrior && inCandidate:
			if now.Required {
				// BREAKING. Every existing row lacks it, and no mutation
				// supplies it -- @default is never applied on insert
				// (memql#2960), so `??` in a mutation body is the only
				// mechanism that would fill it, and nobody has written one yet.
				diff.Changes = append(diff.Changes, ConceptSchemaChange{
					Concept: conceptId, Field: path, Kind: ConceptChangeFieldRequiredAdded,
					Breaking: true, Was: "absent", Now: now.declaredType(),
				})
				break
			}
			diff.Changes = append(diff.Changes, ConceptSchemaChange{
				Concept: conceptId, Field: path, Kind: ConceptChangeFieldAdded,
				Now: now.declaredType(),
			})
		default:
			diff.Changes = append(diff.Changes, compareConceptField(conceptId, path, was, now)...)
		}
	}

	diff.Changes = append(diff.Changes, compareConceptRelationships(conceptId, prior, candidate)...)

	if strings.TrimSpace(prior.Description) != strings.TrimSpace(candidate.Description) {
		diff.Changes = append(diff.Changes, ConceptSchemaChange{
			Concept: conceptId, Kind: ConceptChangeDescriptionChanged,
			Was: strings.TrimSpace(prior.Description), Now: strings.TrimSpace(candidate.Description),
		})
	}

	// The node type drives the `type` column invariants the promote path
	// re-derives (deriveConceptRegistryState), which REFUSES an invalid
	// combination on its own. So this is reported rather than classified
	// breaking: the derivation is the authority on whether the new type is
	// legal, and duplicating its judgement here would give the operator two
	// answers to the same question.
	if strings.TrimSpace(prior.NodeType) != strings.TrimSpace(candidate.NodeType) {
		diff.Changes = append(diff.Changes, ConceptSchemaChange{
			Concept: conceptId, Kind: ConceptChangeNodeTypeChanged,
			Was: strings.TrimSpace(prior.NodeType), Now: strings.TrimSpace(candidate.NodeType),
		})
	}

	for _, c := range diff.Changes {
		if c.Breaking {
			diff.Breaking = true
			break
		}
	}
	return diff, nil
}

// compareConceptField classifies the differences between the two versions of ONE
// field that exists in both.
func compareConceptField(conceptId, path string, was, now conceptFieldShape) []ConceptSchemaChange {
	var out []ConceptSchemaChange

	if was.baseType() != now.baseType() {
		// BREAKING. Every row written under the old definition reads back as
		// the old type; nothing rewrites them.
		out = append(out, ConceptSchemaChange{
			Concept: conceptId, Field: path, Kind: ConceptChangeFieldTypeChanged,
			Breaking: true, Was: was.baseType(), Now: now.baseType(),
		})
	}

	switch {
	case !was.Required && now.Required:
		// BREAKING for the same reason a NEW required field is: the rows that
		// omitted it are already written, and no mutation was ever asked to
		// supply it.
		out = append(out, ConceptSchemaChange{
			Concept: conceptId, Field: path, Kind: ConceptChangeFieldRequiredAdded,
			Breaking: true, Was: "optional", Now: "required",
		})
	case was.Required && !now.Required:
		// Relaxing. Every existing row satisfies a weaker constraint.
		out = append(out, ConceptSchemaChange{
			Concept: conceptId, Field: path, Kind: ConceptChangeFieldRequiredDroppd,
			Was: "required", Now: "optional",
		})
	}

	// Enum membership. Widened is additive (every stored value is still
	// admitted); narrowed is BREAKING (some stored value is not).
	//
	// A single field yields at most ONE enum entry. When an edit both adds and
	// removes members, the NARROWING is what is reported -- it is the half that
	// decides the outcome, and Was/Now carry both membership lists in full, so
	// nothing about the widening is lost. Emitting two entries for one field
	// would render the same field twice in the refusal block and read as two
	// problems.
	if len(was.Enum) > 0 || len(now.Enum) > 0 {
		removed := conceptDiffMissing(was.Enum, now.Enum)
		added := conceptDiffMissing(now.Enum, was.Enum)
		if len(removed) > 0 {
			// Was and Now carry the two membership lists in full, and the
			// enrichment pass re-derives the removed members from them rather
			// than storing a second copy that could drift out of step with the
			// pair a client renders.
			out = append(out, ConceptSchemaChange{
				Concept: conceptId, Field: path, Kind: ConceptChangeEnumNarrowed,
				Breaking: true, Was: strings.Join(was.Enum, "|"), Now: strings.Join(now.Enum, "|"),
			})
		}
		if len(added) > 0 && len(removed) == 0 {
			out = append(out, ConceptSchemaChange{
				Concept: conceptId, Field: path, Kind: ConceptChangeEnumWidened,
				Was: strings.Join(was.Enum, "|"), Now: strings.Join(now.Enum, "|"),
			})
		}
	}

	if strings.TrimSpace(was.Description) != strings.TrimSpace(now.Description) {
		out = append(out, ConceptSchemaChange{
			Concept: conceptId, Field: path, Kind: ConceptChangeDescriptionChanged,
			Was: strings.TrimSpace(was.Description), Now: strings.TrimSpace(now.Description),
		})
	}
	return out
}

// compareConceptRelationships reports added / removed relationship edges.
//
// Neither is classified BREAKING, and the removal deserves the note. A
// relationship drives id canonicalization and traversal, so dropping one does
// change how existing rows' references resolve -- but it makes no row unreadable
// and no field disappear, and the design's breaking set is a closed four that
// does not include it. Widening that set here would be deciding something the
// design decided; reporting the change is how the operator finds out anyway.
//
// A relationship whose FIELD was removed is caught as a removed field, which is
// the case that actually strands data.
func compareConceptRelationships(conceptId string, prior, candidate *memorynodes.Concept) []ConceptSchemaChange {
	priorEdges := relationshipSignatures(prior)
	candidateEdges := relationshipSignatures(candidate)
	var out []ConceptSchemaChange
	for _, sig := range conceptDiffMissing(priorEdges, candidateEdges) {
		out = append(out, ConceptSchemaChange{
			Concept: conceptId, Kind: ConceptChangeRelationshipRemoved, Was: sig,
		})
	}
	for _, sig := range conceptDiffMissing(candidateEdges, priorEdges) {
		out = append(out, ConceptSchemaChange{
			Concept: conceptId, Kind: ConceptChangeRelationshipAdded, Now: sig,
		})
	}
	return out
}

// relationshipSignatures renders a concept's edges as comparable strings. The
// `as` label rides along, so relabelling an edge reads as a change rather than
// as nothing (memql#3652 made `as` the domain-meaning axis, and a change in
// meaning is worth telling the author about even though the engine's behaviour
// is driven by `type`).
func relationshipSignatures(c *memorynodes.Concept) []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.Relationships))
	for _, r := range c.Relationships {
		sig := fmt.Sprintf("%s %s -> %s (%s)",
			strings.TrimSpace(r.Type), strings.TrimSpace(r.Field),
			strings.TrimSpace(r.TargetConcept), strings.TrimSpace(r.Direction))
		if as := strings.TrimSpace(r.As); as != "" {
			sig += " as " + as
		}
		out = append(out, sig)
	}
	sort.Strings(out)
	return out
}

// --- flattening a compiled concept into its field set ---------------------

// conceptFieldShape is one declared payload field, in the concept's own
// vocabulary rather than JSON Schema's.
type conceptFieldShape struct {
	Type        string
	Required    bool
	Enum        []string
	Description string
}

// baseType is the type used for the BREAKING type-change comparison.
func (f conceptFieldShape) baseType() string { return f.Type }

// declaredType renders the field as an author would write it, with `!` for
// required -- the form the refusal block prints.
func (f conceptFieldShape) declaredType() string {
	if f.Required {
		return f.Type + "!"
	}
	return f.Type
}

// flattenConceptFields walks a compiled concept's DEFINITION schema into a map
// of dotted field path -> shape.
//
// It walks `properties` and recurses into a nested block's own `properties`, so
// the same four rules apply one level down -- a nested field removal is a
// removal, a nested `!` added is a new required field. Both are enforced at
// insert (memql#3623), so both are real.
//
// It deliberately does NOT walk `oneOf` branches (a `@variant` property's
// discriminated union) or `allOf` (the discriminator constraints emitted
// alongside them). `allOf` is a constraint restatement, not a field set, so
// walking it would double-count. `oneOf` IS a field set, and not walking it is a
// stated limitation: a change confined to one variant BRANCH is reported as no
// change. What is caught is the property gaining or losing its variant-ness,
// because that lands in the type string below.
func flattenConceptFields(c *memorynodes.Concept) (map[string]conceptFieldShape, error) {
	out := map[string]conceptFieldShape{}
	if c == nil {
		return out, nil
	}
	raw, err := c.DefinitionSchema()
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	collectConceptSchemaFields("", doc, out)
	return out, nil
}

func collectConceptSchemaFields(prefix string, doc map[string]any, out map[string]conceptFieldShape) {
	props, ok := doc["properties"].(map[string]any)
	if !ok {
		return
	}
	required := map[string]bool{}
	if list, ok := doc["required"].([]any); ok {
		for _, item := range list {
			if name, ok := item.(string); ok {
				required[name] = true
			}
		}
	}
	for name, value := range props {
		sub, ok := value.(map[string]any)
		if !ok {
			// A non-object property schema (`true` / `false`) constrains
			// nothing. Record it as free-form rather than dropping it, so its
			// disappearance still reads as a removed field.
			out[joinFieldPath(prefix, name)] = conceptFieldShape{Type: "any", Required: required[name]}
			continue
		}
		path := joinFieldPath(prefix, name)
		out[path] = conceptFieldShape{
			Type:        conceptFieldType(sub),
			Required:    required[name],
			Enum:        conceptFieldEnum(sub),
			Description: stringField(sub, "description"),
		}
		collectConceptSchemaFields(path, sub, out)
	}
}

// conceptFieldType inverts propertyToJSONSchema back to the authored type name,
// so a diff reads in the vocabulary the author wrote rather than in JSON
// Schema's.
//
// The datetime case is the one that has to be right. An author's `datetime`
// emits TWO different schemas depending on whether it is required -- `type:
// string, format: date-time` when it is, and a three-member `oneOf` sentinel
// (date-time OR empty string OR null, memql#1629) when it is not. Reading the
// emitted shape naively would make flipping a datetime to required look like a
// TYPE change on top of the required change, and the type half would be a lie.
// Both forms map to "datetime" here, so only the required change is reported.
func conceptFieldType(sub map[string]any) string {
	if len(conceptFieldEnum(sub)) > 0 {
		return "enum"
	}
	switch stringField(sub, "type") {
	case "string":
		if stringField(sub, "format") == "date-time" {
			return "datetime"
		}
		return "string"
	case "boolean":
		return "bool"
	case "integer":
		return "int"
	case "number":
		return "float"
	case "array":
		if items, ok := sub["items"].(map[string]any); ok {
			return "[]" + conceptFieldType(items)
		}
		return "[]"
	case "object":
		// A map declares its VALUE type through additionalProperties; a closed
		// block sets the same key to the boolean `false`, which is not a value
		// type and must not be read as one.
		if values, ok := sub["additionalProperties"].(map[string]any); ok {
			return "map[string]" + conceptFieldType(values)
		}
		if _, variant := sub["oneOf"]; variant {
			return "variant"
		}
		return "object"
	}
	// No `type` at all. The optional-datetime sentinel is the case that matters
	// and is recognised by its date-time member; anything else with a oneOf is
	// a union whose branches this walk does not descend into.
	if members, ok := sub["oneOf"].([]any); ok {
		for _, member := range members {
			m, ok := member.(map[string]any)
			if ok && stringField(m, "format") == "date-time" {
				return "datetime"
			}
		}
		return "variant"
	}
	return "any"
}

func conceptFieldEnum(sub map[string]any) []string {
	list, ok := sub["enum"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		switch v := item.(type) {
		case string:
			out = append(out, v)
		default:
			out = append(out, fmt.Sprint(v))
		}
	}
	sort.Strings(out)
	return out
}

func stringField(doc map[string]any, key string) string {
	if v, ok := doc[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func joinFieldPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// sortedFieldPaths returns the union of both field sets in a stable order, so a
// refusal reads the same way twice.
func sortedFieldPaths(a, b map[string]conceptFieldShape) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(a)+len(b))
	for path := range a {
		if !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	for path := range b {
		if !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// conceptDiffMissing returns the members of `from` that `in` does not contain.
func conceptDiffMissing(from, in []string) []string {
	present := make(map[string]bool, len(in))
	for _, v := range in {
		present[v] = true
	}
	var out []string
	for _, v := range from {
		if !present[v] {
			out = append(out, v)
		}
	}
	return out
}

// --- enrichment: the REAL row count and the REAL referencing constructs ----

// enrichConceptSchemaDiff fills in RowsAffected / RowCountKnown / ReferencedBy /
// Detail for every change, measured against this cluster.
//
// This is the half the acceptance criterion is about: "the refusal reports a
// real row count and real referencing constructs, not a placeholder". A refusal
// whose numbers were invented would be worse than no numbers -- it would tell an
// operator that a change is expensive when it is free, or free when it is
// expensive, and they would believe it.
//
// Each change gets the count of the rows THAT change lands on, which is a
// different question per kind:
//
//   - a removed field, or one whose type changed: the rows that CARRY it, since
//     those are the rows holding a value the new definition has no place for;
//   - a field that became required: the rows that LACK it, since those are the
//     rows the new definition would refuse;
//   - a narrowed enum: the rows holding one of the values that stopped being
//     legal -- not every row of the concept, which would overstate it by orders
//     of magnitude on a status field.
func (e *MemQLEngine) enrichConceptSchemaDiff(ctx context.Context, diff *ConceptSchemaDiff) {
	if e == nil || diff == nil {
		return
	}
	for i := range diff.Changes {
		change := &diff.Changes[i]
		switch change.Kind {
		case ConceptChangeFieldRemoved, ConceptChangeFieldTypeChanged:
			change.RowsAffected, change.RowCountKnown = e.countConceptRowsWithField(ctx, diff.Concept, change.Field, true)
			change.ReferencedBy = e.constructsReferencingConceptField(diff.Concept, change.Field)
		case ConceptChangeFieldRequiredAdded:
			change.RowsAffected, change.RowCountKnown = e.countConceptRowsWithField(ctx, diff.Concept, change.Field, false)
			// The constructs that matter here are the MUTATIONS bound to the
			// concept, because the failure mode is "no existing mutation
			// supplies it". A mutation that already writes the field is not
			// part of the problem, so it is not listed.
			change.ReferencedBy = e.mutationsNotWritingConceptField(diff.Concept, change.Field)
		case ConceptChangeEnumNarrowed:
			removed := conceptDiffMissing(splitEnumList(change.Was), splitEnumList(change.Now))
			change.RowsAffected, change.RowCountKnown = e.countConceptRowsWithFieldValueIn(ctx, diff.Concept, change.Field, removed)
			change.ReferencedBy = e.constructsReferencingConceptField(diff.Concept, change.Field)
		}
		change.Detail = describeConceptSchemaChange(*change)
	}
}

func splitEnumList(joined string) []string {
	if strings.TrimSpace(joined) == "" {
		return nil
	}
	return strings.Split(joined, "|")
}

// --- row counting ---------------------------------------------------------

// countConceptRowsWithField counts DISTINCT rows of a concept whose LATEST
// version carries (present=true) or lacks (present=false) a payload field.
//
// Distinct rows, not raw versions: MemoryNodes is append-only with a
// (id, "createdAt") primary key, so one logical row is many table rows and
// counting those would report a number an operator cannot reconcile with
// anything they can see. The collapse is the same `DISTINCT ON (id) ... ORDER BY
// id, "createdAt" DESC` the query executor uses, over the same
// `(id, "createdAt" DESC)` index.
//
// Soft-deleted rows are NOT excluded, deliberately. `deleted: true` is a
// per-concept payload convention some domains adopt and others do not -- there
// is no engine-level tombstone on this path -- so excluding it would mean
// guessing which concepts mean it, and a guess in a number an operator is about
// to act on is worse than a number that is simply "every row". A soft-deleted
// row's payload is stored under the old schema exactly like a live one, so it is
// genuinely a row the change reaches.
//
// Returns known=false when this node has no database. It is not zero: zero is a
// claim that no row carries the field, and a node that cannot look must not make
// that claim.
func (e *MemQLEngine) countConceptRowsWithField(ctx context.Context, conceptId, field string, present bool) (int64, bool) {
	segments, ok := fieldPathSegments(field)
	if !ok {
		return 0, false
	}
	expr, args := jsonPayloadExpr(segments, false)
	if present {
		return e.countLatestConceptRows(ctx, conceptId, expr+" IS NOT NULL", args)
	}
	return e.countLatestConceptRows(ctx, conceptId, expr+" IS NULL", args)
}

// countConceptRowsWithFieldValueIn counts DISTINCT rows whose latest version
// holds one of the given values in a field -- the narrowed-enum question.
func (e *MemQLEngine) countConceptRowsWithFieldValueIn(ctx context.Context, conceptId, field string, values []string) (int64, bool) {
	if len(values) == 0 {
		return 0, false
	}
	segments, ok := fieldPathSegments(field)
	if !ok {
		return 0, false
	}
	expr, args := jsonPayloadExpr(segments, true)
	args = append(args, bun.In(values))
	return e.countLatestConceptRows(ctx, conceptId, expr+" IN (?)", args)
}

// countLatestConceptRows runs one predicate over the latest version of every row
// of a concept.
func (e *MemQLEngine) countLatestConceptRows(ctx context.Context, conceptId, predicate string, args []any) (int64, bool) {
	db := e.database()
	if db == nil {
		return 0, false
	}
	latest := db.NewSelect().
		Model((*memorynodes.MemoryNode)(nil)).
		DistinctOn("id").
		Where("concept = ?", conceptId).
		OrderExpr(`id ASC, "createdAt" DESC`)

	count, err := db.NewSelect().
		Model((*memorynodes.MemoryNode)(nil)).
		ModelTableExpr("(?) AS mn", latest).
		Where(predicate, args...).
		Count(ctx)
	if err != nil {
		if e.Component != nil && e.Logger != nil {
			e.Logger.Warn("concept schema-change row count failed; the refusal will say the count is unavailable rather than guess one",
				"component", "memql.engine", "concept", conceptId, "error", err)
		}
		return 0, false
	}
	return int64(count), true
}

// fieldPathSegments splits a dotted field path and refuses anything that is not
// a plain identifier segment.
//
// Every segment reaches SQL as a BIND PARAMETER (see jsonPayloadExpr), so this
// is not the injection guard -- it is a sanity guard on a path that should only
// ever come from a compiled concept's own property names. A path this rejects
// makes the count UNKNOWN rather than wrong.
func fieldPathSegments(field string) ([]string, bool) {
	field = strings.TrimSpace(field)
	if field == "" {
		return nil, false
	}
	segments := strings.Split(field, ".")
	for _, segment := range segments {
		if segment == "" || !conceptFieldSegmentRe.MatchString(segment) {
			return nil, false
		}
	}
	return segments, true
}

var conceptFieldSegmentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// jsonPayloadExpr builds `payload -> ? -> ? ...` (or a trailing `->>` for the
// text form), with every path segment bound rather than interpolated.
func jsonPayloadExpr(segments []string, asText bool) (string, []any) {
	var b strings.Builder
	b.WriteString("payload")
	args := make([]any, 0, len(segments))
	for i, segment := range segments {
		if asText && i == len(segments)-1 {
			b.WriteString(" ->> ?")
		} else {
			b.WriteString(" -> ?")
		}
		args = append(args, segment)
	}
	return b.String(), args
}

// --- referencing constructs ------------------------------------------------

// constructsReferencingConceptField returns the loaded constructs that bind this
// concept AND name this field, as "kind:name".
//
// It reads the LIVE registries rather than the DSL tree on disk, for the same
// reason the construct catalog does (memql#3749): a construct promoted at
// runtime lives in no file, and a construct the loader skipped lives in a file
// and not in the engine. The question an operator is asking here is "what breaks
// on this cluster", and only the registries can answer it.
//
// The field match is a whole-word match over the construct's authored expression
// source. A filter names a payload field bare (`status == "active"`), so the
// bare word IS the reference; a substring match would report `statusReason` for
// `status`, and no match at all would report nothing for the common case.
func (e *MemQLEngine) constructsReferencingConceptField(conceptId, field string) []string {
	if e == nil {
		return nil
	}
	word, ok := fieldWordMatcher(field)
	if !ok {
		return nil
	}
	var out []string
	e.eachFunctionBoundTo(conceptId, func(kind string, fn *Function) {
		if word.MatchString(functionSearchText(fn)) {
			out = append(out, kind+":"+fn.Name)
		}
	})
	e.eachSpecBoundTo(conceptId, func(kind string, spec *Spec) {
		if word.MatchString(spec.ExprSource) {
			out = append(out, kind+":"+spec.Name)
		}
	})
	e.eachShapeBoundTo(conceptId, func(shape *ShapeDefinition) {
		if word.MatchString(shapeSearchText(shape)) {
			out = append(out, "shape:"+shape.Name)
		}
	})
	sort.Strings(out)
	return out
}

// mutationsNotWritingConceptField returns the mutations bound to the concept
// that do NOT mention the field -- literally the set the refusal means by
// "existing mutations do not supply it".
//
// A mutation that already writes the field is excluded, because it is not part
// of the problem: adding the `!` to a field every mutation already sets breaks
// nothing on the write side (it still breaks the rows already written, which is
// what the row count reports).
func (e *MemQLEngine) mutationsNotWritingConceptField(conceptId, field string) []string {
	if e == nil {
		return nil
	}
	word, ok := fieldWordMatcher(field)
	if !ok {
		return nil
	}
	var out []string
	e.eachFunctionBoundTo(conceptId, func(kind string, fn *Function) {
		if kind != ConstructKindMutation {
			return
		}
		if !word.MatchString(functionSearchText(fn)) {
			out = append(out, kind+":"+fn.Name)
		}
	})
	sort.Strings(out)
	return out
}

func fieldWordMatcher(field string) (*regexp.Regexp, bool) {
	leaf := field
	if idx := strings.LastIndex(field, "."); idx >= 0 {
		leaf = field[idx+1:]
	}
	leaf = strings.TrimSpace(leaf)
	if leaf == "" {
		return nil, false
	}
	re, err := regexp.Compile(`\b` + regexp.QuoteMeta(leaf) + `\b`)
	if err != nil {
		return nil, false
	}
	return re, true
}

// functionSearchText is everything about a function a field name could appear
// in. A query carries its filter + shape in ExprSource; a MUTATION carries its
// written fields on the compiled template instead, so a source-only search would
// find no mutation at all -- which is exactly the set the required-field refusal
// is about.
func functionSearchText(fn *Function) string {
	if fn == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(fn.ExprSource)
	if fn.MutationTemplate != nil {
		b.WriteByte('\n')
		b.WriteString(fmt.Sprint(fn.MutationTemplate.PayloadTemplate))
		b.WriteByte('\n')
		b.WriteString(fmt.Sprint(fn.MutationTemplate.PayloadOverlayTemplate))
	}
	return b.String()
}

func shapeSearchText(shape *ShapeDefinition) string {
	if shape == nil {
		return ""
	}
	var b strings.Builder
	for key, value := range shape.Template {
		b.WriteString(key)
		b.WriteByte(' ')
		b.WriteString(fmt.Sprint(value))
		b.WriteByte('\n')
	}
	return b.String()
}

// eachFunctionBoundTo visits every loaded query / mutation / logic whose
// BoundConcept is this concept.
func (e *MemQLEngine) eachFunctionBoundTo(conceptId string, visit func(kind string, fn *Function)) {
	if e.functions == nil {
		return
	}
	for _, fn := range e.functions.List() {
		if fn == nil || strings.TrimSpace(fn.BoundConcept) != conceptId {
			continue
		}
		kind := functionCatalogKind(fn)
		if kind == "" {
			continue
		}
		visit(kind, fn)
	}
}

// eachSpecBoundTo visits every loaded spec bound to this concept.
//
// A spec's BoundName is its SIGNATURE binding, which is the concept's bare name
// (or a shape's), not the canonical id -- so both spellings are accepted. A
// trait is deliberately unbound and is therefore never attributed to a concept,
// which is correct: it is a cross-concept predicate over bare payload fields and
// naming it here would report a construct that is not about this concept at all.
func (e *MemQLEngine) eachSpecBoundTo(conceptId string, visit func(kind string, spec *Spec)) {
	if e.specs == nil {
		return
	}
	bare := conceptBareName(conceptId)
	for _, spec := range e.specs.List() {
		if spec == nil || spec.IsTrait {
			continue
		}
		bound := strings.TrimSpace(spec.BoundName)
		if bound != conceptId && (bare == "" || bound != bare) {
			continue
		}
		visit(ConstructKindSpec, spec)
	}
}

// eachShapeBoundTo visits every loaded shape whose signature concept is this
// concept. A shape projects fields by name, so a removed field leaves it
// projecting a key that is always null -- which is exactly the kind of silent
// wrongness the refusal exists to surface.
func (e *MemQLEngine) eachShapeBoundTo(conceptId string, visit func(shape *ShapeDefinition)) {
	if e.shapes == nil {
		return
	}
	bare := conceptBareName(conceptId)
	for _, shape := range e.shapes.List() {
		if shape == nil {
			continue
		}
		for _, used := range shape.UseConcepts {
			used = strings.TrimSpace(used)
			if used == conceptId || (bare != "" && used == bare) {
				visit(shape)
				break
			}
		}
	}
}

// conceptBareName is the declaration name out of a canonical `v1:ns:name` id.
func conceptBareName(conceptId string) string {
	conceptId = strings.TrimSpace(conceptId)
	if idx := strings.LastIndex(conceptId, ":"); idx >= 0 {
		return conceptId[idx+1:]
	}
	return conceptId
}

// --- rendering the refusal ------------------------------------------------

// describeConceptSchemaChange renders the one-line reason a change is what it
// is, carrying the real numbers. Kept next to the classification so the reason
// and the rule cannot drift.
func describeConceptSchemaChange(c ConceptSchemaChange) string {
	switch c.Kind {
	case ConceptChangeFieldRemoved:
		return leadThen("removed",
			rowClause(c, "carry it"),
			referenceClause(c.ReferencedBy, "reference it"),
		)
	case ConceptChangeFieldTypeChanged:
		return leadThen("",
			rowClause(c, "hold "+c.Was+" values"),
			referenceClause(c.ReferencedBy, "reference it"),
		)
	case ConceptChangeFieldRequiredAdded:
		return leadThen("required",
			rowClause(c, "lack it"),
			referenceClause(c.ReferencedBy, "do not supply it"),
		)
	case ConceptChangeEnumNarrowed:
		removed := conceptDiffMissing(splitEnumList(c.Was), splitEnumList(c.Now))
		return leadThen(quoteConceptValues(removed)+" removed",
			rowClause(c, "hold one of them"),
		)
	case ConceptChangeEnumWidened:
		added := conceptDiffMissing(splitEnumList(c.Now), splitEnumList(c.Was))
		return quoteConceptValues(added) + " added"
	case ConceptChangeFieldAdded:
		return "new optional field"
	case ConceptChangeFieldRequiredDroppd:
		return "no longer required"
	case ConceptChangeDescriptionChanged:
		return "description edited"
	case ConceptChangeRelationshipAdded:
		return "relationship added"
	case ConceptChangeRelationshipRemoved:
		return "relationship removed"
	case ConceptChangeNodeTypeChanged:
		return "node type " + c.Was + " -> " + c.Now
	}
	return ""
}

// rowClause renders the row count, or says the count could not be taken. It
// never renders a zero it did not measure -- see RowCountKnown.
func rowClause(c ConceptSchemaChange, verb string) string {
	if !c.RowCountKnown {
		return "row count unavailable on this node"
	}
	return formatRowCount(c.RowsAffected) + " " + pluralize(c.RowsAffected, "row", "rows") + " " + verb
}

func referenceClause(refs []string, verb string) string {
	if len(refs) == 0 {
		return ""
	}
	byKind := map[string]int64{}
	var order []string
	for _, ref := range refs {
		kind := ref
		if idx := strings.Index(ref, ":"); idx >= 0 {
			kind = ref[:idx]
		}
		if _, seen := byKind[kind]; !seen {
			order = append(order, kind)
		}
		byKind[kind]++
	}
	sort.Strings(order)
	parts := make([]string, 0, len(order))
	for _, kind := range order {
		n := byKind[kind]
		parts = append(parts, formatRowCount(n)+" "+pluralizeConstructKind(kind, n))
	}
	return strings.Join(parts, ", ") + " " + verb
}

// constructKindPlurals spells the plural of every construct kind a reference
// list can name. A naive `kind+"s"` produces "3 querys", which reads as a bug in
// the refusal and undermines the numbers standing next to it.
var constructKindPlurals = map[string]string{
	ConstructKindQuery:    "queries",
	ConstructKindMutation: "mutations",
	ConstructKindLogic:    "logic functions",
	ConstructKindSpec:     "specs",
	ConstructKindTrait:    "traits",
	ConstructKindShape:    "shapes",
}

func pluralizeConstructKind(kind string, n int64) string {
	if n == 1 {
		if kind == ConstructKindLogic {
			return "logic function"
		}
		return kind
	}
	if plural, ok := constructKindPlurals[kind]; ok {
		return plural
	}
	return kind + "s"
}

// leadThen renders "<lead>; <a>, <b>" -- the shape the design's refusal block
// uses. The lead is the classification ("removed", "required"); what follows are
// the measurements, which read as one list rather than as further clauses.
func leadThen(lead string, measurements ...string) string {
	kept := make([]string, 0, len(measurements))
	for _, part := range measurements {
		if strings.TrimSpace(part) != "" {
			kept = append(kept, part)
		}
	}
	tail := strings.Join(kept, ", ")
	switch {
	case lead == "":
		return tail
	case tail == "":
		return lead
	}
	return lead + "; " + tail
}

func quoteConceptValues(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, strconv.Quote(v))
	}
	return strings.Join(quoted, ", ")
}

func pluralize(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// formatRowCount groups thousands, because the number in a refusal is read by a
// person deciding whether to override it and "1,284" is read correctly at a
// glance where "1284" is not.
func formatRowCount(n int64) string {
	digits := strconv.FormatInt(n, 10)
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return sign + b.String()
}

// renderConceptSchemaDiff renders the block a refusal carries and the editor
// shows. Breaking changes come first and are the ones the heading is about; the
// additive ones follow so an author sees the whole change, not only the part
// that stopped them.
//
// The shape is the design's:
//
//	BREAKING - refused
//	  - sku                      removed; 1,284 rows carry it, 3 queries reference it
//	  ~ price  string -> number  1,284 rows hold string values
//	  + region string!           required; 1,284 rows lack it, 2 mutations do not supply it
func renderConceptSchemaDiff(diff ConceptSchemaDiff) string {
	breaking := make([]ConceptSchemaChange, 0, len(diff.Changes))
	additive := make([]ConceptSchemaChange, 0, len(diff.Changes))
	for _, c := range diff.Changes {
		if c.Breaking {
			breaking = append(breaking, c)
			continue
		}
		additive = append(additive, c)
	}

	var b strings.Builder
	if len(breaking) > 0 {
		if diff.Overridden {
			b.WriteString("BREAKING - landed by explicit override\n")
		} else {
			b.WriteString("BREAKING - refused\n")
		}
		writeChangeLines(&b, breaking)
	}
	if len(additive) > 0 {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("ADDITIVE - lands\n")
		writeChangeLines(&b, additive)
	}
	return strings.TrimRight(b.String(), "\n")
}

func writeChangeLines(b *strings.Builder, changes []ConceptSchemaChange) {
	labels := make([]string, len(changes))
	width := 0
	for i, c := range changes {
		labels[i] = conceptChangeLabel(c)
		if len(labels[i]) > width {
			width = len(labels[i])
		}
	}
	for i, c := range changes {
		b.WriteString("  ")
		b.WriteString(labels[i])
		if detail := strings.TrimSpace(c.Detail); detail != "" {
			b.WriteString(strings.Repeat(" ", width-len(labels[i])))
			b.WriteString("  ")
			b.WriteString(detail)
		}
		b.WriteByte('\n')
	}
}

// conceptChangeLabel is the left column: a marker plus the field in the shape
// the change is about. `-` removed, `+` added, `~` altered.
func conceptChangeLabel(c ConceptSchemaChange) string {
	name := c.Field
	if name == "" {
		name = conceptBareName(c.Concept)
	}
	switch c.Kind {
	case ConceptChangeFieldRemoved:
		return "- " + name
	case ConceptChangeFieldAdded:
		return "+ " + name + " " + c.Now
	case ConceptChangeFieldRequiredAdded:
		if c.Was == "absent" {
			return "+ " + name + " " + c.Now
		}
		return "~ " + name + " optional -> required"
	case ConceptChangeFieldTypeChanged:
		return "~ " + name + " " + c.Was + " -> " + c.Now
	case ConceptChangeEnumNarrowed:
		return "~ " + name + " enum narrowed"
	case ConceptChangeEnumWidened:
		return "~ " + name + " enum widened"
	case ConceptChangeFieldRequiredDroppd:
		return "~ " + name + " required -> optional"
	case ConceptChangeRelationshipAdded:
		return "+ @relationship " + c.Now
	case ConceptChangeRelationshipRemoved:
		return "- @relationship " + c.Was
	case ConceptChangeNodeTypeChanged:
		return "~ @type"
	case ConceptChangeDescriptionChanged:
		if c.Field == "" {
			return "~ @description"
		}
		return "~ " + name + " @description"
	}
	return "~ " + name
}

// --- auditing the override ------------------------------------------------

// AuditActionConceptSchemaBreakingOverride is the audit action stamped when an
// operator lands a BREAKING concept schema change explicitly. lower_snake_case
// to match the v1:identity:auditEvent.action convention, like every other
// authored-lifecycle action in this package.
const AuditActionConceptSchemaBreakingOverride = "authored_concept_schema_breaking_override"

// auditConceptSchemaOverride records the override on the same channel every
// other privileged authored act uses: an AuthoredAuditEvent through the engine's
// AuthoredAuditSink, which the app adapts onto v1:identity:auditEvent
// (app/engine_authored.go).
//
// It is the same sink and the same event type the bundle activation / retirement
// path emits, rather than a second audit mechanism -- component/memql cannot
// import component/identity (identity imports memql), which is precisely why
// that indirection exists, and inventing a parallel one here would put half the
// authoring trail on a different surface.
//
// The detail carries the WHOLE classified diff, not a count. An override is the
// one act in this file that leaves no other record of what was overridden: the
// refusal that would have named the fields never happened, and the concept
// registry afterwards shows only the new shape. If the audit row does not say
// which fields, nothing does.
//
// BundleId is deliberately left empty. The adapter maps it onto the audit row's
// TargetId, and this act's target is a CONCEPT, not a bundle -- stuffing the
// concept id into a field named for something else would make the trail read
// wrong to anyone who did not write this line. The concept is named in the
// detail, where it is unambiguous.
//
// A node with no sink wired still gets the WARN log below, so the act is never
// silent -- but the slog line is a second record, not the record.
func (e *MemQLEngine) auditConceptSchemaOverride(ctx context.Context, owner string, diff ConceptSchemaDiff) {
	if e == nil {
		return
	}
	if e.Component != nil && e.Logger != nil {
		e.Logger.Warn("BREAKING concept schema change landed by explicit override",
			"component", "memql.engine",
			"concept", diff.Concept,
			"owner", owner,
			"action", AuditActionConceptSchemaBreakingOverride,
			"changes", len(diff.Changes),
			"summary", diff.Summary)
	}
	sink := e.authoredAuditSink()
	if sink == nil {
		return
	}
	sink.EmitAuthoredAudit(ctx, AuthoredAuditEvent{
		Action:      AuditActionConceptSchemaBreakingOverride,
		OwnerUserId: strings.TrimSpace(owner),
		Detail:      map[string]any{"concept": diff.Concept, "summary": diff.Summary, "changes": diff.Changes},
		OccurredAt:  time.Now().UTC(),
	})
}

// SetAuthoredAuditSink wires the sink authored-lifecycle audit events reach
// v1:identity:auditEvent through (the app adapts it -- app/engine_authored.go).
//
// The activation path takes its sink per-call on AuthoredRuntimeDeps because it
// is driven from the app, which holds one. The concept schema-change override is
// reached from inside the engine's own promote path, which has no deps bundle
// and cannot grow one without threading it through every promote call site, so
// the engine holds the sink instead. Both arrive at the same sink object.
//
// Set once at bootstrap, before the node serves anything.
func (e *MemQLEngine) SetAuthoredAuditSink(sink AuthoredAuditSink) {
	if e == nil {
		return
	}
	e.authoredAudit = sink
}

func (e *MemQLEngine) authoredAuditSink() AuthoredAuditSink {
	if e == nil {
		return nil
	}
	return e.authoredAudit
}
