package memql

// shape_validation_3621_test.go -- memql#3621.
//
// Nothing anywhere compared a shape body against the concept it binds, and
// two shipping shapes were wrong because of it. The corpus gate below is the
// one that failed against the pre-fix tree; the rest pin the four latent
// holes the same absence left open, each of which produced a wire key whose
// value was silently nil rather than an error.

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// quietLogger keeps the loader's boot chatter out of test output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// loadCorpusShapes loads the real concept + shape trees the engine boots
// from, so the gates below run against shipping DSL rather than a fixture.
func loadCorpusShapes(t *testing.T) (*ShapeRegistry, memoryNodes.Registry) {
	t.Helper()
	logger := quietLogger()
	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	shapes := newShapeRegistry()
	if _, err := LoadUnifiedShapes(logger, shapes); err != nil {
		t.Fatalf("LoadUnifiedShapes: %v", err)
	}
	return shapes, memoryNodes.DefaultRegistry()
}

// convertShape parses one shape source and runs it through the converter --
// the exact path LoadUnifiedShapes and the authoring sandbox both take.
func convertShape(t *testing.T, source string) (*ShapeDefinition, error) {
	t.Helper()
	decl, err := languageParser.ParseShapeDecl(source)
	if err != nil {
		t.Fatalf("ParseShapeDecl(%q): %v", source, err)
	}
	return shapeDeclToShapeDefinition(decl, "test:shapes.memql")
}

// TestShapeBodiesAgreeWithBoundConcept is THE gate for the two shipped
// defects. It fails against the pre-fix tree with exactly two violations:
//
//	workspaceFull    projects payload property "createdAt" (meant row.createdAt)
//	delegationFull   projects payload property "agentSubject" (removed by #1630)
//
// Both produced the RIGHT wire key with the WRONG value -- null -- which is
// why four delegation queries and two workbench queries shipped like this.
func TestShapeBodiesAgreeWithBoundConcept(t *testing.T) {
	shapes, concepts := loadCorpusShapes(t)

	violations := validateShapeConceptBindings(shapes, concepts)
	for _, v := range violations {
		t.Errorf("shape %s (%s): %s", v.Shape, v.Origin, v.Detail)
	}
	if len(violations) > 0 {
		t.Fatalf("%d shape(s) disagree with the concept they bind", len(violations))
	}

	// Fail loudly if the gate ever scans nothing: an empty registry would
	// pass vacuously and report a meaningless zero.
	bound := 0
	for _, s := range shapes.List() {
		if s != nil && len(s.UseConcepts) > 0 {
			bound++
		}
	}
	if bound == 0 {
		t.Fatal("scanned zero concept-bound shapes -- the gate is not looking at the tree")
	}
}

// TestShapeConceptValidatorNamesTheTwoDefects reconstructs the two bodies as
// they shipped and asserts each is reported, with the correction the author
// needs. Keeps the diagnosis pinned after the .memql fix removes the live
// instances.
func TestShapeConceptValidatorNamesTheTwoDefects(t *testing.T) {
	_, concepts := loadCorpusShapes(t)

	cases := []struct {
		name      string
		shape     *ShapeDefinition
		wantParts []string
	}{
		{
			name: "workbench workspaceFull bare createdAt",
			shape: &ShapeDefinition{
				Name:        "workspaceFullPreFix",
				Origin:      "unified:workbench/shapes.memql",
				KindRow:     true,
				UseConcepts: []string{"workspace"},
				Template: map[string]any{
					"planId":    `node(\"payload.planId\")`,
					"createdAt": `node(\"payload.createdAt\")`,
				},
			},
			// The hint is the whole value here: both spellings render the
			// key `createdAt`, so "not declared" alone reads as nonsense
			// against a body that plainly says createdAt.
			wantParts: []string{"createdAt", "ROW INTRINSIC", "row.createdAt"},
		},
		{
			name: "identity delegationFull agentSubject",
			shape: &ShapeDefinition{
				Name:        "delegationFullPreFix",
				Origin:      "unified:identity/shapes.memql",
				KindRow:     true,
				UseConcepts: []string{"delegation"},
				Template: map[string]any{
					"agentId":      `node(\"payload.agentId\")`,
					"agentSubject": `node(\"payload.agentSubject\")`,
				},
			},
			wantParts: []string{"agentSubject", "v1:identity:delegation", "createdBySubject"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shapes := newShapeRegistry()
			if err := shapes.Upsert(tc.shape); err != nil {
				t.Fatalf("Upsert: %v", err)
			}
			violations := validateShapeConceptBindings(shapes, concepts)
			if len(violations) != 1 {
				t.Fatalf("violations = %d, want 1: %+v", len(violations), violations)
			}
			for _, want := range tc.wantParts {
				if !strings.Contains(violations[0].Detail, want) {
					t.Errorf("detail %q missing %q", violations[0].Detail, want)
				}
			}
		})
	}
}

