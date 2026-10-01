package memql

import (
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
