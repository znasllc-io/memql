package app

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestComposeIntegrationIsWiredFromTheTransport is a SOURCE assertion, and
// it is the shape this gap demanded (epic memql#4977).
//
// The Materializer's plug-in registers itself from `init()`, so its five
// capabilities resolve at boot and nothing fails loudly. Every existing
// gate was green over a build where NOTHING called the setters -- because
// a capability that returns a typed refusal is a capability that resolves,
// and "object storage is not configured on this node" is a perfectly
// well-formed answer. The feature was installed and inert.
//
// A behavioural test would need an App, an engine, a registry and a blob
// client, which is why one does not exist for the Library's own equivalent
// wiring either. What is cheap and what actually failed is this: the call
// site EXISTS and it is on the transport path that holds the blob client.
//
// If this test ever looks like busywork, the thing to remember is that its
// absence is exactly what let a fully-built app ship answering "storage is
// not configured" on a cluster that was configured.
func TestComposeIntegrationIsWiredFromTheTransport(t *testing.T) {
	src := readAppFile(t, "transport_bff.go")

	if !strings.Contains(src, "a.wireComposeIntegration(uploader, container)") {
		t.Fatal("transport_bff.go does not call wireComposeIntegration(uploader, container).\n" +
			"Without it the compose plug-in registers, its capabilities resolve, and EVERY\n" +
			"materialization answers \"object storage is not configured on this node\" --\n" +
			"on a cluster that is configured. The surface renders, so nothing looks broken.")
	}

	// THE SAME uploader AND container the Library's own mount takes. A
	// materialized file and an uploaded one landing in different
	// containers is a class of bug nobody finds by looking at either one.
	mountIdx := strings.Index(src, "a.mountLibraryArtifactEndpoints(uploader, container)")
	wireIdx := strings.Index(src, "a.wireComposeIntegration(uploader, container)")
	if mountIdx < 0 || wireIdx < 0 || wireIdx < mountIdx {
		t.Error("wireComposeIntegration should sit beside the Library's own mount, after it, " +
			"so the shared blob client and container are visibly the same pair")
	}
}

// TestComposeSettersAreAllCalledOrDeliberatelyAbsent holds the integration's
// exported setters against the wiring that installs them.
//
// A SETTER NOBODY CALLS IS A DEPENDENCY NOBODY SUPPLIES, and this package
// found that out the hard way: three of them were declared, documented, and
// called from nowhere. So the assertion is set equality against an explicit
// list, and adding a setter without wiring it fails here rather than in a
// cluster.
func TestComposeSettersAreAllCalledOrDeliberatelyAbsent(t *testing.T) {
	// SetNow is the exception and it is named rather than pattern-matched:
	// it injects a clock for tests, and nothing in the shell passes one.
	// Every app-level settings store in this tree takes the same shape.
	deliberatelyUnwired := map[string]string{
		"SetNow": "injects a clock; tests only, nothing in the shell passes one",
		"SetComposer": "the reasoning step's implementation. NOT wired in this epic -- " +
			"the compose prompt is a product concern and a node with none refuses a " +
			"materialization that supplied no draft, by name, rather than producing an " +
			"empty file",
	}

	declared := composeSettersInSource(t)
	if len(declared) == 0 {
		t.Fatal("found no Set* methods on integrations/compose, so this test asserts nothing")
	}

	wiring := readAppFile(t, "integrations_compose.go")
	for _, setter := range declared {
		if why, ok := deliberatelyUnwired[setter]; ok {
			if strings.Contains(wiring, "integ."+setter+"(") {
				t.Errorf("%s is listed as deliberately unwired (%q) and integrations_compose.go calls it; "+
					"remove the entry or the call", setter, why)
			}
			continue
		}
		if !strings.Contains(wiring, "integ."+setter+"(") {
			t.Errorf("integrations/compose declares %s and app/integrations_compose.go never calls it.\n"+
				"An unwired setter is a dependency nobody supplies, and the failure is a typed\n"+
				"refusal rather than a crash -- which is why it survives every other gate.\n"+
				"Wire it, or add it to deliberatelyUnwired with the reason.", setter)
		}
	}
}

// ---------------------------------------------------------------------------

var composeSetterPattern = regexp.MustCompile(`(?m)^func \(i \*Integration\) (Set[A-Za-z0-9_]+)\(`)

func composeSettersInSource(t *testing.T) []string {
	t.Helper()
	src := readRepoRelative(t, filepath.Join("integrations", "compose", "integration.go"))
	var out []string
	for _, m := range composeSetterPattern.FindAllStringSubmatch(src, -1) {
		out = append(out, m[1])
	}
	return out
}

func readAppFile(t *testing.T, name string) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate the app package directory")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(self), name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

func readRepoRelative(t *testing.T, rel string) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate the app package directory")
	}
	dir := filepath.Dir(self)
	for range 6 {
		if raw, err := os.ReadFile(filepath.Join(dir, rel)); err == nil {
			return string(raw)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not find %s walking up from the app package", rel)
	return ""
}
