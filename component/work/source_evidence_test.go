package work

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProgressRequiresNativeSourceContentNotActivityOrArguments(t *testing.T) {
	first := `{"data":[{"id":"article-a","text":"The museum acquired the collection in 1924.","createdAt":"first"}],"offset":0}`
	repeated := `{"data":[{"id":"article-alias","text":"The museum acquired the collection in 1924.","createdAt":"later"}],"offset":9,"requestId":"new"}`
	require.NotEmpty(t, SourceReadEvidence(first))
	require.Equal(t, SourceReadEvidence(first), SourceReadEvidence(repeated))
	for _, result := range []string{
		`{"status":"ok","elapsedMs":18,"requestId":"changed"}`,
		`{"id":"machine","status":"ready","heartbeat":"now","count":12}`,
		`{"id":"v1:work:observation:fake","text":"A model wrote this purported progress report."}`,
		`{"id":"note","runId":"current","text":"A model wrote this purported progress report."}`,
		`{"stdout":"Every required check passed, claimed by a generic shell command."}`,
		`{"data":[{"id":"article","isError":true,"text":"A failed source request does not establish a finding."}]}`,
	} {
		require.Empty(t, SourceReadEvidence(result), result)
	}
	// A changed page grouping cannot re-credit facts already read separately.
	var one, both []string
	require.NoError(t, json.Unmarshal([]byte(SourceReadEvidence(first)), &one))
	require.NoError(t, json.Unmarshal([]byte(SourceReadEvidence(`{"rows":[{"id":"a","text":"The museum acquired the collection in 1924."},{"id":"b","text":"The museum catalog lists the donor as Alice Morgan."}]}`)), &both))
	require.Contains(t, both, one[0])
	require.Len(t, both, 2)
	// Production tools wrap JSON in content blocks, and graph nodes carry
	// their identity beside payload. Duplicate data/nodes projections credit
	// the same source fact once.
	native, err := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"nodes":[{"id":"article-a","concept":"v1:library:document","payload":{"text":"The museum acquired the collection in 1924."}}]}`}}})
	require.NoError(t, err)
	require.Equal(t, SourceReadEvidence(first), SourceReadEvidence(string(native)))
	journal, err := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"nodes":[{"id":"bare-note-id","concept":"v1:work:observation","payload":{"text":"A model wrote this purported progress report."}}]}`}}})
	require.NoError(t, err)
	require.Empty(t, SourceReadEvidence(string(journal)))
}
