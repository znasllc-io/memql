package installation

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

type selectionForbiddenAPI struct{ t *testing.T }

func (a selectionForbiddenAPI) Do(context.Context, string, string, string, []byte) ([]byte, error) {
	a.t.Fatal("rejected selection caller reached the receiving cluster")
	return nil, nil
}

// Every public entry must authenticate the current operator before internal
// origin can reach the closed installation host. In particular, a claimed
// owner in a credential cannot undo the resolved actor's lower role.
func TestSelectionEntrypointsRejectUnidentifiedOrInsufficientActorsBeforeHostIO(t *testing.T) {
	p := &Preparer{host: &preparationHost{
		api: selectionForbiddenAPI{t},
		journal: &preparationJournal{db: func() *sql.DB {
			t.Fatal("rejected selection caller reached the durable journal")
			return nil
		}},
	}}
	selection := PrepareSelection{InstallationID: "installation", RequestID: "request", CandidateID: "sha256:" + strings.Repeat("a", 64), CatalogDigest: "sha256:" + strings.Repeat("b", 64), OverlayRevision: strings.Repeat("c", 40), ExpectedCurrentRevision: strings.Repeat("d", 40)}
	plan := "memql-id:" + strings.Repeat("e", 64)
	entries := map[string]func(context.Context) error{
		"prepare": func(ctx context.Context) error { _, err := p.PrepareSelected(ctx, selection); return err },
		"resume": func(ctx context.Context) error {
			_, err := p.ResumeSelected(ctx, selection.InstallationID, plan)
			return err
		},
		"cancel": func(ctx context.Context) error {
			_, err := p.CancelSelected(ctx, selection.InstallationID, plan)
			return err
		},
		"rollback": func(ctx context.Context) error {
			_, err := p.RollbackSelected(ctx, selection.InstallationID, plan)
			return err
		},
		"start": func(ctx context.Context) error {
			_, err := p.StartSelected(ctx, selection.InstallationID, plan)
			return err
		},
		"observe": func(ctx context.Context) error {
			_, err := p.ObserveSelected(ctx, selection.InstallationID, plan)
			return err
		},
		"status": func(ctx context.Context) error { _, err := p.SelectionStatus(ctx); return err },
		"publication": func(ctx context.Context) error {
			_, err := p.PublicationSource(ctx, selection.InstallationID, selection.CandidateID, selection.CatalogDigest)
			return err
		},
	}
	actors := map[string]*auth.AccessContext{
		"missing":                 nil,
		"missing identity":        {Role: auth.RoleOwner},
		"reader with owner claim": {Role: auth.RoleReader, UserId: "v1:identity:user:operator"},
		"writer":                  {Role: auth.RoleWriter, UserId: "v1:identity:user:operator"},
		"unknown role":            {Role: auth.Role("unrecognized"), UserId: "v1:identity:user:operator"},
		"anonymous owner":         {Role: auth.RoleOwner, UserId: "v1:identity:user:operator", IsAnonymous: true},
		"synthetic owner":         {Role: auth.RoleOwner, UserId: "v1:identity:user:operator", Synthetic: true},
		"role stand-in owner":     {Role: auth.RoleOwner, UserId: "v1:identity:user:operator", RoleStandIn: true},
	}
	for name, actor := range actors {
		t.Run(name, func(t *testing.T) {
			ctx := auth.ContextWithClaims(context.Background(), map[string]any{"role": "owner"})
			if actor != nil {
				ctx = auth.ContextWithAccess(ctx, actor)
			}
			for entry, invoke := range entries {
				t.Run(entry, func(t *testing.T) {
					require.ErrorContains(t, invoke(ctx), "requires an identified")
					require.False(t, auth.OriginFromContext(ctx).IsInternal(), "admission must not mutate the caller's context")
				})
			}
		})
	}
}

func TestSelectionAdmissionPreservesActorCredentialCeilingsAndCancellation(t *testing.T) {
	p := &Preparer{host: &preparationHost{}}
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleAdmin, auth.RoleDeveloper} {
		t.Run(string(role), func(t *testing.T) {
			actor := &auth.AccessContext{Role: role, UserId: "v1:identity:user:operator", IdentityId: "v1:identity:identity:credential"}
			credential := &auth.TokenInfo{Subject: "operator", Scopes: []string{"installation:select"}}
			delegation := &auth.DelegationContext{RoleCeiling: role, Scopes: []string{"installation:select"}, ExpiresAt: time.Now().Add(time.Minute)}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			ctx = auth.ContextWithAccess(ctx, actor)
			ctx = auth.ContextWithToken(ctx, credential)
			ctx = auth.ContextWithDelegation(ctx, delegation)
			admitted, err := p.selectionContext(ctx)
			require.NoError(t, err)
			require.True(t, auth.OriginFromContext(admitted).IsInternal())
			require.False(t, auth.OriginFromContext(ctx).IsInternal())
			actual, ok := auth.AccessFromContext(admitted)
			require.True(t, ok)
			require.Same(t, actor, actual)
			require.Same(t, credential, auth.TokenInfoFromContext(admitted))
			actualDelegation, ok := auth.DelegationFromContext(admitted)
			require.True(t, ok)
			require.Same(t, delegation, actualDelegation)
			before, _ := ctx.Deadline()
			after, _ := admitted.Deadline()
			require.Equal(t, before, after)
			cancel()
			require.ErrorIs(t, admitted.Err(), context.Canceled)
		})
	}
}