// TestShapeTerminalKeyCollisionRefused pins latent hole 1. Every path is
// keyed by its TERMINAL segment into a plain map, so two paths sharing one
// terminal silently collapsed to a single entry, last write wins. `row.id` +
// `id` is the sharp case -- the row id is dropped in favour of a payload
// property -- and `credentials.aaguid` vs `aaguid` is exactly the passkey
// union shape memql#3605 lived in.
func TestShapeTerminalKeyCollisionRefused(t *testing.T) {
	cases := []struct {
		name   string
		source string
	}{
		{"row.id vs bare id", "@row\nshape workspace collide {\n  row.id\n  id\n}"},
		{"nested vs bare", "@row\nshape workspace collide {\n  credentials.aaguid\n  aaguid\n}"},
		{"two nested paths", "@row\nshape workspace collide {\n  a.name\n  b.name\n}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := convertShape(t, tc.source)
			if err == nil {
				t.Fatal("want a load error for a terminal-key collision, got nil")
			}
			if !strings.Contains(err.Error(), "same template key") {
				t.Fatalf("error = %v, want it to name the colliding template key", err)
			}
		})
	}

	// The non-colliding sibling still loads, and keeps BOTH entries.
	shape, err := convertShape(t, "@row\nshape workspace fine {\n  row.id\n  planId\n}")
	if err != nil {
		t.Fatalf("distinct terminals must load: %v", err)
	}
	if len(shape.Template) != 2 {
		t.Fatalf("Template = %v, want 2 entries", shape.Template)
	}
}

// TestShapeKindIsDeclaredAndHonoured pins latent hole 2. The kind was parsed,
// stored, and never checked -- declared_usage_validator.go even claims "the
// shape kind validator already verifies the body contains at least one
// matching path", and there was no such validator.
func TestShapeKindIsDeclaredAndHonoured(t *testing.T) {
	cases := []struct {
		name     string
		source   string
		wantPart string
	}{
		{
			name:     "no kind at all",
			source:   "shape workspace kindless {\n  planId\n}",
			wantPart: "declares neither @row nor @actor",
		},
		{
			name:     "row-only shape projecting an actor path",
			source:   "@row\nshape workspace rowOnly {\n  planId\n  actor.userId\n}",
			wantPart: "does not declare @actor",
		},
		{
			name:     "actor-only shape projecting a payload property",
			source:   "@actor\nshape actorOnly {\n  actor.userId\n  planId\n}",
			wantPart: "does not declare @row",
		},
		{
			name:     "actor-only shape projecting a row intrinsic",
			source:   "@actor\nshape actorOnlyIntrinsic {\n  actor.userId\n  row.id\n}",
			wantPart: "row intrinsic",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := convertShape(t, tc.source)
			if err == nil {
				t.Fatal("want a load error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantPart) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantPart)
			}
		})
	}

	// A mixed shape declaring both kinds is legal and keeps both flags.
	mixed, err := convertShape(t, "@row\n@actor\nshape workspace mixed {\n  planId\n  actor.userId\n}")
	if err != nil {
		t.Fatalf("mixed shape must load: %v", err)
	}
	if !mixed.KindRow || !mixed.KindActor {
		t.Fatalf("KindRow=%v KindActor=%v, want both true", mixed.KindRow, mixed.KindActor)
	}

	// The other direction -- a declared kind with no matching path -- is the
	// sentence declared_usage_validator.go credited to a validator that did
	// not exist ("the body contains at least one matching path").
	if _, err := convertShape(t, "@row\n@actor\nshape workspace unusedActor {\n  planId\n}"); err == nil {
		t.Fatal("a declared @actor with no actor.* path must be refused")
	} else if !strings.Contains(err.Error(), "declares @actor but projects no") {
		t.Fatalf("error = %v, want the unused-@actor message", err)
	}
	if _, err := convertShape(t, "@row\n@actor\nshape unusedRow {\n  actor.userId\n}"); err == nil {
		t.Fatal("a declared @row with no row path must be refused")
	} else if !strings.Contains(err.Error(), "declares @row but projects no") {
		t.Fatalf("error = %v, want the unused-@row message", err)
	}
}

