package library

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

type libraryRoleValidator func(context.Context, string, string, auth.Role) error

func (f libraryRoleValidator) ValidateRoleTest(ctx context.Context, session, subject string, role auth.Role) error {
	return f(ctx, session, subject, role)
}

func TestRoleTestDocumentManagementAcrossReplicas(t *testing.T) {
	f := newRevisionDB(t)
	// Initialize registries serially, then overlap independent owner fixtures.
	// Each persona keeps its own engines, model answers and document/run IDs.
	t.Parallel()
	for _, tc := range []struct {
		name string
		run  func(*testing.T, *revisionDB)
	}{
		{"history", testHistoryBranchesAcrossReplicas},
		{"feedback", testDeleteDocumentAnnotationsAcrossReplicas},
		{"approval", testDocumentRevisionAnalyzesThenApprovesAndAppliesAcrossReplicas},
	} {
		t.Run(tc.name, func(t *testing.T) {
			personal := f.isolatedCase(t)
			personal.owner = "role-test-" + personal.owner
			revoked := false
			auth.SetRoleTestValidator(libraryRoleValidator(func(_ context.Context, session, subject string, role auth.Role) error {
				if revoked || session != "library-session" || strings.TrimPrefix(subject, "v1:identity:user:") != personal.owner || role != auth.RoleReader {
					return errors.New("test ended or authority changed")
				}
				return nil
			}))
			t.Cleanup(func() { auth.SetRoleTestValidator(nil) })
			access := &auth.AccessContext{UserId: personal.owner, Role: auth.RoleReader, RoleTestSessionID: "library-session"}
			a, err := auth.ForwardedAuthorityForUser(access, auth.RoleTestClass, "", time.Now().Add(time.Hour), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			personal.ctx = auth.BindForwardedContext(context.Background(), a.Principal().Claims, access, a)
			tc.run(t, personal)
			artifact, doc := personal.document("# Before end\n")
			revoked = true
			if _, err := personal.second.handleEditDocument(personal.ctx, map[string]any{"documentId": doc.source, "content": "after end", "expectedVersion": doc.version, "expectedRevision": doc.revision}, 0); err == nil {
				t.Fatal("ended persona edited document")
			}
			if _, err := personal.second.handleDocumentReview(personal.ctx, map[string]any{"artifactId": artifact}, 0); err == nil {
				t.Fatal("ended persona read review")
			}
		})
	}
}
