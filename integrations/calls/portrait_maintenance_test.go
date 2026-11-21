package calls

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
)

func TestPortraitMaintenanceCursorRefusesIncompleteAndInvalidPages(t *testing.T) {
	for _, test := range []struct {
		name   string
		result any
	}{
		{"nil", (*memql.ExecuteResult)(nil)},
		{"metadata missing", &memql.ExecuteResult{}},
		{"missing", &memql.ExecuteResult{Meta: &memql.ResultMeta{HasMore: true}}},
		{"repeat", &memql.ExecuteResult{Meta: &memql.ResultMeta{HasMore: true, Cursor: "current"}}},
		{"oversized", memql.NewResultWithOutput([]map[string]any{{"id": "one"}, {"id": "two"}, {"id": "three"}})},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := NewStore(invitationProbeEngine(func(context.Context, string) (any, error) { return test.result, nil }), nil)
			rows, next, e := store.portraitMaintenanceCandidates(context.Background(), "current")
			require.ErrorIs(t, e, errPortraitCursor)
			require.Nil(t, rows)
			require.Empty(t, next)
		})
	}
}
func TestPortraitMaintenanceCursorResetsInvalidStateButPreservesTransientRetry(t *testing.T) {
	for _, cause := range []error{memql.ErrCursorMalformed, memql.ErrCursorSortMismatch, errPortraitCursor, fmt.Errorf("temporary store failure")} {
		t.Run(cause.Error(), func(t *testing.T) {
			engine := invitationProbeEngine(func(ctx context.Context, _ string) (any, error) {
				require.Equal(t, "old", memql.CursorFromContext(ctx))
				return nil, cause
			})
			svc := &Service{store: NewStore(engine, nil), portraitStorage: &portraitLifecycleFake{}, portraitMaintenance: &portraitMaintenanceState{cursor: "old", recent: []string{"old"}}}
			require.Error(t, svc.ReconcilePortraits(context.Background()))
			if cause == memql.ErrCursorMalformed || cause == memql.ErrCursorSortMismatch || cause == errPortraitCursor {
				require.Empty(t, svc.portraitMaintenance.cursor)
				require.Empty(t, svc.portraitMaintenance.recent)
			} else {
				require.Equal(t, "old", svc.portraitMaintenance.cursor)
			}
		})
	}
}
func TestPortraitMaintenanceCursorDetectsShortCyclesAndExhaustion(t *testing.T) {
	next := "A"
	engine := invitationProbeEngine(func(context.Context, string) (any, error) {
		out := memql.NewResultWithOutput([]map[string]any{})
		out.Meta = &memql.ResultMeta{Cursor: next, HasMore: next != ""}
		return out, nil
	})
	svc := &Service{store: NewStore(engine, nil), portraitStorage: &portraitLifecycleFake{}, portraitMaintenance: &portraitMaintenanceState{}}
	require.NoError(t, svc.ReconcilePortraits(context.Background()))
	next = "B"
	require.NoError(t, svc.ReconcilePortraits(context.Background()))
	next = "A"
	require.ErrorIs(t, svc.ReconcilePortraits(context.Background()), errPortraitCursor)
	require.Empty(t, svc.portraitMaintenance.cursor)
	next = "C"
	require.NoError(t, svc.ReconcilePortraits(context.Background()))
	next = ""
	require.NoError(t, svc.ReconcilePortraits(context.Background()))
	require.Empty(t, svc.portraitMaintenance.cursor)
	require.Empty(t, svc.portraitMaintenance.recent)
}

func TestPortraitInventoriesRefuseHiddenRowsAndClearInheritedCursor(t *testing.T) {
	for _, meta := range []*memql.ResultMeta{nil, {HasMore: true}, {HasMore: true, Cursor: "more"}, {Cursor: "more"}} {
		store := NewStore(invitationProbeEngine(func(ctx context.Context, _ string) (any, error) {
			require.Empty(t, memql.CursorFromContext(ctx))
			out := memql.NewResultWithOutput([]map[string]any{})
			out.Meta = meta
			return out, nil
		}), nil)
		ctx := memql.ContextWithCursor(context.Background(), "skip-first-page")
		_, e := store.delegations(ctx, "owner", "site", "key")
		require.ErrorContains(t, e, "incomplete")
		_, e = store.portraits(ctx, "owner", "site", "key")
		require.ErrorContains(t, e, "incomplete")
		svc := &Service{store: store}
		_, e = svc.portraitReferenced(ctx, "owner", &portraitRow{SiteID: "site", KeyID: "key"}, nil)
		require.ErrorContains(t, e, "incomplete")
	}
}

func TestPortraitInventoriesRejectMalformedPayloadsAndDuplicateIDs(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload any
	}{
		{"scalar", "not an inventory"},
		{"object", map[string]any{"id": "one"}},
		{"mixed", []any{map[string]any{"id": "one"}, "discarded element"}},
		{"missing ID", []map[string]any{{"digest": "not identity"}}},
		{"duplicate", []map[string]any{{"id": "same"}, {"id": "same"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := NewStore(invitationProbeEngine(func(context.Context, string) (any, error) { return memql.NewResultWithOutput(test.payload), nil }), nil)
			rows, e := store.readCompleteOwnerPage(context.Background(), "owner", "portrait references", "safeNamedQuery()", nil)
			require.ErrorContains(t, e, "incomplete")
			require.Nil(t, rows)
			rows, next, e := store.portraitMaintenanceCandidates(context.Background(), "")
			require.ErrorIs(t, e, errPortraitCursor)
			require.Nil(t, rows)
			require.Empty(t, next)
		})
	}
}
func TestPortraitInventoriesAcceptExplicitEmptyLists(t *testing.T) {
	for _, payload := range []any{[]any{}, []map[string]any{}} {
		store := NewStore(invitationProbeEngine(func(context.Context, string) (any, error) { return memql.NewResultWithOutput(payload), nil }), nil)
		rows, e := store.readCompleteOwnerPage(context.Background(), "owner", "portrait references", "safeNamedQuery()", nil)
		require.NoError(t, e)
		require.Empty(t, rows)
		rows, next, e := store.portraitMaintenanceCandidates(context.Background(), "")
		require.NoError(t, e)
		require.Empty(t, rows)
		require.Empty(t, next)
	}
}
