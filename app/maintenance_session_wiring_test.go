package app

// maintenance_session_wiring_test.go -- guards which session the closed-chunk
// compaction is handed (component/retention/compact.go), for the reason
// gate_pool_wiring_test.go guards the write-gate pool: nothing in the repo calls
// app.Build(), and the compaction's own tests run on whatever session they open,
// so only this file can lose the CONNECTION. Hand the plug-ins a pool instead
// and every one of those tests still passes, while in the cluster each rewrite
// of a large chunk is cut off at pgdriver's 10 s read deadline or the request
// pool's 8 s statement timeout, and no closed chunk ever gives its space back.

import (
	"os"
	"strings"
	"testing"
)

func TestThePluginContextOpensTheDedicatedMaintenanceSession(t *testing.T) {
	plugins, err := os.ReadFile("plugins.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plugins), "OpenMaintenanceSession: a.maintenanceSessionOpener(),") {
		t.Fatal("plugins.go no longer hands PluginContext.OpenMaintenanceSession the app's maintenance-session opener")
	}
	app, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(app)
	at := strings.Index(body, "func (a *App) maintenanceSessionOpener()")
	if at < 0 {
		t.Fatal("app.go no longer defines maintenanceSessionOpener")
	}
	fn := body[at:]
	if end := strings.Index(fn, "\n}\n"); end > 0 {
		fn = fn[:end]
	}
	if !strings.Contains(fn, "a.db.OpenMaintenanceSession(ctx)") {
		t.Fatalf("maintenanceSessionOpener does not open the database's maintenance session:\n%s", fn)
	}
	for _, pool := range []string{"BunDB()", "DirectBunDB()", "GateDB()"} {
		if strings.Contains(fn, pool) {
			t.Fatalf("maintenanceSessionOpener reaches %s, a pool whose deadlines end a long rewrite part-way:\n%s", pool, fn)
		}
	}
}
