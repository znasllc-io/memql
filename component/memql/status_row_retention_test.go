package memql

import (
	"testing"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// THE DRIVER'S BOOKKEEPING EXPIRES; THE RECORD DOES NOT.
//
// v1:pipelines:run and v1:worker:appSession are rewritten by their holders on a
// heartbeat (every 30 s and every 2 s) and on every step, and each write appends a
// full version of the row: on production on 2026-11-06 that was ~17 KB a pipeline
// run version (its policySnapshot copied each time) and ~10 KB an app session
// version, 1.9 GB in the week of October 29. Nothing reads a superseded version of
// either; what each run or session did is kept as rows of its own (one pipeline run
// row per attempt; a session's steps and Library transcript). So each concept keeps
// only its newest version past one day, and does not archive the rest.
//
// These are the pins that fail first if a declaration is edited away. The behaviour
// that follows -- the hourly sweep retiring exactly the superseded versions past a
// day, under the maintenance principal and the real row admission -- is proved
// against a database in component/retention/status_rows_db_test.go.
func TestHotStatusRowsKeepOnlyTheirNewestVersionPastADay(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	want := memorynodes.RetentionPolicy{TTL: 24 * time.Hour, KeepLatest: true, Archive: "none"}
	for _, name := range []string{"v1:pipelines:run", "v1:worker:appSession"} {
		c, err := memorynodes.Get(name)
		if err != nil || c == nil {
			t.Fatalf("%s is not in the loaded tree: %v", name, err)
		}
		if c.Retention == nil {
			t.Errorf("%s declares no @retention, so every heartbeat version is kept forever", name)
			continue
		}
		if got := *c.Retention; got != want {
			t.Errorf("%s retention = %+v, want %+v: keepLatest=false would delete the run or session itself, and archive=required would copy disposable bookkeeping into object storage before anything is deleted", name, got, want)
		}
	}
}
