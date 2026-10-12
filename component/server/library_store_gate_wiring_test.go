package server

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// library_store_gate_wiring_test.go -- a library store with no gate pool says
// which wiring it lacks (memql#6022, F9 fix round 4), instead of "file version
// coordination is unavailable".

type noExecutor struct{}

func (noExecutor) Execute(context.Context, string) (any, error) { return nil, nil }

func TestALibraryStoreWithNoGatePoolNamesTheWiringItLacks(t *testing.T) {
	release, err := NewEngineLibraryStore(noExecutor{}).versionGate(context.Background(), "file-1")
	if release != nil || !errors.Is(err, memql.ErrWriteGatePoolMissing) {
		t.Fatalf("an unwired library store's file-version gate answered %v, want ErrWriteGatePoolMissing", err)
	}
	if !strings.Contains(err.Error(), "server.NewEngineLibraryStore") {
		t.Errorf("the answer does not say which wiring is missing: %v", err)
	}
	if _, err := NewEngineLibraryStore(noExecutor{}, func() *sql.DB { return nil }).versionGate(context.Background(), "file-1"); !errors.Is(err, memql.ErrWriteGatePoolMissing) || !strings.Contains(err.Error(), "failed to open the write-gate pool") {
		t.Errorf("a resolver with no pool answered %v", err)
	}
}
