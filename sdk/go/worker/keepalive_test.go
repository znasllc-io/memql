package worker

import (
	"testing"
	"time"
)

func TestDialKeepaliveBeatsSixtySecondProxyIdle(t *testing.T) {
	if DefaultKeepaliveTime >= 60*time.Second {
		t.Fatalf("DefaultKeepaliveTime %s must be well under a 60s proxy idle", DefaultKeepaliveTime)
	}
	if DefaultKeepaliveTime < 5*time.Second {
		t.Fatalf("DefaultKeepaliveTime %s is too aggressive for production", DefaultKeepaliveTime)
	}
	if DefaultKeepaliveTimeout <= 0 || DefaultKeepaliveTimeout >= DefaultKeepaliveTime {
		t.Fatalf("DefaultKeepaliveTimeout %s must be positive and under Time %s", DefaultKeepaliveTimeout, DefaultKeepaliveTime)
	}
}
