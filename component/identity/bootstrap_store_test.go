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
	case strings.Contains(q, "activeUsers("):
		if f.ownerErr != nil {
			return nil, f.ownerErr
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: f.ownerNodes}}, nil
	case strings.Contains(q, "clusterSettingsCurrent("):
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
			"bootstrapEmail": structpb.NewStringValue("owner@example.com"),
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

// -----------------------------------------------------------------------
// HasClaimedOwner: a named owner is not a claimed cluster (memql#3591)
// -----------------------------------------------------------------------
//
// The install writes the owner user row when it bootstraps from env, so the
// cluster has a named owner a passkey-enrolment link can be minted for. That
// broke the old reading of "an owner row exists" as proof somebody had signed in
// -- which the auto-bootstrap guard used to stamp bootstrappedAt. Stamping a
// cluster claimed before anybody claimed it would take /setup away as a fallback,
// silently.
//
// The predicate is credentials: an owner holding an active magic-link or passkey
// identity has authenticated by one of the two routes there are.

// claimEngine answers the owner listing and the per-owner sign-in-identity read.
type claimEngine struct {
	ownerNodes []*memqlv1.MemoryNode
	ownerErr   error

	credsFor  map[string][]*memqlv1.MemoryNode
	credsErr  error
	credQuery []string
}

func (f *claimEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	switch {
	case strings.Contains(q, "signInIdentitiesForUser("):
		f.credQuery = append(f.credQuery, q)
		if f.credsErr != nil {
			return nil, f.credsErr
		}
		for userId, nodes := range f.credsFor {
			if strings.Contains(q, userId) {
				return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: nodes}}, nil
			}
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	case strings.Contains(q, "activeUsers("):
		if f.ownerErr != nil {
			return nil, f.ownerErr
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: f.ownerNodes}}, nil
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

func magicLinkIdentityNode() *memqlv1.MemoryNode {
	return &memqlv1.MemoryNode{
		Id: "v1:identity:identity:ml",
		Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
			"identityType": structpb.NewStringValue("magic_link"),
			"active":       structpb.NewBoolValue(true),
		}},
	}
}

func TestStore_HasClaimedOwner(t *testing.T) {
	t.Run("an owner the bootstrap merely NAMED is not a claim", func(t *testing.T) {
		engine := &claimEngine{ownerNodes: []*memqlv1.MemoryNode{ownerUserNode()}}
		store := &Store{Engine: engine}
		claimed, err := store.HasClaimedOwner(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if claimed {
			t.Error("reported claimed for an owner with no credential at all. That stamps " +
				"bootstrappedAt on the next boot and takes /setup away before anybody has signed in.")
		}
		if len(engine.credQuery) == 0 {
			t.Error("no credential read happened, so this answered from the row's existence -- " +
				"which is the thing that stopped being proof")
		}
	})

	t.Run("an owner holding a sign-in credential IS a claim", func(t *testing.T) {
		engine := &claimEngine{
			ownerNodes: []*memqlv1.MemoryNode{ownerUserNode()},
			credsFor:   map[string][]*memqlv1.MemoryNode{"v1:identity:user:owner": {magicLinkIdentityNode()}},
		}
		store := &Store{Engine: engine}
		claimed, err := store.HasClaimedOwner(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !claimed {
			t.Error("an owner who has authenticated must read as claimed -- this is memql#1864's " +
				"self-heal, and losing it means the claim email re-sends on every deploy")
		}
	})

	t.Run("no owner at all is not a claim", func(t *testing.T) {
		store := &Store{Engine: &claimEngine{}}
		claimed, err := store.HasClaimedOwner(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if claimed {
			t.Error("reported claimed with no owner row")
		}
	})

	t.Run("the owner read failing surfaces rather than reading as unclaimed", func(t *testing.T) {
		store := &Store{Engine: &claimEngine{ownerErr: errors.New("boom: 53300")}}
		if _, err := store.HasClaimedOwner(context.Background()); err == nil {
			t.Error("a DB failure was swallowed into 'not claimed'; the caller must be able to " +
				"fail-safe rather than re-spam the owner")
		}
	})

	t.Run("the credential read failing surfaces too", func(t *testing.T) {
		engine := &claimEngine{
			ownerNodes: []*memqlv1.MemoryNode{ownerUserNode()},
			credsErr:   errors.New("boom: 53300"),
		}
		store := &Store{Engine: engine}
		if _, err := store.HasClaimedOwner(context.Background()); err == nil {
			t.Error("a failed credential read reported 'not claimed', which is the answer that " +
				"sends an email")
		}
	})
}
