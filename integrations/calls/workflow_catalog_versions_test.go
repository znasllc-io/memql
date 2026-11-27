package calls

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

// Published versions are pinned by approvals/bookings. New built-in behavior
// must not silently rewrite any of the fourteen definitions already released.
func TestWorkflowCatalogPreservesPublishedV1Definitions(t *testing.T) {
	want := map[string]string{
		"working-session":     "452919d921da55062f4668d077c138a1a725ec111b77a4877b1f57293e09d26f",
		"one-on-one":          "c9b35225a8b23db882aef3897f325330e6b0d70c4412f4b79e522cabed342a3d",
		"interview":           "7cdaa878b06ab0f39f14af6f24bd4be040742c1ee8daed9f47763ffd89cf10a8",
		"presentation":        "8ea2884d04ef52a68e94a46eaf7e6d87e0693a000a814c08ff33474b996b8646",
		"panel":               "d3bbea81cd7d651c248df9cadbda8fa538af2b3bc3d820ee4ed1f825a272ecc8",
		"standup":             "ce5a55deb507a11b31ad87c4907245a74dc67500239893e689ed892cbdbcdc95",
		"retrospective":       "10ecfafbe30667fadd0ade9940ac5f80e100857a38162eff498d97c0761e8992",
		"planning":            "348475ea8e538640da2dce2fa711738dd4d30c2523e51ce0fdf81a029a963088",
		"pair-programming":    "ba943b73b0a7afaf6f065f4fd4aa1d0ee89e5f17df9724223ac2fa27addb3877",
		"study-group":         "4692ed8b2b87cb18e633ce9a3e87f850ea91e971ec016e89e757ce81360dd48b",
		"tutoring":            "18c7dfb0c33a75a5879c68f7232825c7176280ec784b400adfdd80a2f956f600",
		"supervised-learning": "5da7bc82b680750dd164da17986f0534946b718aacf4d0ba968cfa85acba8b77",
		"office-hours":        "92eb5a39ba67ee75c2e05e4c98ea117de9b0ed998aab8f46390c7b9fd19b2db0",
		"demonstration":       "644bfc5508001050ff68e4bb83a507062745b7cd1dc8364d2abb23f660441080",
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
		digest := sha256.Sum256(canonical)
		require.Equal(t, want[id], hex.EncodeToString(digest[:]), id+" v1 changed")
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
