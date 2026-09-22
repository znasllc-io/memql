package identity

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"
)

func TestBootstrapGateFailsClosedWithoutDatabase(t *testing.T) {
	for _, store := range []*Store{nil, {}, {DirectDB: func() *sql.DB { return nil }}} {
		if release, err := store.AcquireBootstrapGate(context.Background()); err == nil || release != nil {
			t.Fatal("ownership proceeded without coordination")
		}
	}
}

func TestBootstrapGateCoordinatesIndependentReplicas(t *testing.T) {
	db := openDirectDBForMagicLink(t)
	if db == nil {
		return
	}
	first := &Store{DirectDB: func() *sql.DB { return db }}
	second := &Store{DirectDB: func() *sql.DB { return db }}
	release, err := first.AcquireBootstrapGate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	defer once.Do(release)
	started := make(chan struct{})
	acquired := make(chan error, 1)
	go func() {
		close(started)
		unlock, err := second.AcquireBootstrapGate(context.Background())
		if err == nil {
			unlock()
		}
		acquired <- err
	}()
	<-started
	select {
	case err := <-acquired:
		t.Fatalf("second replica passed a held ownership gate: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	once.Do(release)
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("gate not released")
	}
}
