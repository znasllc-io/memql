package identity

import (
	"context"
	"errors"
	"strings"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// bootstrapFakeEngine routes by query prefix: owner-role user listing,
// clusterSettings read. Any *Err triggers a DB-style failure so the
// error-returning store helpers can be exercised.
type bootstrapFakeEngine struct {
	ownerNodes []*memqlv1.MemoryNode
	ownerErr   error

	settingsNodes []*memqlv1.MemoryNode
	settingsErr   error
}

func (f *bootstrapFakeEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	switch {
	case strings.HasPrefix(q, "queryActiveUsers("):
		if f.ownerErr != nil {
			return nil, f.ownerErr
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: f.ownerNodes}}, nil
	case strings.HasPrefix(q, "queryClusterSettingsCurrent("):
		if f.settingsErr != nil {
			return nil, f.settingsErr
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: f.settingsNodes}}, nil
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

func ownerUserNode() *memqlv1.MemoryNode {
	return &memqlv1.MemoryNode{
		Id: "v1:identity:user:owner",
		Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
			"role":   structpb.NewStringValue("owner"),
			"active": structpb.NewBoolValue(true),
		}},
	}
}

func clusterSettingsNode(bootstrappedAt string) *memqlv1.MemoryNode {
	return &memqlv1.MemoryNode{
		Id: "v1:identity:clusterSettings:cluster",
		Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
			"bootstrappedAt": structpb.NewStringValue(bootstrappedAt),
			"bootstrapEmail": structpb.NewStringValue("znas@znas.io"),
		}},
	}
}

func TestStore_HasOwnerUser(t *testing.T) {
	t.Run("owner present", func(t *testing.T) {
		store := &Store{Engine: &bootstrapFakeEngine{ownerNodes: []*memqlv1.MemoryNode{ownerUserNode()}}}
		has, err := store.HasOwnerUser(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !has {
			t.Fatal("expected HasOwnerUser=true when an owner row exists")
		}
	})

	t.Run("no owner", func(t *testing.T) {
		store := &Store{Engine: &bootstrapFakeEngine{ownerNodes: nil}}
		has, err := store.HasOwnerUser(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if has {
			t.Fatal("expected HasOwnerUser=false when no owner row exists")
		}
	})

	t.Run("db error surfaces", func(t *testing.T) {
		store := &Store{Engine: &bootstrapFakeEngine{ownerErr: errors.New("53300")}}
		if _, err := store.HasOwnerUser(context.Background()); err == nil {
			t.Fatal("expected HasOwnerUser to surface the DB error (fail-safe), got nil")
		}
	})
}

func TestStore_IsClusterBootstrappedE(t *testing.T) {
	t.Run("stamped -> true", func(t *testing.T) {
		store := &Store{Engine: &bootstrapFakeEngine{
			settingsNodes: []*memqlv1.MemoryNode{clusterSettingsNode("2026-06-20T00:00:00Z")},
		}}
		ok, err := store.IsClusterBootstrappedE(context.Background())
		if err != nil || !ok {
			t.Fatalf("want (true,nil), got (%v,%v)", ok, err)
		}
	})

	t.Run("empty stamp -> false, no error", func(t *testing.T) {
		store := &Store{Engine: &bootstrapFakeEngine{
			settingsNodes: []*memqlv1.MemoryNode{clusterSettingsNode("")},
		}}
		ok, err := store.IsClusterBootstrappedE(context.Background())
		if err != nil || ok {
			t.Fatalf("want (false,nil), got (%v,%v)", ok, err)
		}
	})

	t.Run("db error -> (false, err); bool wrapper fail-closes", func(t *testing.T) {
		store := &Store{Engine: &bootstrapFakeEngine{settingsErr: errors.New("53300")}}
		ok, err := store.IsClusterBootstrappedE(context.Background())
		if err == nil {
			t.Fatal("want non-nil error from IsClusterBootstrappedE on DB failure")
		}
		if ok {
			t.Fatal("want false on error")
		}
		// The bool wrapper must collapse the error to false (fail-closed).
		if store.IsClusterBootstrapped(context.Background()) {
			t.Fatal("IsClusterBootstrapped must fail-closed (false) on DB error")
		}
	})
}
