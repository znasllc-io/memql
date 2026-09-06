package agents

// memql#3616: the agentFactoryAnalyze prompt's input schema is
// additionalProperties:false and is validated BEFORE the template renders,
// so the map analyzeGoal builds must match the declaration exactly. It did
// not: three keys the prompt never declared (domainCatalog /
// liveSourceCatalog / toolCatalog, marked "the prompt tolerates empty" --
// but the schema rejects the KEY, not the value) and one required key that
// was never passed at all (skillCatalog). Every ensureAgentForGoal call
// therefore errored in analysis, so automatic agent provisioning could not
// succeed.
//
// This test drives the REAL payload builder against the REAL prompt schema
// loaded from the DSL tree, so it fails the moment the two drift again.

import (
	"encoding/json"
	"testing"
	"text/template"

	"github.com/znasllc-io/memql/component/memql"
)

func loadAgentFactoryAnalyzePrompt(t *testing.T) *memql.PromptTemplate {
	t.Helper()
	registry := memql.NewPromptRegistry()
	if _, err := memql.LoadUnifiedPrompts(nil, registry, template.New("partials")); err != nil {
		t.Fatalf("LoadUnifiedPrompts: %v", err)
	}
	prompt, ok := registry.Get("agentFactoryAnalyze")
	if !ok || prompt == nil {
		t.Fatal("agentFactoryAnalyze prompt not registered")
	}
	return prompt
}

// normalizeForSchema mirrors aiRuntime.normalizeAIData: the runtime
// JSON-round-trips a caller's payload before ValidateData, because the
// schema validator only recognises encoding/json-native containers. The
// validation this test performs must run on the same bytes the runtime
// validates.
func normalizeForSchema(t *testing.T, data map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return out
}

// TestAnalyzeGoalPayloadSatisfiesPromptSchema is the acceptance: the exact
// map analyzeGoal hands to InvokeAIStructured validates clean.
func TestAnalyzeGoalPayloadSatisfiesPromptSchema(t *testing.T) {
	prompt := loadAgentFactoryAnalyzePrompt(t)

	data := analyzeGoalPromptData(
		"help me keep the books",
		[]agentSnapshot{{Id: "v1:agents:agent:a1", Name: "Ada", RoleSlug: "accountant", SkillIds: []string{"bookkeeping"}}},
		[]roleSnapshot{{Slug: "accountant", Name: "Accountant", Category: "finance", Tier: "B", MaxSkills: 8}},
		[]skillSnapshot{{Slug: "bookkeeping", Name: "Bookkeeping", Category: "finance", Description: "Ledgers", Tier: "B", Tags: []string{"finance"}}},
		"2026-08-13T00:00:00Z",
		"",
	)

	if err := prompt.ValidateData(normalizeForSchema(t, data)); err != nil {
		t.Fatalf("the payload analyzeGoal builds must satisfy the prompt schema: %v", err)
	}
}

// TestRetryPayloadSatisfiesPromptSchema is the memql#4690 half of the same
// contract, and the one worth stating separately: the retry payload carries an
// EXTRA key (priorError). The schema is additionalProperties:false and is
// validated before the template renders, so an undeclared key does not degrade
// to "the correction is missing" -- it fails the call. Every retry would error
// in analysis, which is precisely the failure the retry was added to prevent.
func TestRetryPayloadSatisfiesPromptSchema(t *testing.T) {
	prompt := loadAgentFactoryAnalyzePrompt(t)

	data := analyzeGoalPromptData("a goal", nil, nil, nil, "2026-08-13T00:00:00Z",
		`roleSlug "content-creator" is not in the role catalog. Choose one of: accountant`)

	if err := prompt.ValidateData(normalizeForSchema(t, data)); err != nil {
		t.Fatalf("the RETRY payload must satisfy the prompt schema too: %v", err)
	}
}

