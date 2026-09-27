package automations

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// The fleet bundle's clearDelinquencyOnRecovery (deploy/fleet/dsl/fleet/
// automations.memql) must fire on the TRANSITION past_due -> active and on
// nothing else (memql#5437 review). Every write to an active subscription is a
// node.updated event -- a period roll, a mirrored field -- and firing on the
// state rewrote the subscriber on each of them, returning an account to
// `active` that an operator had set otherwise.
//
// Driven through the path the scheduler fires through, as the procedure
// ladder's filters are (procedure_ladder_filters_test.go): bindEventArgs, then
// evaluateTriggerFilter, over the event executeUpdate publishes. The bundle is
// not embedded, so it is mounted over the embedded tree the way a node mounts
// MEMQL_DSL_PATH.

func loadedFleetAutomation(t *testing.T, name string) *Automation {
	t.Helper()
	quiet := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	bundle := os.DirFS(filepath.Join("..", "..", "deploy", "fleet", "dsl"))
	_, _, unmount := memqldsl.MountOverlayDomains(quiet, bundle)
	t.Cleanup(func() {
		unmount()
		memorynodes.ReplaceAll(nil)
		_, _ = memql.LoadUnifiedConcepts(quiet)
	})
	if _, err := memql.LoadUnifiedConcepts(quiet); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	loaded, err := NewLoader(LoaderOptions{Logger: quiet}).LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	for _, a := range loaded {
		if a != nil && a.Name == name {
			if a.Trigger == nil || a.Trigger.Filter == "" {
				t.Fatalf("%s loaded with no trigger filter; the property under test is the filter", name)
			}
			return a
		}
	}
	t.Fatalf("no automation named %s loads from the fleet bundle (%d loaded)", name, len(loaded))
	return nil
}

func TestClearDelinquencyFiresOnlyOnRecoveryFromPastDue(t *testing.T) {
	a := loadedFleetAutomation(t, "clearDelinquencyOnRecovery")
	const subscription = "v1:fleet:subscription"
	for _, tc := range []struct {
		name      string
		status    string
		oldStatus string
		fires     bool
	}{
		{"past_due to active: the recovery", "active", "past_due", true},
		// The shapes the filter exists to refuse.
		{"a later write to an active subscription", "active", "active", false},
		{"a trial converting", "active", "trialing", false},
		{"a paused subscription resuming", "active", "paused", false},
		{"a first write that is already active", "active", "", false},
		{"still past_due", "past_due", "past_due", false},
		{"active to past_due", "past_due", "active", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := map[string]any{"status": tc.status, "subscriberId": "v1:fleet:subscriber:s1"}
			ev := graphUpdatedEvent(subscription, subscription+":sub1", fields, tc.oldStatus)
			if got := filterFires(t, a, ev); got != tc.fires {
				t.Fatalf("clearDelinquencyOnRecovery fired=%v for %s, want %v", got, tc.name, tc.fires)
			}
		})
	}
}
