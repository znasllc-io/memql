package work

import (
	"reflect"
	"testing"
)

func baseline(id, term, body string) map[string]any {
	return map[string]any{"id": id, "name": id, "version": 1, "status": "active", "active": true, "predefined": true, "reviewedBaseline": true, "tags": []string{term}, "instructions": map[string]any{"body": body, "selectionTerms": []string{term}}}
}

func TestSkillSelectionPinsMultipleExactVersionsAndExplainsExclusions(t *testing.T) {
	rows := []map[string]any{baseline("testing", "testing", "Write a failing regression first."), baseline("software", "software", "Preserve unrelated edits."), baseline("other", "calendar", "Meetings.")}
	forged := baseline("forged", "software", "Unreviewed advice.")
	forged["reviewedBaseline"] = false
	rows = append(rows, forged)
	s, err := SelectSkillContext("Software testing", rows, 3, 1, 8000)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Selected) != 2 || s.Selected[0].ID != "software" || s.Selected[1].ID != "testing" {
		t.Fatalf("selection %+v", s)
	}
	if s.Selected[0].Text != "Preserve unrelated edits." || len(s.Selected[0].Digest) != 64 || s.Selected[0].Version != 1 {
		t.Fatal("lost content/version evidence")
	}
	if len(s.Rejected) != 2 || s.Rejected[0].Reason != "not-active-reviewed-versioned-content" || s.Rejected[1].Reason != "below-declared-match-floor" {
		t.Fatalf("exclusion evidence %+v", s.Rejected)
	}
	for _, c := range s.Rejected {
		if c.Text != "" {
			t.Fatal("rejected advice entered context")
		}
	}
	rows[0], rows[1] = rows[1], rows[0]
	again, err := SelectSkillContext("Software testing", rows, 3, 1, 8000)
	if err != nil || !reflect.DeepEqual(s, again) {
		t.Fatal("catalog ordering changed selection")
	}
}

func TestSkillSelectionNeverTruncatesInstructionsOrSelectsUnreviewedVersions(t *testing.T) {
	rows := []map[string]any{baseline("one", "software", "Complete reviewed text.")}
	s, err := SelectSkillContext("software", rows, 1, 1, 1)
	if err != nil || len(s.Selected) != 0 || s.Rejected[0].Reason != "context-allocation" {
		t.Fatalf("%+v %v", s, err)
	}
	for _, v := range []any{0, -1, 1.5, 1e100, "1"} {
		rows[0]["version"] = v
		s, err := SelectSkillContext("software", rows, 1, 1, 8000)
		if err != nil || len(s.Selected) != 0 {
			t.Fatalf("invalid version %v selected", v)
		}
	}
}

func TestSkillDigestIdentifiesContentAcrossInstallationAndUsageMetadata(t *testing.T) {
	row := baseline("software", "software", "Preserve the user's work.")
	selectOne := func() SkillContext {
		t.Helper()
		s, err := SelectSkillContext("software", []map[string]any{row}, 1, 1, 8000)
		if err != nil || len(s.Selected) != 1 {
			t.Fatalf("%+v: %v", s, err)
		}
		return s.Selected[0]
	}
	before := selectOne()
	row["createdAt"] = "another installation's timestamp"
	row["reinforceCount"] = 100
	row["lastReinforced"] = "later"
	if after := selectOne(); before.Digest != after.Digest {
		t.Fatal("metadata changed content identity")
	}
	row["instructions"].(map[string]any)["body"] = "Changed instructions."
	if after := selectOne(); before.Digest == after.Digest {
		t.Fatal("changed content retained its digest")
	}
}
