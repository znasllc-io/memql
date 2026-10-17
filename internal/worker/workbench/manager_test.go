//go:build darwin || linux

package workbench

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeEngine struct {
	mu               sync.Mutex
	created, removed []string
	execute          func(context.Context, string, Request) (Result, error)
}

func (*fakeEngine) Ready(context.Context) (Runtime, error) {
	return Runtime{ID: "daemon", CPUs: 8, MemoryBytes: 16 << 30}, nil
}
func (f *fakeEngine) Create(_ context.Context, _ Runtime, name, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, name)
	return nil
}
func (f *fakeEngine) Remove(_ context.Context, _ Runtime, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, name)
	return nil
}
func (f *fakeEngine) Execute(ctx context.Context, _ Runtime, name string, r Request) (Result, error) {
	if f.execute != nil {
		return f.execute(ctx, name, r)
	}
	return Result{OK: true, Action: r.Action, Payload: name}, nil
}
func identity(run string) Identity {
	return Identity{Home: "home", Owner: "owner", Caller: "caller", Registration: "reg", Machine: "machine", Run: run, ExpiresAt: time.Now().Add(Lifetime)}
}
func request() Request { return Request{Action: "exec", Args: map[string]any{"cmd": "true"}} }
func TestWorkspacesSeparateBorrowersAndKeepCapacityAcrossHomes(t *testing.T) {
	f := &fakeEngine{}
	root := t.TempDir()
	a, b := New(f, root), New(f, root)
	one, err := a.Execute(context.Background(), identity("one"), request())
	if err != nil {
		t.Fatal(err)
	}
	same, err := a.Execute(context.Background(), identity("one"), request())
	if err != nil || one.Payload != same.Payload {
		t.Fatal("same run lost its workspace", err)
	}
	other := identity("one")
	other.Caller = "another-caller"
	two, err := b.Execute(context.Background(), other, request())
	if err != nil || one.Payload == two.Payload {
		t.Fatal("borrowers shared a workspace", err)
	}
	if _, err := a.Execute(context.Background(), identity("third"), request()); !errors.Is(err, ErrBusy) {
		t.Fatal("third workspace bypassed machine-wide capacity", err)
	}
	if err := a.CloseHome("home"); err != nil {
		t.Fatal(err)
	}
	if err := b.CloseHome("home"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Execute(context.Background(), identity("one"), request()); err == nil {
		t.Fatal("ended workspace silently started over")
	}
}
func TestWorkbenchNeverRunsBesideExclusivePipelineBuild(t *testing.T) {
	root := t.TempDir()
	build, err := lockFile(filepath.Join(root, "pipeline.lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeEngine{}
	m := New(f, root)
	if _, err = m.Execute(context.Background(), identity("run"), request()); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if len(f.created) != 0 {
		t.Fatal("capacity refusal created a container")
	}
	build.Close()
	if _, err = m.Execute(context.Background(), identity("run"), request()); err != nil {
		t.Fatal(err)
	}
	if _, err = lockFile(filepath.Join(root, "pipeline.lock"), false); !errors.Is(err, ErrBusy) {
		t.Fatal("pipeline bypassed workspace reservation", err)
	}
	_ = m.CloseHome("home")
}
func TestCancellationDestroysOnlyThatWorkspaceAndNeverReplays(t *testing.T) {
	f := &fakeEngine{}
	m := New(f, t.TempDir())
	if _, err := m.Execute(context.Background(), identity("other"), request()); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	f.execute = func(ctx context.Context, _ string, _ Request) (Result, error) {
		close(started)
		<-ctx.Done()
		return Result{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := m.Execute(ctx, identity("cancelled"), request()); done <- err }()
	<-started
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancellation ignored")
	}
	if len(f.removed) != 1 {
		t.Fatal("cancellation crossed workspace scope", f.removed)
	}
	f.execute = nil
	if _, err := m.Execute(context.Background(), identity("cancelled"), request()); err == nil {
		t.Fatal("cancelled work replayed")
	}
	if _, err := m.Execute(context.Background(), identity("other"), request()); err != nil {
		t.Fatal("unrelated workspace stopped", err)
	}
	_ = m.CloseHome("home")
}
func TestIdleWorkspaceReleasesCapacityWithoutRecreatingOldGoal(t *testing.T) {
	f := &fakeEngine{}
	m := New(f, t.TempDir())
	now := time.Now()
	m.now = func() time.Time { return now }
	if _, err := m.Execute(context.Background(), identity("old"), request()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(IdleTTL + time.Second)
	m.Sweep()
	if _, err := m.Execute(context.Background(), identity("old"), request()); err == nil {
		t.Fatal("expired goal got an empty replacement")
	}
	if _, err := m.Execute(context.Background(), identity("new"), request()); err != nil {
		t.Fatal(err)
	}
	_ = m.CloseHome("home")
}
func TestUncertainOrphanIsReconciledWithoutReplaying(t *testing.T) {
	root := t.TempDir()
	lease, err := reserve(root)
	if err != nil {
		t.Fatal(err)
	}
	old := identity("old")
	key, _ := old.Key()
	name := "memql-workbench-0123456789abcdef0123456789abcdef"
	if err = lease.save(record{Name: name, Key: key, DockerID: "daemon"}); err != nil {
		t.Fatal(err)
	}
	lease.close()
	f := &fakeEngine{}
	m := New(f, root)
	if _, err = m.Execute(context.Background(), identity("new"), request()); !errors.Is(err, ErrInterrupted) {
		t.Fatal(err)
	}
	if len(f.created) != 0 || len(f.removed) != 1 || f.removed[0] != name {
		t.Fatal("orphan was replayed or cleanup exceeded its scope")
	}
	if _, err = m.Execute(context.Background(), old, request()); err == nil {
		t.Fatal("interrupted goal got an empty replacement")
	}
	if _, err = m.Execute(context.Background(), identity("new"), request()); err != nil {
		t.Fatal(err)
	}
	_ = m.CloseHome("home")
}

func TestRestartCleansBothRecordedSlotsBeforePipelinesResume(t *testing.T) {
	root := t.TempDir()
	keys := make([]string, Slots)
	for slot := 0; slot < Slots; slot++ {
		lease, err := reserveAt(root, slot)
		if err != nil {
			t.Fatal(err)
		}
		keys[slot], _ = identity(fmt.Sprint(slot)).Key()
		if err := lease.save(record{Name: fmt.Sprintf("memql-workbench-%032x", slot+1), Key: keys[slot], DockerID: "daemon"}); err != nil {
			t.Fatal(err)
		}
		lease.close()
	}
	if pending, err := HasInterrupted(root); err != nil || !pending {
		t.Fatal("orphan was not fenced", err)
	}
	f := &fakeEngine{}
	m := New(f, root)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.removed) != Slots || len(f.created) != 0 {
		t.Fatal("recovery did not remove exactly its recorded containers")
	}
	if pending, err := HasInterrupted(root); err != nil || pending {
		t.Fatal("completed cleanup did not free CI capacity", err)
	}
	for slot := 0; slot < Slots; slot++ {
		if _, err := m.Execute(context.Background(), identity(fmt.Sprint(slot)), request()); err == nil {
			t.Fatal("recovery replayed a lost workspace")
		}
	}
}