// TestEmptyBodyShapeCarriesItsDeclaredKind pins the other half of hole 2:
// shape_converter.go hardcoded KindRow: true on the empty-body path, so an
// @actor-only shape came back claiming to be a row shape.
func TestEmptyBodyShapeCarriesItsDeclaredKind(t *testing.T) {
	if _, err := convertShape(t, "@actor\nshape workspace emptyActor {\n}"); err == nil {
		t.Fatal("an empty body under @actor alone must be refused -- the default projection is payload-only")
	} else if !strings.Contains(err.Error(), "does not declare @row") {
		t.Fatalf("error = %v, want it to name the missing @row", err)
	}

	shape, err := convertShape(t, "@row\nshape workspace emptyRow {\n}")
	if err != nil {
		t.Fatalf("empty-body @row shape must load: %v", err)
	}
	if !shape.DefaultProjection {
		t.Fatal("empty body must mark DefaultProjection")
	}
	if !shape.KindRow || shape.KindActor {
		t.Fatalf("KindRow=%v KindActor=%v, want row-only", shape.KindRow, shape.KindActor)
	}
}

// TestShapeActorMembersAreAClosedSet pins the third part of hole 2. The
// runtime renderer (extractNodeFieldValue) has no actor case at all, so an
// unknown member does not even fail when the shape runs -- it renders nil.
// validateActorMemberPaths (#2623 / #2625) closed this set for Query /
// Mutation / Logic / Automation and stopped short of shapes.
func TestShapeActorMembersAreAClosedSet(t *testing.T) {
	for _, member := range []string{"displayName", "bogusMember", "partitions"} {
		t.Run(member, func(t *testing.T) {
			_, err := convertShape(t, "@actor\nshape actorBad {\n  actor."+member+"\n}")
			if err == nil {
				t.Fatalf("actor.%s must be refused", member)
			}
			if !strings.Contains(err.Error(), "closed set") {
				t.Fatalf("error = %v, want the closed-set message", err)
			}
		})
	}

	// actor.config.<key> is retired (#2623) and gets its own correction.
	_, err := convertShape(t, "@actor\nshape actorConfig {\n  actor.config.NOT_ALLOWLISTED\n}")
	if err == nil {
		t.Fatal("actor.config.<key> must be refused")
	}
	if !strings.Contains(err.Error(), "retired") {
		t.Fatalf("error = %v, want the actor.config retirement message", err)
	}

	// The canonical envelope still loads, alias included.
	if _, err := convertShape(t, "@actor\nshape actorOk {\n  actor.userId\n  actor.role\n  actor.identityId\n  actor.isClusterOwner\n  actor.primaryEmail\n  actor.now\n  actor.isOwner\n}"); err != nil {
		t.Fatalf("the canonical actor envelope must load: %v", err)
	}
}

// TestShapeIncludeIsRefused pins latent hole 3. CLAUDE.md promised
// "transitive inclusion is supported, cycles + field collisions are errors";
// no such code ever existed. parseShapeDecl reads a body as a path list, so
// `include base` became two payload paths and two always-null keys. Zero real
// shapes used it, so the decision was to remove the promise, not build the
// feature -- and to say so at the one moment an author would find out.
func TestShapeIncludeIsRefused(t *testing.T) {
	_, err := convertShape(t, "@row\nshape workspace usesInclude {\n  include otherShape\n}")
	if err == nil {
		t.Fatal("want a load error for `include`, got nil")
	}
	for _, want := range []string{"include", "NOT implemented"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want it to contain %q", err, want)
		}
	}
}

