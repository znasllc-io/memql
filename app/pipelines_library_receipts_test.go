package app

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

func TestPipelineReceiptReadRefusesBeforeDatabaseAccess(t *testing.T) {
	s := &pipelinesLibraryStore{uploadDB: func() *sql.DB { t.Fatal("invalid read reached the database"); return nil }}
	scope := pipelinesteps.RunFileReceiptScope{OwnerUserID: "owner", WorkRunID: "run", StepKey: "build/archive", Attempt: 1}
	owner := auth.ContextWithUserActor(context.Background(), scope.OwnerUserID)
	trusted := auth.ContextWithInternalOrigin(owner)
	valid := strings.Repeat("a", 64)
	for _, ctx := range []context.Context{context.Background(), owner,
		auth.ContextWithInternalOrigin(context.Background()),
		auth.ContextWithClientOrigin(trusted),
		auth.ContextWithInternalOrigin(auth.ContextWithUserActor(context.Background(), "another-owner")),
	} {
		if _, err := s.ReadRunFileReceipts(ctx, scope, []string{valid}); err == nil {
			t.Fatal("untrusted/mismatched actor read accepted")
		}
		if _, _, err := s.OpenRunFileReceipt(ctx, scope, valid); err == nil {
			t.Fatal("untrusted actor opened artifact stream")
		}
	}
	for _, ids := range [][]string{{"short"}, {strings.Repeat("A", 64)}, {strings.Repeat("z", 64)}, {valid, valid}, make([]string, 1025), {strings.Repeat("a", 1<<20)}} {
		if _, err := s.ReadRunFileReceipts(trusted, scope, ids); err == nil {
			t.Fatal("invalid/beyond-bound IDs accepted")
		}
	}
	scope.Attempt = 0
	if _, err := s.ReadRunFileReceipts(trusted, scope, []string{valid}); err == nil {
		t.Fatal("zero attempt accepted")
	}
}
