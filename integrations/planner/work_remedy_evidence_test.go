package planner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReplanEvidenceBoundsRepeatedBodiesWithoutLosingPrefix(t *testing.T) {
	large := "BEGIN " + strings.Repeat("章", 20000) + " END"
	receipts := []map[string]any{
		{"key": "small", "call": map[string]any{"name": "gather"}, "result": map[string]any{"value": "12 tickets", "result": "DUPLICATE ENVELOPE"}},
		{"key": "chapter", "call": map[string]any{"name": "draft"}, "result": map[string]any{"value": large}},
		{"key": "sections", "result": map[string]any{"value": map[string]any{"chapter": large, "again": large}}},
	}
	before, _ := json.Marshal(receipts)
	projected, err := projectReplanEvidence(receipts, 100, 150)
	if err != nil || len(projected) != len(receipts) {
		t.Fatalf("lost prefix: %v %v", projected, err)
	}
	if projected[0]["result"] != "12 tickets" {
		t.Fatal("small semantic result lost or duplicated")
	}
	for j := 1; j < len(projected); j++ {
		preview := projected[j]["resultPreview"].(string)
		if !utf8.ValidString(preview) || projected[j]["resultOmitted"] != true || len(projected[j]["resultSHA256"].(string)) != 64 {
			t.Fatalf("unmarked or invalid preview: %v", projected[j])
		}
	}
	if !strings.Contains(projected[1]["resultPreview"].(string), "BEGIN") || !strings.Contains(projected[1]["resultPreview"].(string), "END") {
		t.Fatal("preview lost both-boundary context")
	}
	encoded, _ := json.Marshal(projected)
	if len(encoded) > 1500 || strings.Contains(string(encoded), "DUPLICATE ENVELOPE") {
		t.Fatalf("unbounded/duplicated prompt: %d bytes", len(encoded))
	}
	after, _ := json.Marshal(receipts)
	if string(before) != string(after) {
		t.Fatal("prompt compaction changed executable journal")
	}
	// Exercise the actual DSL policy, not just a hand-authored codec setting.
	view, err := replanEvidenceView(context.Background(), receipts)
	encoded, _ = json.Marshal(view)
	if err != nil || len(encoded) > 6000 {
		t.Fatalf("DSL projection missing: bytes=%d err=%v", len(encoded), err)
	}
}

func TestReplanEvidenceRetainsIdentitiesAfterPreviewBudgetRunsOut(t *testing.T) {
	rows := []map[string]any{{"key": "first", "result": strings.Repeat("x", 200)}, {"key": "last", "result": strings.Repeat("y", 200)}}
	view, err := projectReplanEvidence(rows, 10, 10)
	if err != nil || len(view) != 2 || view[1]["key"] != "last" || view[1]["resultOmitted"] != true {
		t.Fatalf("budget removed completed identity: %v %v", view, err)
	}
	if strings.Contains(view[1]["resultPreview"].(string), "yyy") {
		t.Fatal("exhausted preview allowance was exceeded")
	}
}

func TestReplanUsesBoundedViewButKeepsFullJournalForInstallation(t *testing.T) {
	eng := &replanEngine{answer: replanAnswer(t, replanSource, false)}
	rc := replanFixture()
	large := strings.Repeat("retained evidence ", 20000)
	rc.CompletedSteps[0]["result"] = map[string]any{"value": large, "result": large}
	rec := &remedyRecorder{context: rc}
	if !newRemedy(eng, rec).Replan(context.Background(), "v1:work:run:r1", "u1", "draft", "finish remaining work") || len(rec.installed) != 1 {
		t.Fatalf("replan failed: %v", rec.asked)
	}
	sent, _ := json.Marshal(eng.aiData[0]["completedSteps"])
	if len(sent) > 2000 || !strings.Contains(string(sent), "resultOmitted") {
		t.Fatalf("model received full duplicate results: %d bytes", len(sent))
	}
	if rc.CompletedSteps[0]["result"].(map[string]any)["value"] != large || rec.installed[0].ResumeAt != "write" {
		t.Fatal("context view changed saved results or resumed prefix")
	}
}
