package calls

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	contentid "github.com/znasllc-io/memql/core/id"
)

// Published versions are pinned by approvals/bookings. New built-in behavior
// must not silently rewrite any of the fourteen definitions already released.
// These content fingerprints were migrated from the fourteen SHA-256 goldens
// at 830d707bc only after every original golden matched the unchanged canonical
// definition bytes, independently verified in Go and Python. No catalog content
// was regenerated or changed when adopting the repository's core/id primitive.
func TestWorkflowCatalogPreservesPublishedV1Definitions(t *testing.T) {
	want := map[string]string{
		"working-session":     "23276ac2a28545a7e5b274a3eeebb5c45b4a6db28cd697e3b0ba74df1d63454c",
		"one-on-one":          "e1f69323c945d81955d45a0c67f384261ec9305b0edd83e8b2037662c1d2d010",
		"interview":           "3c17ee057dbe034696eb2cf5c63c2c1df18843bbc0835bec70d9607b9b58efac",
		"presentation":        "c59514c6d4b5683a9b383bbec380d0bc463fc3523fbc560f3fbffd6f0f949c12",
		"panel":               "4db8bbe577498f2d4c044d7c3ed7834bd5b44c7afde9de6dffcdf5e985f2140d",
		"standup":             "6e81f28db69b3fe881b1e614c4dd136c6b8d42a0ef5f04abba7a534da5733cf5",
		"retrospective":       "f6077787ab65e33093dbab2abc3c208cb5692b677f72b311c93564c93941d89d",
		"planning":            "af94070110b7ff12a0a02ed809ec0501d44e33d4976f5849dedf31602f15957e",
		"pair-programming":    "17c7cc5f08b676e67b743bfbe0a820b69d4fced2d4f193317cfd0557dbe253e2",
		"study-group":         "af713503e33d08206677ef4afbf18279ff45a4256e6927bf9b183d15cf28ffbf",
		"tutoring":            "a4f51ec9e632ac76feb83a9220c0a155ad375ffe568621c9d63fc5a14f3162fb",
		"supervised-learning": "e813d372c5d57b0f6da5562bf74768c358da2637d484d11ca3fb186e9b9f47dc",
		"office-hours":        "a17250ea7c6f000a14fc762e2aa15e7c8765b14e9402b9de7d78f2aec4f208e5",
		"demonstration":       "fee3eebf96dbb32dbbc33383bc192019aff00f8dbcb81a679256df287d0b497f",
	}
	var records []map[string]any
	require.NoError(t, json.Unmarshal(workflowCatalogJSON, &records))
	for _, record := range records {
		id, _ := record["id"].(string)
		if record["version"] != float64(1) || want[id] == "" {
			continue
		}
		canonical, err := json.Marshal(record)
		require.NoError(t, err)
		digest := contentid.New().FromString(string(canonical))
		require.Equal(t, want[id], string(digest), id+" v1 changed")
		delete(want, id)
	}
	require.Empty(t, want, "no previously published built-in may disappear")
}

func TestWorkflowCatalogExposesReviewedPresentationAndRealStructuredGuidance(t *testing.T) {
	catalog, err := workflowCatalog()
	require.NoError(t, err)
	byVersion := map[string]callrules.WorkflowDefinition{}
	for _, d := range catalog {
		byVersion[fmt.Sprintf("%s:%d", d.ID, d.Version)] = d
	}
	for _, id := range []string{"presentation", "office-hours"} {
		d, ok := byVersion[id+":2"]
		require.True(t, ok)
		require.Equal(t, int64(2), d.Revision)
		presentations := 0
		for _, node := range d.Nodes {
			if node.Type == "presentation" {
				presentations++
			}
			require.Nil(t, node.Effect, "catalog selection never authorizes an automatic media effect")
		}
		require.GreaterOrEqual(t, presentations, 2, "presenter controls require actual presentation visits")
		require.NotEmpty(t, byVersion[id+":1"].Nodes)
	}
	planning := byVersion["planning:2"]
	parallel := callrules.WorkflowParallelRegions(planning)
	require.Len(t, parallel, 1)
	require.Len(t, parallel["groups"].Branches, 2)
	require.Equal(t, "selection", parallel["groups"].AfterID)
	for _, branch := range parallel["groups"].Branches {
		exit, ok := callrules.WorkflowNodeByID(planning, branch.ExitID)
		require.True(t, ok)
		require.Equal(t, "acknowledgment", exit.Type)
		require.NotEmpty(t, exit.RoleID)
	}
	simulation := callrules.SimulateWorkflow(planning)
	require.EqualValues(t, 570, simulation.MaximumSeconds)
	require.Equal(t, 9, simulation.MaximumVisits)
	training := byVersion["training:1"]
	repeats := callrules.WorkflowRepeatRegions(training)
	require.Len(t, repeats, 1)
	require.Equal(t, 3, repeats["rounds"].Count)
	exit, ok := callrules.WorkflowNodeByID(training, repeats["rounds"].ExitID)
	require.True(t, ok)
	require.Equal(t, "acknowledgment", exit.Type)
	simulation = callrules.SimulateWorkflow(training)
	require.EqualValues(t, 795, simulation.MaximumSeconds)
	require.Equal(t, 13, simulation.MaximumVisits)
}
