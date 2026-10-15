package campaigns

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// gate_pool_wiring_test.go -- a campaigns worker with no gate pool says which
// wiring it lacks (memql#6022, F9 fix round 4), at every gate it takes.

func TestACampaignsWorkerWithNoGatePoolNamesTheWiringItLacks(t *testing.T) {
	for name, gateDB := range map[string][]func() *sql.DB{
		"never given one":            nil,
		"given a resolver with none": {func() *sql.DB { return nil }},
	} {
		w := NewWorker(&fakeEngine{}, nil, nil, quietLogger(), gateDB...)
		gates := map[string]func() error{
			"newsletter": func() error {
				_, err := w.newsletterGate(context.Background(), newsletterLock("site", "s1"))
				return err
			},
			"test send":       func() error { _, err := w.testGate(context.Background(), "k"); return err },
			"series":          func() error { _, err := w.seriesGate(context.Background(), "k"); return err },
			"provider ledger": func() error { _, err := w.providerLedgerGate(context.Background(), "k"); return err },
			"progress":        func() error { _, err := w.progressGate(context.Background(), "k"); return err },
			"single send":     func() error { _, err := w.singleSendGate(context.Background(), "k"); return err },
			"template":        func() error { _, err := w.templateGate(context.Background(), "k"); return err },
		}
		for gate, take := range gates {
			err := take()
			if !errors.Is(err, memql.ErrWriteGatePoolMissing) {
				t.Errorf("%s: the %s gate answered %v, want ErrWriteGatePoolMissing", name, gate, err)
				continue
			}
			if gateDB == nil && !strings.Contains(err.Error(), "campaigns.NewWorker") {
				t.Errorf("%s: the %s gate does not say which wiring is missing: %v", name, gate, err)
			}
			if gateDB != nil && !strings.Contains(err.Error(), "failed to open the write-gate pool") {
				t.Errorf("%s: the %s gate does not say the pool is not open: %v", name, gate, err)
			}
		}
	}
}
