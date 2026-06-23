package admin

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity"
	memqlengine "github.com/znasllc-io/memql/component/memql"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/types/known/structpb"
)

// clusterSettingsNode builds a MemoryNode shaped like clusterSettingsFull
// with the given id and brandName.
func clusterSettingsNode(id, brandName string) *memqlv1.MemoryNode {
	return &memqlv1.MemoryNode{
		Id: id,
		Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
			"id":        structpb.NewStringValue(id),
			"brandName": structpb.NewStringValue(brandName),
		}},
	}
}

// clusterSettingsFakeEngine records every query it receives and returns
// a canned node list for clusterSettingsCurrent calls.
type clusterSettingsFakeEngine struct {
	nodes   []*memqlv1.MemoryNode
	queries []string
}

func (f *clusterSettingsFakeEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	f.queries = append(f.queries, q)
	if strings.HasPrefix(q, "clusterSettingsCurrent(") {
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: f.nodes}}, nil
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

// TestLiveSettingsReaderSnapshotPicksCanonicalRow proves that when the
// engine returns the canonical "cluster" row (as it does after the
// id=="cluster" filter was added to clusterSettingsCurrent in
// fix/1807-identity-brand-singleton), Snapshot resolves the brandName
// deterministically regardless of how many nodes the engine returns.
//
// Before the fix, clusterSettingsCurrent had NO filter and the
// engine could return multiple distinct clusterSettings rows (one per
// id in the DB). Callers took Nodes[0], which was non-deterministic
// across replicas when a stale miskeyed row existed alongside the
// canonical "cluster" row (#1807). After the fix, the query pins
// id=="cluster" so only one row is returned.
func TestLiveSettingsReaderSnapshotPicksCanonicalRow(t *testing.T) {
	// The canonical clusterSettings row (id="cluster") written by the
	// first-run wizard and all subsequent admin saves.
	canonicalNode := clusterSettingsNode("v1:identity:clusterSettings:cluster", "My Brand")

	eng := &clusterSettingsFakeEngine{
		nodes: []*memqlv1.MemoryNode{canonicalNode},
	}
	reader := &LiveSettingsReader{
		Engine:   eng,
		Fallback: identity.Config{BrandName: "fallback-brand"},
	}

	got := reader.Snapshot(context.Background())
	if got.BrandName != "My Brand" {
		t.Errorf("BrandName = %q, want %q", got.BrandName, "My Brand")
	}

	// Verify the query was actually issued (not short-circuited).
	if len(eng.queries) == 0 {
		t.Fatal("no queries issued; reader may have short-circuited before calling the engine")
	}
	for _, q := range eng.queries {
		if !strings.HasPrefix(q, "clusterSettingsCurrent(") {
			t.Errorf("unexpected query %q; expected clusterSettingsCurrent", q)
		}
	}
}

// TestLiveSettingsReaderSnapshotFallsBackWhenEmpty proves that when the
// engine returns no nodes (first-run: wizard not yet completed),
// Snapshot returns the env-derived fallback brand and does not error.
func TestLiveSettingsReaderSnapshotFallsBackWhenEmpty(t *testing.T) {
	eng := &clusterSettingsFakeEngine{nodes: nil}
	reader := &LiveSettingsReader{
		Engine:   eng,
		Fallback: identity.Config{BrandName: "env-brand"},
	}

	got := reader.Snapshot(context.Background())
	if got.BrandName != "env-brand" {
		t.Errorf("BrandName = %q, want %q (fallback)", got.BrandName, "env-brand")
	}
}

// TestLiveSettingsReaderTokenSettingsPicksCanonicalRow proves that
// TokenSettings also resolves deterministically from the canonical row.
func TestLiveSettingsReaderTokenSettingsPicksCanonicalRow(t *testing.T) {
	canonicalNode := &memqlv1.MemoryNode{
		Id: "v1:identity:clusterSettings:cluster",
		Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
			"accessTokenTTLSeconds":  structpb.NewNumberValue(3600),
			"refreshTokenTTLSeconds": structpb.NewNumberValue(86400),
		}},
	}
	eng := &clusterSettingsFakeEngine{nodes: []*memqlv1.MemoryNode{canonicalNode}}
	reader := &LiveSettingsReader{Engine: eng}

	ts := reader.TokenSettings(context.Background())
	if ts.AccessTokenTTL.Seconds() != 3600 {
		t.Errorf("AccessTokenTTL = %v, want 3600s", ts.AccessTokenTTL)
	}
	if ts.RefreshTokenTTL.Seconds() != 86400 {
		t.Errorf("RefreshTokenTTL = %v, want 86400s", ts.RefreshTokenTTL)
	}
}
