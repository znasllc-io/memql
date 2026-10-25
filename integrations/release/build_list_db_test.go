package release

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

func emptyBuildHistoryReader(t *testing.T, db *sql.DB, engine memql.IntegrationEngineAccess) (*Integration, context.Context) {
	t.Helper()
	owner := id.NewShortId()
	ctx := auth.ContextWithAccess(t.Context(), &auth.AccessContext{UserId: owner, Role: auth.RoleOwner})
	ctx = auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: owner})
	i := NewIntegration(nil, engine, resolver{
		systemVariable: func(context.Context, string) (string, error) {
			t.Fatal("empty release history required live configuration")
			return "", nil
		},
	})
	i.candidateDB = func() *sql.DB { return db }
	return i, ctx
}

func assertEmptyBuildHistory(t *testing.T, value any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Intents []releaseBuildView `json:"intents"`
	}
	if err := json.Unmarshal(body, &page); err != nil || page.Intents == nil || len(page.Intents) != 0 {
		t.Fatalf("empty release history did not return an explicit empty intents array: %s (%v)", body, err)
	}
}

// Exercise the real loaded DSL, named argument conversion, integration handler
// and owner-scoped PostgreSQL journal. A transport fixture returning [] cannot
// detect an object decoder incorrectly applied to this scalar argument.
func TestReleaseBuildListExecutesEmptyHistoryThroughBuiltin(t *testing.T) {
	db := candidateTestDB(t)
	engine := assemblyEngine(t, db)
	i, ctx := emptyBuildHistoryReader(t, db, engine)
	if err := engine.RegisterIntegration(i); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, call string }{
		{"omitted_default", "builtin releaseBuilds()"},
		{"explicit_50", "builtin releaseBuilds(limit: 50)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := engine.Execute(ctx, tc.call)
			if err != nil {
				t.Fatal(err)
			}
			rows := memql.MaterializeRows(result.OutputPayload())
			if len(rows) != 1 {
				t.Fatalf("expected one release history envelope, got %d", len(rows))
			}
			assertEmptyBuildHistory(t, rows[0])
		})
	}
}

func TestReleaseBuildListAcceptsWireIntegers(t *testing.T) {
	i, ctx := emptyBuildHistoryReader(t, candidateTestDB(t), nil)
	var wire map[string]any
	if err := json.Unmarshal([]byte(`{"limit":50}`), &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["limit"].(float64); !ok {
		t.Fatal("fixture did not exercise the runtime JSON numeric representation")
	}
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"runtime_float64", wire},
		{"native_integer", map[string]any{"limit": 50}},
		{"json_number", map[string]any{"limit": json.Number("50")}},
		{"lower_bound", map[string]any{"limit": 1}},
		{"upper_bound", map[string]any{"limit": 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := i.handleBuildList(ctx, tc.args, 0)
			if err != nil || len(rows) != 1 {
				t.Fatalf("valid scalar list request failed: rows=%d err=%v", len(rows), err)
			}
			assertEmptyBuildHistory(t, json.RawMessage(rows[0].Payload))
		})
	}
}

func TestReleaseBuildListRefusesInvalidLimitsAndNonOwnersBeforeDatabase(t *testing.T) {
	i := NewIntegration(nil, nil, resolver{})
	i.candidateDB = func() *sql.DB {
		t.Fatal("invalid release history request reached the database")
		return nil
	}
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"fractional", 50.5}, {"string", "50"}, {"boolean", true}, {"null", nil},
		{"object", map[string]any{"limit": 50}}, {"array", []any{50}},
		{"zero", 0}, {"negative", -1}, {"over_limit", 101},
		{"overflow", 1e40}, {"nonfinite", math.NaN()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rows, err := i.handleBuildList(ownerCtx(), map[string]any{"limit": tc.value}, 0); err == nil || len(rows) != 0 {
				t.Fatalf("invalid limit was accepted: rows=%d err=%v", len(rows), err)
			}
		})
	}
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"anonymous", context.Background()}, {"developer", actorContext(auth.RoleDeveloper)}, {"admin", actorContext(auth.RoleAdmin)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rows, err := i.handleBuildList(tc.ctx, map[string]any{"limit": float64(50)}, 0); err == nil || len(rows) != 0 {
				t.Fatalf("non-owner read history: rows=%d err=%v", len(rows), err)
			}
		})
	}
	t.Run("object_decoder_remains_strict", func(t *testing.T) {
		var limit int
		if err := profileDecode(50, &limit); err == nil || !strings.Contains(err.Error(), "must be an object") {
			t.Fatalf("generic profile object validation was weakened: %v", err)
		}
	})
}
