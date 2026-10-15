package filebackups

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/database/storagequota"
	"github.com/znasllc-io/memql/component/memql"
)

type quotaFixture struct {
	*storageFixture
	quota *storagequota.Store
}

func (p *quotaFixture) ReserveStorage(ctx context.Context, container, object string, size int64) error {
	g, err := p.quota.Guard(ctx, "fixture", container+"/"+object, "")
	if err != nil {
		return err
	}
	defer g.Close()
	return g.Plan(ctx, size)
}

func TestBackupConcurrentAdmissionUsesOneGateConnectionPerReplica(t *testing.T) {
	s, replica, ctx, p := serviceFixture(t)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	services := []*Service{s, replica()}
	for _, service := range services {
		service.GateDB().SetMaxOpenConns(1)
		q := &storagequota.Store{DB: service.DB, GateDB: service.GateDB}
		if err := q.Initialize(ctx, "fixture", func(func(string, string, int64) error) error { return nil }); err != nil {
			t.Fatal(err)
		}
		service.Provider = &quotaFixture{storageFixture: p, quota: q}
	}
	// Each of two replicas handles concurrent jobs with a one-slot gate pool.
	// Admission takes a real quota object lock under the real transfer gate.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for n := range 8 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			service := services[n%len(services)]
			id := fmt.Sprintf("admission-%d", n)
			if _, err := service.Create(ctx, id, "worker-001", "/source/test.zip", "archive", Metadata{}); err != nil {
				errs <- err
				return
			}
			// Stop before storage creation: the shared fixture models one blob,
			// whereas this regression concerns the admission lock for eight blobs.
			locked, release, err := service.gate(ctx, id)
			if err != nil {
				errs <- err
				return
			}
			defer release()
			job, err := service.read(locked, id, "owner-001")
			if err == nil {
				err = service.Provider.ReserveStorage(locked, service.Container, job.Object, 4)
			}
			errs <- err
		}(n)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	q := services[0].Provider.(*quotaFixture).quota
	u, err := q.Usage(ctx, "")
	if err != nil || u.ClusterReserved != 8*(4+storagequota.ObjectOverhead) {
		t.Fatalf("admission receipt: %+v, %v", u, err)
	}
	for _, service := range services {
		if stats := service.GateDB().Stats(); stats.InUse != 0 || stats.OpenConnections > 1 {
			t.Fatalf("gate pool leaked or expanded: %+v", stats)
		}
	}
}

func TestNestedBackupGatesRemainHeldUntilOuterRelease(t *testing.T) {
	s, replica, ctx, _ := serviceFixture(t)
	s.GateDB().SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	outer, err := memql.AcquireGateHold(ctx, s.GateDB(), 0, "transfer-test")
	if err != nil {
		t.Fatal(err)
	}
	defer outer.Release()
	shared := outer.Context(ctx)
	q := &storagequota.Store{DB: s.DB, GateDB: s.GateDB}
	guard, err := q.Guard(shared, "fixture", "nested-object", "")
	if err != nil {
		t.Fatal(err)
	}
	guard.Close()
	if err = outer.Verify(ctx); err != nil {
		t.Fatalf("closing a borrowed quota gate ended the transfer gate: %v", err)
	}
	for _, key := range []string{"sync-source-test", "library-file-test"} {
		inner, err := memql.AcquireGateHold(shared, s.GateDB(), 0, key)
		if err != nil {
			t.Fatal(err)
		}
		if err = inner.Verify(shared); err != nil {
			t.Fatal(err)
		}
		inner.Release()
	}
	other := replica()
	_, err = memql.AcquireGateHold(ctx, other.GateDB(), 150*time.Millisecond, "library-file-test")
	if !errors.Is(err, memql.ErrWriteGateBusy) {
		t.Fatalf("nested release dropped the outer transaction's lock: %v", err)
	}
	if err = outer.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	outer.Release()
	if _, err = q.Guard(shared, "fixture", "after-release", ""); err == nil {
		t.Fatal("a stale scope silently acquired a fresh quota gate")
	}
	if _, err = memql.AcquireGateHold(shared, s.GateDB(), 0, "after-release"); err == nil {
		t.Fatal("a stale shared scope silently acquired a fresh gate")
	}
	inner, err := memql.AcquireGateHold(ctx, other.GateDB(), 0, "library-file-test")
	if err != nil {
		t.Fatal(err)
	}
	inner.Release()
}

func TestArchivePrepareBorrowsItsQuotaGate(t *testing.T) {
	s, _, ctx, p := serviceFixture(t)
	s.GateDB().SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	q := &storagequota.Store{DB: s.DB, GateDB: s.GateDB}
	if err := q.Initialize(ctx, "fixture", func(func(string, string, int64) error) error { return nil }); err != nil {
		t.Fatal(err)
	}
	s.Provider = &quotaFixture{storageFixture: p, quota: q}
	job, err := s.Create(ctx, "small-archive", "worker-001", "/source/small.zip", "archive", Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	job, err = s.Prepare(ctx, job.ID, 4, strings.Repeat("a", 64))
	if err != nil || job.State != "uploading" {
		t.Fatalf("prepare with one gate connection: %+v, %v", job, err)
	}
}
