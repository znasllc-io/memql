package memql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPackageDeploymentModePersistsAtCreationAndUpdate(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("package-mode")
	caller := userSiteCtx("mode-owner-" + suffix)
	for _, automatic := range []bool{false, true} {
		id := "mode-manual-" + suffix
		if automatic {
			id = "mode-automatic-" + suffix
		}
		args := map[string]any{"packageId": id, "name": id, "sourceKind": "repo", "repoUrl": "https://github.com/example/" + id, "repoRef": "main", "autoDeploy": automatic}
		_, err := runSiteMutation(t, caller, eng, "createPackage", args)
		require.NoError(t, err)
		p := latestPayload(t, caller, db, "v1:platform:package", "v1:platform:package:"+id)
		require.Equal(t, automatic, p["autoDeploy"])
		_, err = runSiteMutation(t, caller, eng, "setPackageAutoDeploy", map[string]any{"packageId": id, "autoDeploy": !automatic})
		require.NoError(t, err)
		p = latestPayload(t, caller, db, "v1:platform:package", "v1:platform:package:"+id)
		require.Equal(t, !automatic, p["autoDeploy"])
		require.NotEmpty(t, p["autoDeployChangedAt"])
		_, err = runSiteMutation(t, userSiteCtx("mode-other-"+suffix), eng, "setPackageAutoDeploy", map[string]any{"packageId": id, "autoDeploy": automatic})
		require.Error(t, err, "another source owner cannot change this policy")
	}
}
