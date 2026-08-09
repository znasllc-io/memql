package identity

import (
	"context"
	"errors"
	"strings"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// store_no_unbootstrap_3415_test.go guards the Go half of memql#3415: the
// identity Store must never emit a write that takes a stamped cluster back to
// un-bootstrapped.
//
// The engine-side @noUnset("bootstrappedAt") annotation on the two
// clusterSettings mutations is the structural guarantee, and it covers callers
// this package never sees (a raw named-mutation call, a test, an automation).
// This layer is the second one: PersistClusterSettings builds its own mutation
// string from a struct whose zero value has BootstrappedAt == "", so "I forgot
// to carry the stamp forward" is the natural mistake to make here. It is
// refused at the point the string is built, not trusted to the annotation.

// recordingEngine answers clusterSettingsCurrent from a fixture and records
// every query it is handed, so a test can assert on what the store EMITTED.
type recordingEngine struct {
	settingsNodes []*memqlv1.MemoryNode
	settingsErr   error
	queries       []string
}

func (f *recordingEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	f.queries = append(f.queries, q)
	if strings.Contains(q, "clusterSettingsCurrent(") {
		if f.settingsErr != nil {
			return nil, f.settingsErr
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: f.settingsNodes}}, nil
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

func (f *recordingEngine) writes() []string {
	var out []string
	for _, q := range f.queries {
		if strings.Contains(q, "ClusterSettings(") && !strings.Contains(q, "clusterSettingsCurrent(") {
			out = append(out, q)
		}
	}
	return out
}

// TestPersist3415_CarriesForwardAnExistingBootstrapStamp is the incident at the
// store: a caller persists a settings row without a stamp (the auto-bootstrap
// row literal has no BootstrappedAt field at all) onto a cluster that IS
// bootstrapped. The emitted mutation must carry the stored stamp, not "".
func TestPersist3415_CarriesForwardAnExistingBootstrapStamp(t *testing.T) {
	const stamp = "2026-08-09T07:38:20Z"
	eng := &recordingEngine{settingsNodes: []*memqlv1.MemoryNode{clusterSettingsNode(stamp)}}
	s := &Store{Engine: eng}

	if err := s.PersistClusterSettings(context.Background(), ClusterSettingsRow{
		RegistrationMode:    "open",
		InternalDefaultRole: "reader",
	}); err != nil {
		t.Fatalf("PersistClusterSettings: %v", err)
	}

	writes := eng.writes()
	if len(writes) != 1 {
		t.Fatalf("expected exactly 1 clusterSettings write, got %d: %v", len(writes), writes)
	}
	if strings.Contains(writes[0], `bootstrappedAt: ""`) {
		t.Errorf("emitted mutation blanks bootstrappedAt on a bootstrapped cluster (memql#3415):\n%s", writes[0])
	}
	if !strings.Contains(writes[0], `bootstrappedAt: "`+stamp+`"`) {
		t.Errorf("emitted mutation must carry the stored stamp %q forward (memql#3415):\n%s", stamp, writes[0])
	}
}

// TestPersist3415_UnreadableStateRefusesTheWrite pins the fail-closed
// direction. If the current row cannot be read, the store cannot prove the
// write is not a blanking write, and the surface at stake is the one that
// decides whether the ownership wizard is open. Refuse.
func TestPersist3415_UnreadableStateRefusesTheWrite(t *testing.T) {
	eng := &recordingEngine{settingsErr: errors.New("db unreachable")}
	s := &Store{Engine: eng}

	err := s.PersistClusterSettings(context.Background(), ClusterSettingsRow{
		RegistrationMode:    "open",
		InternalDefaultRole: "reader",
	})
	if err == nil {
		t.Fatal("PersistClusterSettings must fail when the current settings row cannot be read (memql#3415)")
	}
	if writes := eng.writes(); len(writes) != 0 {
		t.Errorf("no write may be emitted when cluster state is unknown; got %v", writes)
	}
}

// TestPersist3415_FreshClusterWritesAnEmptyStamp is the counterweight: the
// first-run path (no settings row yet) must still write, with the stamp empty.
// The wizard's magic-link consume is what fills it in later.
func TestPersist3415_FreshClusterWritesAnEmptyStamp(t *testing.T) {
	eng := &recordingEngine{}
	s := &Store{Engine: eng}

	if err := s.PersistClusterSettings(context.Background(), ClusterSettingsRow{
		RegistrationMode:    "open",
		InternalDefaultRole: "reader",
		BootstrapEmail:      "owner@example.com",
	}); err != nil {
		t.Fatalf("PersistClusterSettings on a fresh cluster: %v", err)
	}
	writes := eng.writes()
	if len(writes) != 1 {
		t.Fatalf("expected exactly 1 clusterSettings write, got %d: %v", len(writes), writes)
	}
	if !strings.Contains(writes[0], `bootstrappedAt: ""`) {
		t.Errorf("a fresh cluster must still be writable with an empty stamp:\n%s", writes[0])
	}
}

// TestPersist3415_CallerSuppliedStampIsPreserved: a caller that DOES supply a
// stamp keeps it. The guard refuses only set -> unset, never set -> set.
func TestPersist3415_CallerSuppliedStampIsPreserved(t *testing.T) {
	const stored = "2026-08-09T07:38:20Z"
	const supplied = "2026-08-09T11:15:20Z"
	eng := &recordingEngine{settingsNodes: []*memqlv1.MemoryNode{clusterSettingsNode(stored)}}
	s := &Store{Engine: eng}

	if err := s.PersistClusterSettings(context.Background(), ClusterSettingsRow{
		RegistrationMode:    "open",
		InternalDefaultRole: "reader",
		BootstrappedAt:      supplied,
	}); err != nil {
		t.Fatalf("PersistClusterSettings: %v", err)
	}
	writes := eng.writes()
	if len(writes) != 1 {
		t.Fatalf("expected exactly 1 clusterSettings write, got %d: %v", len(writes), writes)
	}
	if !strings.Contains(writes[0], `bootstrappedAt: "`+supplied+`"`) {
		t.Errorf("an explicitly-supplied stamp must win:\n%s", writes[0])
	}
}
