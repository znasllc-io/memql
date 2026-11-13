package memql

import "testing"

func TestOrganizationCollectionReadRequiresTheGuardedExecutor(t *testing.T) {
	for _, tc := range []struct {
		name, executor, kind, resource string
		want                           bool
	}{
		{"deployableDirectoryPage", "integration.packages.directoryPage", "builtin", "", true},
		{"deployableDirectoryPage", "integration.packages.directoryPage", "builtin", "deployables", true},
		{"deployableDirectoryPage", "integration.packages.directoryPage", "builtin", "sources", false},
		{"deployableDirectoryPage", "integration.packages.directoryPage", "builtin", "unknown", false},
		{"deployableDirectoryPage", "integration.custom.unscoped", "builtin", "deployables", false},
		{"otherDirectory", "integration.packages.directoryPage", "builtin", "deployables", false},
		{"siteStatistics", "integration.siteTraffic.statistics", "builtin", "", true},
		{"siteStatistics", "integration.custom.unscoped", "builtin", "", false},
		{"otherStatistics", "integration.siteTraffic.statistics", "builtin", "", false},
		{"siteStatistics", "integration.siteTraffic.statistics", "mutation", "", false},
	} {
		fn := &Function{Name: tc.name, Executor: tc.executor, FunctionKind: tc.kind}
		if got := organizationCollectionRead(fn, map[string]any{"resource": tc.resource}); got != tc.want {
			t.Errorf("%s/%s/%s resource %q: got %t, want %t", tc.name, tc.executor, tc.kind, tc.resource, got, tc.want)
		}
	}
	fn := &Function{Name: "deployableDirectoryPage", Executor: "integration.packages.directoryPage", FunctionKind: "builtin"}
	if organizationCollectionRead(fn, map[string]any{"resource": ActorReference{}}) {
		t.Fatal("a dynamic directory mode borrowed the deployables-only exception")
	}
}
