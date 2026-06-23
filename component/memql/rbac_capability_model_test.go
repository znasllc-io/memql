package memql

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestRBACCapabilityModelExpressible is the E1.1 (memql#2069) acceptance
// gate: the primitive verbs + capability model must be EXPRESSIBLE and
// QUERYABLE in the DSL. It loads the embedded tree DB-free (the same
// loader path TestEngineInitLoadsFullDSL / TestEmbeddedDSLLoadsCleanly
// exercise) and asserts:
//
//  1. the v1:rbac:capability concept loads with the closed 5-verb enum
//     (read/create/update/delete/execute);
//  2. the capability catalog read surface (queries) + the createCapability
//     write surface + the capability row-specs are registered, i.e. the
//     model is queryable;
//  3. the base-role capability SEED grant matrix is present and matches the
//     behavior currently hardcoded in component/auth/rbac.go -- including
//     the load-bearing distinctions the later children key on: the
//     create != update split on `principal`, developer-authors-but-doesn't-
//     manage-users, and admin-manages-users-but-doesn't-author.
//
// Multi-node note: the capability catalog is global/_system reference data
// with no per-user ownership, so an enforcement decision resolves
// identically on any node from the same seeded rows. This test pins the
// catalog content that makes that node-agnostic resolution correct.
func TestRBACCapabilityModelExpressible(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	concepts := memoryNodes.DefaultRegistry()

	// --- 1. concept + verb enum -------------------------------------------
	capDef, err := concepts.Get("v1:rbac:capability")
	if err != nil || capDef == nil {
		t.Fatalf("v1:rbac:capability concept not registered: %v", err)
	}
	wantVerbs := map[string]bool{
		"read": true, "create": true, "update": true, "delete": true, "execute": true,
	}
	gotVerbs := enumValuesForField(t, capDef, "verb")
	if len(gotVerbs) != len(wantVerbs) {
		t.Fatalf("capability.verb enum = %v, want exactly the 5 primitive verbs read/create/update/delete/execute", gotVerbs)
	}
	for _, v := range gotVerbs {
		if !wantVerbs[v] {
			t.Errorf("capability.verb enum carries unexpected verb %q (the verb set must be exactly read/create/update/delete/execute)", v)
		}
	}

	// --- 2. queryable: read + write surface + specs -----------------------
	specSchemaIdx, err := buildSchemaIndex(concepts)
	if err != nil {
		t.Fatalf("buildSchemaIndex: %v", err)
	}
	specRegistry, err := loadEmbeddedSpecs(logger, specSchemaIdx)
	if err != nil {
		t.Fatalf("loadEmbeddedSpecs: %v", err)
	}
	if _, err := LoadUnifiedSpecs(logger, specRegistry); err != nil {
		t.Fatalf("LoadUnifiedSpecs: %v", err)
	}
	for _, name := range []string{"capabilityIsAllow", "capabilityIsDeny", "capabilityIsPredefined"} {
		if !specRegistry.Has(name) {
			t.Errorf("capability row-trait %q not registered -- the filter vocabulary is missing", name)
		}
	}

	functionRegistry, err := loadEmbeddedFunctions(logger, concepts)
	if err != nil {
		t.Fatalf("loadEmbeddedFunctions: %v", err)
	}
	if _, _, err := LoadUnifiedFunctions(logger, functionRegistry, concepts); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	wantFns := []string{
		// read surface (capabilities are queryable)
		"activeCapabilities", "capabilitiesForRole", "capabilityGrant", "capabilitiesForResourceType",
		// write surface (seed materializer + E1.4 custom roles)
		"createCapability",
	}
	for _, name := range wantFns {
		if !functionRegistry.Has(name) {
			t.Errorf("rbac function %q not registered -- the capability model is not queryable/writable", name)
		}
	}

	// --- 3. base-role grant matrix (loaded seeds) -------------------------
	seedRegistry := NewSeedRegistry()
	if _, err := LoadUnifiedSeeds(logger, seedRegistry); err != nil {
		t.Fatalf("LoadUnifiedSeeds: %v", err)
	}
	grants := collectCapabilityGrants(t, seedRegistry)

	// helper closures over the loaded grant set.
	has := func(role, verb, resource string) bool {
		return grants[capKey{role, verb, resource}]
	}
	mustHave := func(role, verb, resource string) {
		if !has(role, verb, resource) {
			t.Errorf("base role %q is MISSING capability (%s, %s) -- behavior regression vs component/auth/rbac.go", role, verb, resource)
		}
	}
	mustNotHave := func(role, verb, resource string) {
		if has(role, verb, resource) {
			t.Errorf("base role %q unexpectedly HOLDS capability (%s, %s) -- behavior regression vs component/auth/rbac.go", role, verb, resource)
		}
	}

	// owner: full authority incl. principal management.
	mustHave("owner", "create", "principal")
	mustHave("owner", "update", "principal")
	mustHave("owner", "delete", "principal")
	mustHave("owner", "create", "construct")
	mustHave("owner", "execute", "deployment")

	// developer: engineering power (authors + inline + data + forward deploy)
	// but NO user management -- the developer-outranks-admin power axis is
	// engineering, not principals.
	mustHave("developer", "create", "construct")  // CanAuthor
	mustHave("developer", "execute", "construct")  // CanRunInline
	mustHave("developer", "execute", "deployment") // AtLeastDeveloper forward deploy
	mustHave("developer", "create", "data")        // CanWrite
	mustNotHave("developer", "create", "principal")
	mustNotHave("developer", "update", "principal")
	mustNotHave("developer", "delete", "principal")

	// admin: user-management power (full principal verbs) but NOT authoring.
	mustHave("admin", "create", "principal") // CanManageUser create half
	mustHave("admin", "update", "principal") // CanManageUser update half
	mustHave("admin", "delete", "principal")
	mustHave("admin", "create", "agent")        // CanCreateAgent
	mustHave("admin", "create", "group")        // CanManageGroup
	mustHave("admin", "execute", "deployment")  // AtLeastAdmin
	mustNotHave("admin", "create", "construct") // admin does NOT author (CanAuthor)
	mustNotHave("admin", "update", "construct")
	mustNotHave("admin", "execute", "construct") // admin does NOT run inline DSL

	// user (member tier): read/write data plane, no management/authoring/deploy.
	mustHave("user", "create", "data") // CanWrite
	mustHave("user", "update", "data")
	mustHave("user", "read", "data")
	mustNotHave("user", "create", "principal")
	mustNotHave("user", "create", "construct")
	mustNotHave("user", "execute", "deployment")

	// The create != update split is load-bearing for E1.3 governance: admin
	// holds create AND update on principal, but the model must be able to
	// express create-without-update. Prove both verbs are independently
	// representable by confirming they are distinct grant rows (not folded
	// into one), so a custom role in E1.4 can hold one without the other.
	if !has("admin", "create", "principal") || !has("admin", "update", "principal") {
		t.Error("create and update on principal must be independently expressible grants (the create != update split E1.3 governance keys on)")
	}
}

// capKey identifies a (role, verb, resourceType) grant.
type capKey struct{ role, verb, resource string }

// collectCapabilityGrants walks every loaded seed that targets the
// v1:rbac:capability concept and returns the set of ALLOW grants keyed by
// (roleSlug, verb, resourceType). Deny grants are excluded (none seeded in
// E1.1). Reads the in-package seed body directly -- no DB required.
func collectCapabilityGrants(t *testing.T, reg *SeedRegistry) map[capKey]bool {
	t.Helper()
	out := map[capKey]bool{}
	for _, def := range reg.All() {
		if def.UseConcept != "capability" {
			continue
		}
		role := def.Body.fields["roleSlug"].str
		verb := def.Body.fields["verb"].str
		resource := def.Body.fields["resourceType"].str
		if role == "" || verb == "" || resource == "" {
			t.Errorf("capability seed %q has an empty roleSlug/verb/resourceType (%q/%q/%q)", def.Name, role, verb, resource)
			continue
		}
		// effect defaults to allow; treat an explicit "deny" as not-granted.
		if eff := def.Body.fields["effect"].str; eff == "deny" {
			continue
		}
		out[capKey{role, verb, resource}] = true
	}
	if len(out) == 0 {
		t.Fatal("no v1:rbac:capability seeds loaded -- the base-role grant catalog is empty")
	}
	return out
}

// enumValuesForField returns the enum value set declared for a concept
// field, read from the concept's definition JSON-schema
// (properties.<field>.enum). Fails the test if the field is not a
// non-empty string enum.
func enumValuesForField(t *testing.T, c *memoryNodes.Concept, field string) []string {
	t.Helper()
	raw, err := c.DefinitionSchema()
	if err != nil {
		t.Fatalf("DefinitionSchema: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("unmarshal definition schema: %v", err)
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("definition schema has no properties block")
	}
	prop, ok := props[field].(map[string]any)
	if !ok {
		t.Fatalf("concept has no %q property", field)
	}
	rawEnum, ok := prop["enum"].([]any)
	if !ok || len(rawEnum) == 0 {
		t.Fatalf("concept field %q is not a non-empty enum (got %v)", field, prop["enum"])
	}
	out := make([]string, 0, len(rawEnum))
	for _, v := range rawEnum {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
