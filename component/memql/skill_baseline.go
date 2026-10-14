package memql

import (
	"bytes"
	"encoding/json"
)

// IsInstalledSkillBaseline verifies content against the currently installed
// DSL seed, not the mutable predefined flag of a public catalog row. Learned
// or edited content must use its own review path; it cannot impersonate a
// reviewed release by copying that flag or a release's version number.
func (e *MemQLEngine) IsInstalledSkillBaseline(row map[string]any) bool {
	if e == nil || e.seeds == nil {
		return false
	}
	id, _ := row["id"].(string)
	for _, seed := range e.seeds.All() {
		// Signature-form declarations resolve the unique concept at load time
		// and keep UseNamespace empty, as the seed materializer does.
		if (seed.UseNamespace != "" && seed.UseNamespace != "skills") || seed.UseConcept != "skill" || seed.Scope != "global" {
			continue
		}
		seedID, _ := seedValueToInterface(seed.Body.fields["id"]).(string)
		if BareShortId(id) != BareShortId(seedID) {
			continue
		}
		expected := buildArgsFromBody(seed.Body, seed.UseConcept, seedID, "")
		for _, key := range []string{"name", "version", "instructions", "tags", "predefined", "active", "status"} {
			if _, ok := expected[key]; !ok {
				return false
			} // automatic binding requires explicit reviewed content/version/state
		}
		// These createSkill defaults also form part of the release content.
		// An extra script/tool under otherwise unchanged instructions is not a
		// reviewed baseline. Runtime counters and timestamps are not content.
		for _, key := range []string{"scripts", "resources", "constructRefs", "toolSlugs", "liveSourceIds", "domainIds"} {
			if _, ok := expected[key]; !ok {
				expected[key] = []any{}
			}
		}
		if _, ok := expected["effects"]; !ok {
			expected["effects"] = map[string]any{}
		}
		for key, v := range expected {
			if key == "id" || key == "skillId" {
				continue // resolved as a bare id above
			}
			actual := row[key]
			if actual == nil {
				// Unmaterialized seed values used by inspection/tests omit empty
				// defaults; the stored mutation explicitly writes the same empties.
				switch empty := v.(type) {
				case []any:
					if len(empty) == 0 {
						actual = empty
					}
				case map[string]any:
					if len(empty) == 0 {
						actual = empty
					}
				}
			}
			want, err := json.Marshal(v)
			if err != nil {
				return false
			}
			got, err := json.Marshal(actual)
			if err != nil || !bytes.Equal(want, got) {
				return false
			}
		}
		return true
	}
	return false
}
