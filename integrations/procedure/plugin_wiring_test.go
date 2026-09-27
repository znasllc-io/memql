package procedure

import (
	"testing"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/memql"
)

// plugin_wiring_test.go -- the registered factory wires what every node needs
// without app/ doing anything: the ladder's lock handle (review finding C1).

func TestThePlugInTakesTheLadderLockOnTheDirectEndpoint(t *testing.T) {
	var factory func(memql.PluginContext) (memql.IntegrationProvider, error)
	for _, p := range memql.RegisteredPlugins() {
		if p.Name == integrationName {
			factory = p.Factory
		}
	}
	if factory == nil {
		t.Fatal("the procedure plug-in is not registered")
	}
	direct, pooled := &bun.DB{}, &bun.DB{}
	for name, tc := range map[string]struct {
		direct, pooled func() *bun.DB
		want           *bun.DB
	}{
		"the direct endpoint, which a session lock needs": {func() *bun.DB { return direct }, func() *bun.DB { return pooled }, direct},
		"the pool, for a context with no direct getter":   {nil, func() *bun.DB { return pooled }, pooled},
	} {
		// The engine is only held, never called, while the factory builds.
		provider, err := factory(memql.PluginContext{
			Engine: struct{ memql.IntegrationEngineAccess }{}, DirectBunDB: tc.direct, BunDB: tc.pooled,
		})
		if err != nil {
			t.Fatalf("%s: factory: %v", name, err)
		}
		i, ok := provider.(*Integration)
		if !ok {
			t.Fatalf("%s: the factory built a %T", name, provider)
		}
		if !i.LadderLockInstalled() {
			t.Fatalf("%s: the factory installed no handle for the ladder lock, so every move runs unlocked", name)
		}
		if got := i.lockDB(); got != tc.want {
			t.Fatalf("%s: the ladder lock is taken on %p, want %p", name, got, tc.want)
		}
	}
	if newTestIntegration(newFakeEngine()).LadderLockInstalled() {
		t.Fatal("an integration built by New has a lock handle nobody gave it")
	}
}
