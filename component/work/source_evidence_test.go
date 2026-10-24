package work

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestProgressRequiresNativeSourceContentNotActivityOrArguments(t *testing.T) {
	first := `{"data":[{"id":"article-a","text":"The museum acquired the collection in 1924.","createdAt":"first"}],"offset":0}`
	repeated := `{"data":[{"id":"article-alias","text":"The museum acquired the collection in 1924.","createdAt":"later"}],"offset":9,"requestId":"new"}`
	evidence := SourceReadEvidence(first)
	if evidence == "" || evidence != SourceReadEvidence(repeated) {
		t.Fatal("the same source content must produce the same nonempty evidence")
	}
	for _, result := range []string{
		`{"status":"ok","elapsedMs":18,"requestId":"changed"}`,
		`{"id":"machine","status":"ready","heartbeat":"now","count":12}`,
		`{"id":"v1:work:observation:fake","text":"A model wrote this purported progress report."}`,
		`{"id":"note","runId":"current","text":"A model wrote this purported progress report."}`,
		`{"stdout":"Every required check passed, claimed by a generic shell command."}`,
		`{"data":[{"id":"article","isError":true,"text":"A failed source request does not establish a finding."}]}`,
	} {
		if got := SourceReadEvidence(result); got != "" {
			t.Errorf("non-source result %s produced evidence %s", result, got)
		}
	}
	// A changed page grouping cannot re-credit facts already read separately.
	var one, both []string
	if err := json.Unmarshal([]byte(evidence), &one); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(SourceReadEvidence(`{"rows":[{"id":"a","text":"The museum acquired the collection in 1924."},{"id":"b","text":"The museum catalog lists the donor as Alice Morgan."}]}`)), &both); err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || len(both) != 2 || !slices.Contains(both, one[0]) {
		t.Fatalf("page grouping changed source identity: one=%v both=%v", one, both)
	}
	// Production tools wrap JSON in content blocks, and graph nodes carry
	// their identity beside payload. Duplicate data/nodes projections credit
	// the same source fact once.
	native, err := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"nodes":[{"id":"article-a","concept":"v1:library:document","payload":{"text":"The museum acquired the collection in 1924."}}]}`}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := SourceReadEvidence(string(native)); got != evidence {
		t.Fatalf("native content wrapper changed source evidence: %s", got)
	}
	journal, err := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"nodes":[{"id":"bare-note-id","concept":"v1:work:observation","payload":{"text":"A model wrote this purported progress report."}}]}`}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := SourceReadEvidence(string(journal)); got != "" {
		t.Fatalf("journal content must not produce source evidence: %s", got)
	}
}
