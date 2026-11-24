package memql

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// pipelineRunsBrowse and pipelineRunsBrowseAll share one facet block and differ
// only by the owner conjunct; a change to one list must land in the other.
func TestRunBrowseFacetsMatch(t *testing.T) {
	src, err := os.ReadFile("../../dsl/pipelines/queries.memql")
	require.NoError(t, err)
	filter := func(name string) string {
		m := regexp.MustCompile(`(?s)query run ` + name + ` \{.*?\n  filter\s+(.*?)\n  sort`).FindStringSubmatch(string(src))
		require.Len(t, m, 2, name)
		return strings.Join(strings.Fields(m[1]), " ")
	}
	owner := strings.Replace(filter("pipelineRunsBrowse"), "row => row.ownerUserId == actor.userId && ", "row => ", 1)
	require.Equal(t, filter("pipelineRunsBrowseAll"), owner)
}
