package memql

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

func TestPersonalMutationGateCoversOnlyOwnedAppDoors(t *testing.T) {
	eng := gateTestEngine(t)
	for _, role := range []auth.Role{auth.RoleReader, auth.RoleWriter, auth.RoleAdmin, auth.RoleDeveloper, auth.RoleOwner} {
		ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "personal", Role: role})
		for _, q := range []string{
			`mutation createAskConversation(requestId: "new", title: "New conversation")`,
			`mutation library.createLibraryFolder(folderId: "mine", name: "Notes")`,
			`mutation renameLibraryFolder(folderId: "mine", name: "Renamed")`,
			`mutation moveArtifactToFolder(artifactId: "mine", folderId: "notes")`,
			`mutation archiveLibraryFile(fileId: "mine")`,
			`mutation restoreArtifact(artifactId: "mine")`,
		} {
			require.True(t, eng.PersonalDataPlaneEnforced(ctx, q), "%s: %s", role, q)
		}
		for _, q := range []string{
			`insert("v1:library:folder", id="mine", payload={"name":"Raw"})`,
			`mutation createAgent(agentId: "new", name: "Agent")`,
			`mutation createAudience(audienceId: "new", name: "Audience")`,
			`mutation saveAskConversation(id: "mine", title: "Forged", transcript: {})`,
			`mutation createLibraryFile(fileId: "mine", name: "Forged", blobUrl: "foreign")`,
			`createLibraryFolder(folderId: "mine", name: "Notes") && createAgent(agentId: "new", name: "Agent")`,
		} {
			require.False(t, eng.PersonalDataPlaneEnforced(ctx, q), "%s: %s", role, q)
		}
	}
	for _, access := range []*auth.AccessContext{
		{}, {UserId: "personal", Role: auth.RoleReader, Synthetic: true},
		{UserId: "personal", Role: auth.RoleReader, Unranked: true}, {Role: auth.RoleReader},
	} {
		ctx := auth.ContextWithAccess(context.Background(), access)
		require.False(t, eng.PersonalAppCapable(ctx, "files"), "%+v", access)
	}
	for _, role := range []auth.Role{"", "unknown"} {
		s, cs := gateTestSession(t, eng, role)
		driveGateQuery(t, s, "unknown-personal", `mutation createAskConversation(requestId: "new", title: "New")`)
		require.NotNil(t, queryErrorFor(cs, "unknown-personal"))
	}
}

// Execute through the real browser handler on two independent engines. A
// classifier-only test cannot prove that ownership survives the replica hop.
func TestViewerPersonalWritesThroughExecuteQueryAcrossReplicas(t *testing.T) {
	db := openWireTestDB(t)
	_, err := memqlengine.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	open := func() *memqlengine.MemQLEngine {
		e, err := memqlengine.New(db)
		require.NoError(t, err)
		e.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		require.NoError(t, e.Init(concept.DefaultRegistry()))
		return e
	}
	a, b := open(), open()
	user := fmt.Sprintf("viewer-wire-%d", time.Now().UnixNano())
	session := func(e *memqlengine.MemQLEngine, who string) (*streamSession, *captureStream) {
		s, cs := gateTestSession(t, e, auth.RoleReader)
		s.access.UserId = who
		cs.ctx = auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: who})
		return s, cs
	}
	sa, ca := session(a, user)
	sb, cb := session(b, user)
	folder := user + "-folder"
	q := parser.QuoteString
	driveGateQuery(t, sa, "folder-create", fmt.Sprintf(`mutation createLibraryFolder(folderId: %s, name: "Notes")`, q(folder)))
	waitForQueryResult(t, ca, "folder-create", 15*time.Second)
	for n, mutation := range []string{
		fmt.Sprintf(`renameLibraryFolder(folderId: %s, name: "Renamed")`, q(folder)),
		fmt.Sprintf(`archiveLibraryFolder(folderId: %s)`, q(folder)),
		fmt.Sprintf(`restoreLibraryFolder(folderId: %s)`, q(folder)),
		`createAskConversation(requestId: "new", title: "New conversation")`,
	} {
		request := fmt.Sprintf("personal-%d", n)
		driveGateQuery(t, sb, request, "mutation "+mutation)
		waitForQueryResult(t, cb, request, 15*time.Second)
	}
	outsider, co := session(b, user+"-other")
	driveGateQuery(t, outsider, "foreign-folder", fmt.Sprintf(`mutation renameLibraryFolder(folderId: %s, name: "Stolen")`, q(folder)))
	_, refused := awaitAccountOutcome(t, co, "foreign-folder", 15*time.Second)
	require.NotNil(t, refused, "personal permission changed someone else's folder")
	for n, query := range []string{
		`mutation createAudience(audienceId: "forbidden", name: "Forbidden")`,
		`mutation createCampaign(campaignId: "forbidden", name: "Forbidden", audienceId: "none", templateId: "none")`,
		`mutation createAgent(agentId: "forbidden", name: "Forbidden")`,
		fmt.Sprintf(`insert("v1:library:folder", id=%s, payload={"name":"Raw"})`, q(folder)),
		`mutation saveAskConversation(id: "forged", title: "Forged", transcript: {}, versionTime: "2026-10-15T00:00:00Z")`,
	} {
		request := fmt.Sprintf("forbidden-%d", n)
		driveGateQuery(t, sb, request, query)
		_, refused := awaitAccountOutcome(t, cb, request, 15*time.Second)
		require.NotNil(t, refused, query)
		require.Regexp(t, `(?i)permission|organization_|server.only`, refused.GetMessage(), query)
	}
}
