package memql

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestVersionTimeAfterPreservesOrderingAcrossClockSkew(t *testing.T) {
	prior := time.Date(2030, 1, 1, 0, 0, 0, 123456789, time.UTC)
	for _, now := range []time.Time{prior.Add(-time.Hour), prior, prior.Add(10 * time.Nanosecond)} {
		result := VersionTimeAfter(prior, now)
		if !result.After(prior.Truncate(time.Microsecond)) || result.Nanosecond()%1000 != 0 {
			t.Fatalf("not a later PostgreSQL timestamp: prior %v now %v result %v", prior, now, result)
		}
	}
	future := prior.Add(time.Hour)
	if got := VersionTimeAfter(prior, future); !got.Equal(future.Truncate(time.Microsecond)) {
		t.Fatalf("ignored forward clock: %v", got)
	}
}

// TestAWriteGateWithNoPoolSaysWhichWiringIsMissing: a caller built without the
// gate pool used to answer only "shared write coordination is unavailable", and
// the proving harness failed that way deep inside a lifecycle with nothing to say
// what was missing. The answer names the wiring, is recognisable by errors.Is, and
// is not busy -- nothing will change by waiting.
func TestAWriteGateWithNoPoolSaysWhichWiringIsMissing(t *testing.T) {
	_, err := AcquireWriteGate(context.Background(), nil, "some-key")
	if !errors.Is(err, ErrWriteGatePoolMissing) {
		t.Fatalf("a gate with no pool answered %v, want ErrWriteGatePoolMissing", err)
	}
	for _, names := range []string{"database.Database.GateDB", "PluginContext.GateDB", "database.NewGatePool"} {
		if !strings.Contains(err.Error(), names) {
			t.Errorf("a gate with no pool answered %q, which does not name %s", err, names)
		}
	}
	if IsWriteGateBusy(err) {
		t.Errorf("a missing pool reads as busy: %v", err)
	}
}

// TestResolvingAGatePoolNamesTheCallerAndItsSetter: the two ways a caller can lack
// its pool -- never given a resolver, or a resolver that has none (the database
// has not started, has stopped, or could not open its gate pool) -- each say so,
// with the caller's name and the way it is given one.
func TestResolvingAGatePoolNamesTheCallerAndItsSetter(t *testing.T) {
	_, err := ResolveWriteGatePool(nil, "acme", "give it database.Database.GateDB with Acme.SetGateDB")
	if !errors.Is(err, ErrWriteGatePoolMissing) || !strings.Contains(err.Error(), "acme:") || !strings.Contains(err.Error(), "Acme.SetGateDB") {
		t.Errorf("an unwired caller answered %v", err)
	}
	_, err = ResolveWriteGatePool(func() *sql.DB { return nil }, "acme", "give it database.Database.GateDB with Acme.SetGateDB")
	if !errors.Is(err, ErrWriteGatePoolMissing) || !strings.Contains(err.Error(), "acme:") || !strings.Contains(err.Error(), "failed to open the write-gate pool") {
		t.Errorf("a caller whose resolver has no pool answered %v", err)
	}
	if IsWriteGateBusy(err) {
		t.Errorf("a missing pool reads as busy: %v", err)
	}
	pool := &sql.DB{}
	if got, err := ResolveWriteGatePool(func() *sql.DB { return pool }, "acme", "-"); err != nil || got != pool {
		t.Errorf("a wired caller resolved %v, %v", got, err)
	}
}
