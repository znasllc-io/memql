package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/database"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// A fresh process is essential: an earlier analyzer/engine test may already
// have anchored the packs, hiding the first-boot ordering that broke Fylo.
func TestDatabasePhaseResolvesProductPackImportsBeforeEnginePhase(t *testing.T) {
	const childEnv = "MEMQL_TEST_STOREFRONT_DATABASE_PHASE"
	if os.Getenv(childEnv) != "1" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, "-test.run=^TestDatabasePhaseResolvesProductPackImportsBeforeEnginePhase$")
		cmd.Env = append(os.Environ(), childEnv+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fresh database phase: %v\n%s", err, out)
		}
		return
	}
	root := t.TempDir()
	domain := filepath.Join(root, "fylo")
	if err := os.Mkdir(domain, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"memql.toml": "memql = \"1.0\"\nedition = \"2026\"\n",
		"concepts.memql": `use wholesale.concepts.{ application }
@version("1.0.0")
@description("A product's qualifying detail attached to a wholesale application.")
@rowAuthz(owner="ownerUserId", clusterOwner)
concept applicationDetail {
 ownerUserId string!
 applicationId string!
 @relationship(type="references", field="applicationId", target=application, direction="outgoing")
}
`,
	} {
		if err := os.WriteFile(filepath.Join(domain, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MEMQL_DSL_PATH", root)
	t.Setenv(memql.AllowSkipsEnvVar, "")
	app := &App{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), mux: http.NewServeMux(), overrides: Overrides{
		NewDatabase: func(...database.DatabaseArg) (*memorynodes.MemoryNodesDatabase, error) {
			return &memorynodes.MemoryNodesDatabase{}, nil
		},
		FatalWithLogger: func(_ *slog.Logger, msg string, args ...any) {
			t.Fatalf("database phase refused boot: %s %v", msg, args)
		},
	}}
	app.databaseAndConcepts()
	if app.engine != nil {
		t.Fatal("test unexpectedly entered the engine phase")
	}
	detail, err := app.registry.Get("v1:fylo:applicationDetail")
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Relationships) != 1 || detail.Relationships[0].TargetConcept != "v1:wholesale:application" {
		t.Fatalf("unresolved product relationship: %+v", detail.Relationships)
	}
	if _, err := app.registry.Get("v1:wholesale:application"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"reviews", "wholesale"} {
		if enabled, declared := memqldsl.PackDefaults()[name]; !declared || enabled {
			t.Fatalf("pack %s must remain declared disabled, got %v/%v", name, enabled, declared)
		}
	}
	// The analyzer may anchor again in this same process; no double registration
	// or implicit enablement may result from moving the first registration early.
	app.anchorStorefrontPacks()
	if memqldsl.PackDefaultEnabled("reviews") || memqldsl.PackDefaultEnabled("wholesale") {
		t.Fatal("repeat anchor enabled a storefront pack")
	}
}
