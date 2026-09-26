package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

func TestNavigationMatchesLabelsWithoutGuessingAcrossAmbiguity(t *testing.T) {
	rows := []map[string]any{{"id": "vscode", "title": "MemQL for Visual Studio Code and Cursor"}, {"id": "os", "title": "MemQL OS"}}
	for _, search := range []string{"VS Code and Cursor", "MQL Visual Studio Code Cursor", "vs code", "vscode"} {
		matches := matchNavigationRecords(rows, []string{"title"}, search)
		require.Len(t, matches, 1, search)
		require.Equal(t, "vscode", matches[0].ID)
	}
	require.Empty(t, matchNavigationRecords(rows, []string{"title"}, "birds"))
	rows = append(rows, map[string]any{"id": "other", "title": "Another Visual Studio Code and Cursor"})
	require.Len(t, matchNavigationRecords(rows, []string{"title"}, "VS Code and Cursor"), 2)
}
func TestNavigationCatalogContainsOnlyRegisteredSections(t *testing.T) {
	apps := navigationApps()
	require.Greater(t, len(apps), 10)
	for _, app := range apps {
		for _, record := range app.Records {
			found := false
			for _, section := range app.Sections {
				found = found || section.ID == record.Section
			}
			require.True(t, found, app.ID)
		}
	}
}

func TestNavigationResolvesBuiltInSiteForOwnerWithoutSharingItWithAnotherRole(t *testing.T) {
	e, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	site := "nav-" + id.NewShortId()
	_, err := createSiteRaw(t, systemSiteCtx(), e, map[string]any{"siteId": site, "hostname": site + "." + siteTestDomain, "bundleRef": "blob://navigation/test", "title": "MemQL for Visual Studio Code and Cursor", "systemOwned": true})
	require.NoError(t, err)
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleWriter} {
		owner := "v1:identity:user:" + id.NewShortId()
		ctx := common.ContextWithRun(auth.ContextWithAccess(auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: owner}), &auth.AccessContext{UserId: owner, Role: role}), common.RunContext{RunId: "v1:work:run:" + id.NewShortId(), OwnerUserId: owner})
		nodes, err := e.workNavigateBuiltin(ctx, map[string]any{"app": "Deployables", "section": "Deployables", "record": site}, 0)
		require.NoError(t, err)
		var result map[string]any
		require.NoError(t, json.Unmarshal(nodes[0].Payload, &result))
		require.Equal(t, role == auth.RoleOwner, result["requested"], fmt.Sprint(result))
		if role == auth.RoleOwner {
			require.Equal(t, site, result["arguments"].(map[string]any)["siteId"])
			run, _ := common.RunFromContext(ctx)
			evidence, err := e.workRows(ctx, "workObservationsForOwnerRun", run.RunId)
			require.NoError(t, err)
			require.Contains(t, fmt.Sprint(evidence), site)
		} else {
			require.NotContains(t, fmt.Sprint(result), "Visual Studio")
		}
	}
}
