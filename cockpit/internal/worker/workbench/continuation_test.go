//go:build darwin || linux

package workbench

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestActiveWorkbenchRenewsLeaseWithoutLosingFiles(t *testing.T) {
	f := &fakeEngine{}
	m := New(f, t.TempDir())
	now := time.Now()
	m.now = func() time.Time { return now }
	defer m.CloseHome("home")
	var workspace any
	for elapsed := time.Duration(0); elapsed <= 2*Lifetime; elapsed += IdleTTL / 2 {
		id := identity("continuing-goal")
		id.ExpiresAt = now.Add(Lifetime)
		r, err := m.Execute(context.Background(), id, request())
		if err != nil {
			t.Fatalf("active goal stopped after %s: %v", elapsed, err)
		}
		if workspace != nil && workspace != r.Payload {
			t.Fatal("renewal replaced the existing filesystem")
		}
		workspace = r.Payload
		now = now.Add(IdleTTL / 2)
		m.Sweep()
	}
	if len(f.created) != 1 || len(f.removed) != 0 {
		t.Fatalf("renewal recreated the workspace: created=%v removed=%v", f.created, f.removed)
	}
}

func TestExplicitRecoveryUsesNewGenerationWithoutReplayingLostOperation(t *testing.T) {
	f := &fakeEngine{}
	m := New(f, t.TempDir())
	now := time.Now()
	m.now = func() time.Time { return now }
	defer m.CloseHome("home")
	id := identity("continuing-goal")
	old, err := m.Execute(context.Background(), id, request())
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(IdleTTL + time.Second)
	m.Sweep()
	if _, err = m.Execute(context.Background(), id, request()); !errors.Is(err, ErrStateLost) {
		t.Fatalf("lost state not reported: %v", err)
	}
	key, _ := id.Key()
	if _, err := os.Stat(filepath.Join(m.root, "workbench-ended-"+key)); err != nil {
		t.Fatal(err)
	}
	id.Generation++
	id.ExpiresAt = now.Add(Lifetime)
	current, err := m.Execute(context.Background(), id, request())
	if err != nil || old.Payload == current.Payload {
		t.Fatalf("recovery did not isolate the new filesystem: %v", err)
	}
	if len(f.created) != 2 || len(f.removed) != 1 {
		t.Fatalf("unexpected replay/cleanup: created=%v removed=%v", f.created, f.removed)
	}
	id.Generation--
	if _, err = m.Execute(context.Background(), id, request()); !errors.Is(err, ErrStateLost) {
		t.Fatalf("old generation revived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.root, "workbench-ended-"+key)); err != nil {
		t.Fatal("recovery erased the prior tombstone", err)
	}
}