// TestShapeBoundConceptMustResolve pins latent hole 4, first half: a shape
// bound to a concept that does not resolve used to load in silence (and on
// the empty-body path, failure was a Warn that left an empty projection).
func TestShapeBoundConceptMustResolve(t *testing.T) {
	_, concepts := loadCorpusShapes(t)

	shapes := newShapeRegistry()
	if err := shapes.Upsert(&ShapeDefinition{
		Name:        "boundToNothing",
		Origin:      "unified:workbench/shapes.memql",
		KindRow:     true,
		UseConcepts: []string{"noSuchConceptAnywhere"},
		Template:    map[string]any{"planId": `node(\"payload.planId\")`},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	violations := validateShapeConceptBindings(shapes, concepts)
	if len(violations) != 1 {
		t.Fatalf("violations = %d, want 1: %+v", len(violations), violations)
	}
	if !strings.Contains(violations[0].Detail, "does not resolve") {
		t.Fatalf("detail = %q, want it to say the binding does not resolve", violations[0].Detail)
	}
}

// TestAmbiguousShapeBindingResolvesByDomain pins latent hole 4, second half.
// Four real shapes bind a bare name that is ambiguous cluster-wide
// (forge/requestFull, planner/planFull, telephony/callFull,
// worker/workerInvocationFull). With an explicit body nothing broke, because
// the binding was only consulted by the default-projection expansion -- so
// converting any of them to the empty-body form (memql#2035, the encouraged
// direction) produced an EMPTY projection and a log line. This asserts the
// conversion now works, and that an origin carrying no domain is reported
// rather than silently empty.
func TestAmbiguousShapeBindingResolvesByDomain(t *testing.T) {
	_, concepts := loadCorpusShapes(t)

	// Precondition: `plan` really is ambiguous by bare name.
	if _, err := resolveConceptByTrailingSegment(concepts, "plan"); err == nil {
		t.Skip("`plan` is no longer an ambiguous trailing segment; the hole this pins is gone")
	}

	shapes := newShapeRegistry()
	if err := shapes.Upsert(&ShapeDefinition{
		Name:              "planFullDefaulted",
		Origin:            "unified:planner/shapes.memql",
		KindRow:           true,
		UseConcepts:       []string{"plan"},
		Template:          map[string]any{},
		DefaultProjection: true,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if violations := validateShapeConceptBindings(shapes, concepts); len(violations) != 0 {
		t.Fatalf("domain-hinted binding must resolve, got %+v", violations)
	}

	if expanded := expandDefaultShapeProjections(quietLogger(), shapes, concepts); expanded != 1 {
		t.Fatalf("expanded = %d, want 1 -- an ambiguous binding must resolve through the shape's domain", expanded)
	}
	got, _ := shapes.Get("planFullDefaulted")
	if got == nil || len(got.Template) == 0 {
		t.Fatal("default projection is empty -- the ambiguous binding silently produced nothing")
	}
	if _, ok := got.Template["goal"]; !ok {
		t.Fatalf("expanded template %v does not look like v1:planner:plan (no `goal`)", sortedTemplateKeys(got.Template))
	}

	// Same binding, no domain in the origin: unresolvable, and now LOUD.
	orphan := newShapeRegistry()
	if err := orphan.Upsert(&ShapeDefinition{
		Name:              "planFullNoDomain",
		Origin:            "unified:shapes.memql",
		KindRow:           true,
		UseConcepts:       []string{"plan"},
		Template:          map[string]any{},
		DefaultProjection: true,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	violations := validateShapeConceptBindings(orphan, concepts)
	if len(violations) != 1 {
		t.Fatalf("violations = %d, want 1 for an ambiguous binding with no domain: %+v", len(violations), violations)
	}
	if !strings.Contains(violations[0].Detail, "ambiguous") {
		t.Fatalf("detail = %q, want it to name the ambiguity", violations[0].Detail)
	}
}
