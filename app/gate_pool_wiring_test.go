package app

// gate_pool_wiring_test.go -- guards the wiring of the write-gate pool
// (memql#6022, F9 fix round 3).
//
// WHY A SOURCE-LEVEL GUARD, as campaign_digest_sweep_wiring_test.go: nothing in
// the repo calls app.Build(), and standing up a real App needs a database. The
// behaviour is tested where it lives -- the pool and its sessions in
// component/database, the gate in component/memql, each caller's use of its gate
// in its own package -- and what only this file can lose is the CONNECTION: which
// pool each caller is handed. Hand any of them the request pool again and every
// one of those suites still passes (they run on whatever pool the test supplies)
// while the pod goes back to gates that take the connections their holders work
// on, and whose locks the request pool's 60 s idle limit ends without a word.

import (
	"os"
	"strings"
	"testing"
)

// gateWiringSite is one place in app/ that hands a write-gate caller its pool.
type gateWiringSite struct {
	file    string // in app/
	call    string // the constructor or option the pool is given to
	wanted  string // the expression that must follow
	missing string // what is lost when it goes
}

var gateWiringSites = []gateWiringSite{
	{"engine.go", "campaigns.NewWorker(", "a.gateDBGetter()", "the campaigns worker's newsletter, series, template, test-send and single-send gates"},
	{"integrations_library.go", "server.NewEngineLibraryStore(", "a.gateDBGetter()", "the image store's file-version gate"},
	{"transport_artifacts.go", "server.NewEngineLibraryStore(", "a.gateDBGetter()", "the artifact handler's file-version gate"},
	{"integrations_planner_init.go", "planner.WithGateDBGetter(", "a.gateDBGetter()", "the planner's standing-directive gate"},
	{"plugins.go", "GateDB:", "a.gateDBGetter()", "the library, work and email plug-ins' gates (PluginContext.GateDB)"},
}

func TestEveryWriteGateCallerIsHandedTheGatePool(t *testing.T) {
	for _, site := range gateWiringSites {
		src, err := os.ReadFile(site.file)
		if err != nil {
			t.Fatalf("read %s: %v", site.file, err)
		}
		body := string(src)
		at := strings.Index(body, site.call)
		if at < 0 {
			t.Errorf("%s no longer contains %q; this guard needs updating, or %s lost its pool", site.file, site.call, site.missing)
			continue
		}
		// The pool is part of the same call or option: look within its argument list.
		window := body[at:]
		if end := strings.Index(window, "\n\t)"); end > 0 {
			window = window[:end]
		}
		if len(window) > 700 {
			window = window[:700]
		}
		if !strings.Contains(window, site.wanted) {
			t.Errorf("%s: %s is not given %s. %s would take its gate on a pool that is not the gate pool: the connections its holder works on, and sessions the request pool ends after 60 s with the lock",
				site.file, site.call, site.wanted, site.missing)
		}
	}
}

// TestNoWriteGateCallerResolvesTheRequestPool: the places that used to hand the
// gates the request pool's *sql.DB are gone, and none was replaced by another.
func TestNoWriteGateCallerResolvesTheRequestPool(t *testing.T) {
	for _, file := range []string{"engine.go", "integrations_library.go", "transport_artifacts.go", "integrations_planner_init.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		body := string(src)
		for _, constructor := range []string{"campaigns.NewWorker(", "server.NewEngineLibraryStore(", "planner.WithGateDBGetter("} {
			at := strings.Index(body, constructor)
			if at < 0 {
				continue
			}
			window := body[at:]
			if end := strings.Index(window, "\n\t)"); end > 0 {
				window = window[:end]
			}
			if len(window) > 700 {
				window = window[:700]
			}
			for _, request := range []string{"semanticCacheDBGetter", ".BunDB()", "DirectBunDB"} {
				if strings.Contains(window, request) {
					t.Errorf("%s: %s is given %s, a request-pool handle, beside its gate pool", file, constructor, request)
				}
			}
		}
	}
}

// TestTheGatePoolGetterIsNilWithoutADatabase: an App that has not started its
// database phase hands out no pool, and a gate says so instead of panicking.
func TestTheGatePoolGetterIsNilWithoutADatabase(t *testing.T) {
	a := &App{}
	if db := a.gateDBGetter()(); db != nil {
		t.Fatalf("an App with no database handed out a gate pool: %v", db)
	}
}
