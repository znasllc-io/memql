package app

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/memql"
)

// Every (concept, vector field) pair an indexing writer embeds under has at
// least one rebuild source, so a memory-search model switch cannot leave a
// kind of item out of the new index. The writers:
//
//	v1:work:observation       component/memql/executor_mutation.go embedWorkObservation
//	                          (lessons: integrations/library/training_lessons.go handleIndexTrainingLesson)
//	v1:memory:conversationEvidence  component/memql/work_memory_index.go indexConversationEvidence
//	v1:memory:workAcknowledgement   component/memql/work_acknowledgement.go
//	v1:authoring:construct    component/memql/authoring_catalog_promote.go embedCatalogConstruct
//	v1:library:fileChunk      integrations/library/analysis.go embedChunk
//	v1:knowledge:documentChunk integrations/knowledge/capabilities.go storeVector callers
//
// The blank imports of the library and knowledge integrations in
// plugins_core.go are what register theirs.
func TestEveryIndexedPairHasASource(t *testing.T) {
	have := map[[2]string][]string{}
	for _, s := range memql.MemorySources() {
		k := [2]string{s.Concept, s.VectorField}
		have[k] = append(have[k], s.Name)
	}
	for _, p := range [][2]string{
		{"v1:work:observation", "content"},
		{"v1:memory:conversationEvidence", "content"},
		{"v1:memory:workAcknowledgement", "content"},
		{"v1:authoring:construct", "content"},
		{"v1:library:fileChunk", "content"},
		{"v1:knowledge:documentChunk", "content"},
	} {
		require.NotEmpty(t, have[p], "%v has no memory source", p)
	}
	require.ElementsMatch(t, []string{"trainingLesson", "workObservation"}, have[[2]string{"v1:work:observation", "content"}])
}
