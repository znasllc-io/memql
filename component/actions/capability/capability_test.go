package capability

import "testing"

func TestCapabilityClassByNamespace(t *testing.T) {
	cases := []struct {
		cap     string
		class   string
		known   bool
		sideEff bool
	}{
		{"shell.exec", ClassExec, true, true},
		{"fs.readFile", ClassRead, true, false},
		{"fs.writeFile", ClassWrite, true, true},
		{"fs.remove", ClassWrite, true, true},
		{"http.get", ClassRead, true, false},
		{"http.post", ClassWrite, true, true},
		{"mcp.callTool", ClassExec, true, true},
		{"integration.github.tagRelease", ClassWrite, true, true},
		{"integration.auth.resolveUser", ClassRead, true, false},
		{"integration.workbench.teardownDirectory", ClassWrite, true, true},
		{"integration.rbac.governPrincipal", ClassRead, true, false},
		// Unrecognized -> not classifiable, conservatively not side-effecting.
		{"integration.unknown.thing", "", false, false},
		{"weird.verb", "", false, false},
	}
	for _, c := range cases {
		class, known := CapabilityClass(c.cap)
		if class != c.class || known != c.known {
			t.Errorf("CapabilityClass(%q) = (%q,%v), want (%q,%v)", c.cap, class, known, c.class, c.known)
		}
		if got := SideEffecting(c.cap); got != c.sideEff {
			t.Errorf("SideEffecting(%q) = %v, want %v", c.cap, got, c.sideEff)
		}
	}
}

func TestValidNamespace(t *testing.T) {
	for _, ok := range []string{"fs.readFile", "shell.exec", "http.get", "integration.x.y", "mcp.tool"} {
		if !ValidNamespace(ok) {
			t.Errorf("ValidNamespace(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"weird.thing", "graph.insert", "logic.call"} {
		if ValidNamespace(bad) {
			t.Errorf("ValidNamespace(%q) = true, want false", bad)
		}
	}
}

func TestBuiltinExecutorsScan(t *testing.T) {
	src := `@enabled
@executor("integration.workbench.teardownDirectory")
@args(profile="object")
builtin workbenchTeardownDirectory {
  planId string @required
}

@enabled
@executor("integration.auth.resolveUser")
builtin resolveUser {
  token string @required
}

builtin noExecutor {
  x string
}`
	got := builtinExecutors(src)
	if got["workbenchTeardownDirectory"] != "integration.workbench.teardownDirectory" {
		t.Errorf("teardown executor = %q", got["workbenchTeardownDirectory"])
	}
	if got["resolveUser"] != "integration.auth.resolveUser" {
		t.Errorf("resolveUser executor = %q", got["resolveUser"])
	}
	if _, ok := got["noExecutor"]; ok {
		t.Errorf("builtin with no preceding executor must not be mapped")
	}
}

func TestClassifierFromExecutors(t *testing.T) {
	cls := classifierFromExecutors(map[string]string{
		"workbenchTeardownDirectory": "integration.workbench.teardownDirectory",
		"resolveUser":                "integration.auth.resolveUser",
		"suggestNextVersion":         "integration.deployversion.suggestNextVersion",
	})
	if !cls("workbenchTeardownDirectory") {
		t.Error("teardownDirectory must classify as side-effecting")
	}
	if cls("resolveUser") {
		t.Error("resolveUser must classify as read (not side-effecting)")
	}
	if cls("suggestNextVersion") {
		t.Error("suggestNextVersion must classify as read (not side-effecting)")
	}
}

// DefaultClassifier scans the embedded tree; the known side-effecting builtin
// reached from a read context must classify as side-effecting.
func TestDefaultClassifierEmbedded(t *testing.T) {
	cls := DefaultClassifier()
	if !cls("workbenchTeardownDirectory") {
		t.Error("embedded classifier must mark workbenchTeardownDirectory side-effecting")
	}
	if cls("rbacGovernPrincipal") {
		t.Error("embedded classifier must mark rbacGovernPrincipal read-only")
	}
}
