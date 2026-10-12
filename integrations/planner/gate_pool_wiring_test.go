package planner

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// gate_pool_wiring_test.go -- the planner's standing-directive gate, with no gate
// pool, says which wiring it lacks (memql#6022, F9 fix round 4) instead of
// "standing directive requires a shared database".

func TestThePlannersDirectiveGateWithNoPoolNamesTheWiringItLacks(t *testing.T) {
	release, err := (&PlannerIntegration{}).directiveGate(context.Background(), "v1:agents:agent:a1")
	if release != nil || !errors.Is(err, memql.ErrWriteGatePoolMissing) {
		t.Fatalf("an unwired planner's directive gate answered %v, want ErrWriteGatePoolMissing", err)
	}
	if !strings.Contains(err.Error(), "planner.WithGateDBGetter") {
		t.Errorf("the answer does not say which wiring is missing: %v", err)
	}
	wired := &PlannerIntegration{gateDBGetter: func() *sql.DB { return nil }}
	if _, err := wired.directiveGate(context.Background(), "v1:agents:agent:a1"); !errors.Is(err, memql.ErrWriteGatePoolMissing) || !strings.Contains(err.Error(), "failed to open the write-gate pool") {
		t.Errorf("a resolver with no pool answered %v", err)
	}
}