// TestAnalyzeGoalPayloadSatisfiesPromptSchemaWhenCatalogsEmpty: the
// best-effort loaders return nil on a query error, and "create" must stay
// achievable from an empty catalog. Empty slices are still values for
// required fields, so this has to validate too.
func TestAnalyzeGoalPayloadSatisfiesPromptSchemaWhenCatalogsEmpty(t *testing.T) {
	prompt := loadAgentFactoryAnalyzePrompt(t)

	data := analyzeGoalPromptData("a goal", nil, nil, nil, "2026-08-13T00:00:00Z", "")
	if err := prompt.ValidateData(normalizeForSchema(t, data)); err != nil {
		t.Fatalf("empty catalogs must still validate (the factory tolerates them): %v", err)
	}
}

// TestAnalyzeGoalPayloadCarriesNoUndeclaredKeys states the rule directly:
// the payload's key set must be a subset of what the prompt declares. The
// three phantom catalogs were each a real key that failed every call.
func TestAnalyzeGoalPayloadCarriesNoUndeclaredKeys(t *testing.T) {
	prompt := loadAgentFactoryAnalyzePrompt(t)

	declared := map[string]bool{}
	for _, arg := range prompt.Arguments() {
		name, _ := arg["name"].(string)
		declared[name] = true
	}
	if len(declared) == 0 {
		t.Fatal("prompt declares no arguments -- the check would pass vacuously")
	}

	data := analyzeGoalPromptData("a goal", nil, nil, nil, "2026-08-13T00:00:00Z", "")
	for key := range data {
		if !declared[key] {
			t.Errorf("payload key %q is not declared by agentFactoryAnalyze; "+
				"additionalProperties:false rejects it before the template renders, so every call fails", key)
		}
	}

	// And the mirror: every REQUIRED declared field is actually supplied.
	for _, arg := range prompt.Arguments() {
		name, _ := arg["name"].(string)
		required, _ := arg["required"].(bool)
		if required {
			if _, ok := data[name]; !ok {
				t.Errorf("required prompt field %q is never passed by analyzeGoal", name)
			}
		}
	}
}

// TestSkillCatalogForPromptKeysOnSlug pins the identity the model is asked
// to emit. Role rows store BARE slugs in lockedSkillIds / availableSkillIds
// / forbiddenSkillIds and buildCreateAgentArgs unions the decision straight
// onto those, so handing the model canonical `v1:skills:skill:<slug>` ids
// would silently break every subset + forbidden check.
func TestSkillCatalogForPromptKeysOnSlug(t *testing.T) {
	out := skillCatalogForPrompt([]skillSnapshot{{Slug: "bookkeeping", Name: "Bookkeeping"}})
	if len(out) != 1 {
		t.Fatalf("skillCatalogForPrompt returned %d entries, want 1", len(out))
	}
	if got := out[0]["slug"]; got != "bookkeeping" {
		t.Errorf("skill identity = %v, want the bare slug \"bookkeeping\"", got)
	}
	if _, hasId := out[0]["id"]; hasId {
		t.Error("the projection must not carry a canonical row id -- the model would emit it " +
			"and every role-catalog subset check compares bare slugs")
	}
}

// TestSkillSnapshotFromRowRequiresSlug: the slug is the identity the
// decision has to name, so a row without one is unusable rather than
// partially usable.
func TestSkillSnapshotFromRowRequiresSlug(t *testing.T) {
	if _, ok := skillSnapshotFromRow(map[string]any{"payload": map[string]any{"name": "No Slug"}}); ok {
		t.Error("a skill row with no slug must be dropped, not surfaced with an empty identity")
	}
	got, ok := skillSnapshotFromRow(map[string]any{
		"id":      "v1:skills:skill:bookkeeping",
		"payload": map[string]any{"slug": "bookkeeping", "name": "Bookkeeping", "tags": []any{"finance"}},
	})
	if !ok {
		t.Fatal("a well-formed skill row must decode")
	}
	if got.Slug != "bookkeeping" || got.Name != "Bookkeeping" || len(got.Tags) != 1 {
		t.Errorf("decoded snapshot = %+v", got)
	}
}
