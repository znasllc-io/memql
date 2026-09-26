package memql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPackageDeploymentModePersistsAtCreationAndUpdate(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("package-mode")
	installSiteOrganizationCapabilities(t)
	accountID := "mode-account-" + suffix
	caller := siteOrganizationMemberCtx(t, eng, "mode-owner-"+suffix, accountID)
	stranger := siteOrganizationMemberCtx(t, eng, "mode-other-"+suffix, "mode-other-account-"+suffix)
	for _, automatic := range []bool{false, true} {
		id := "mode-manual-" + suffix
		if automatic {
			id = "mode-automatic-" + suffix
		}
		args := map[string]any{"packageId": id, "accountId": accountID, "name": id, "sourceKind": "repo", "repoUrl": "https://github.com/example/" + id, "repoRef": "main", "autoDeploy": automatic}
		_, err := runSiteMutation(t, caller, eng, "createPackage", args)
		require.NoError(t, err)
		p := latestPayload(t, caller, db, "v1:platform:package", "v1:platform:package:"+id)
		require.Equal(t, automatic, p["autoDeploy"])
		require.Equal(t, accountID, BareShortId(stringFromAny(p["accountId"])))
		_, err = runSiteMutation(t, caller, eng, "setPackageAutoDeploy", map[string]any{"packageId": id, "autoDeploy": !automatic})
		require.NoError(t, err)
		p = latestPayload(t, caller, db, "v1:platform:package", "v1:platform:package:"+id)
		require.Equal(t, !automatic, p["autoDeploy"])
		require.NotEmpty(t, p["autoDeployChangedAt"])
		_, err = runSiteMutation(t, stranger, eng, "setPackageAutoDeploy", map[string]any{"packageId": id, "autoDeploy": automatic})
		require.Error(t, err, "a member of another organization cannot change this policy")
	}
}
