package identity

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

type fleetEnrollmentEngine struct {
	t       *testing.T
	role    string
	active  bool
	missing bool
	err     error
	reads   int
}

func (f *fleetEnrollmentEngine) Execute(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	f.t.Helper()
	f.reads++
	if !memqlengine.FreshReadFromContext(ctx) {
		f.t.Fatal("enrollment used a cached person")
	}
	if !strings.Contains(q, "query userByIdSystem(") {
		f.t.Fatalf("unexpected side effect: %s", q)
	}
	if f.err != nil {
		return nil, f.err
	}
	nodes := []*memqlv1.MemoryNode{}
	if !f.missing {
		nodes = append(nodes, &memqlv1.MemoryNode{Id: "v1:identity:user:alice", Payload: &structpb.Struct{Fields: map[string]*structpb.Value{"role": structpb.NewStringValue(f.role), "active": structpb.NewBoolValue(f.active)}}})
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: nodes}}, nil
}
func TestFleetEnrollmentUsesCurrentPersonAndOwnMachine(t *testing.T) {
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(nil) })
	for _, role := range []string{"owner", "developer", "admin", "writer", "reader", "custom", ""} {
		t.Run(role, func(t *testing.T) {
			f := &fleetEnrollmentEngine{t: t, role: role, active: true}
			s := &Store{Engine: f}
			got, err := s.FleetEnrollmentOwner(context.Background(), "v1:identity:user:alice", "alice")
			allowed := role == "owner" || role == "developer"
			if allowed && (err != nil || got != "v1:identity:user:alice") {
				t.Fatalf("own enrollment: %q %v", got, err)
			}
			if !allowed && !errors.Is(err, ErrFleetEnrollmentDenied) {
				t.Fatalf("role admitted: %q %v", got, err)
			}
			if _, err = s.FleetEnrollmentOwner(context.Background(), "v1:identity:user:alice", "bob"); !errors.Is(err, ErrFleetEnrollmentDenied) {
				t.Fatal("foreign owner admitted")
			}
		})
	}
	f := &fleetEnrollmentEngine{t: t, role: "developer", active: true}
	s := &Store{Engine: f}
	if _, err := s.FleetEnrollmentOwner(context.Background(), "alice", ""); err != nil {
		t.Fatal(err)
	}
	// Redemption on another node has no originating session state and must
	// observe the current user, not the role present when the code was created.
	f.role = "reader"
	if _, err := s.FleetEnrollmentOwner(context.Background(), "alice", ""); !errors.Is(err, ErrFleetEnrollmentDenied) {
		t.Fatal("role downgrade ignored")
	}
	f.role = "owner"
	f.active = false
	if _, err := s.FleetEnrollmentOwner(context.Background(), "alice", ""); !errors.Is(err, ErrFleetEnrollmentDenied) {
		t.Fatal("inactive person admitted")
	}
	f.active = true
	f.missing = true
	if _, err := s.FleetEnrollmentOwner(context.Background(), "alice", ""); !errors.Is(err, ErrFleetEnrollmentDenied) {
		t.Fatal("missing person admitted")
	}
	f.err = errors.New("database offline")
	if _, err := s.FleetEnrollmentOwner(context.Background(), "alice", ""); err == nil || errors.Is(err, ErrFleetEnrollmentDenied) {
		t.Fatalf("lookup failure concealed: %v", err)
	}
}
