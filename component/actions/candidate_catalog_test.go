package actions

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestCandidateActionLoadUsesPrivateCatalogWithoutChangingLiveCatalog(t *testing.T) {
	const action = `use capabilities.fs.{ readFile }
action candidateRead {
  capability readFile(path: "file.txt")
}
`
	// This call is valid under the live contract. A candidate that adds a
	// required argument must be judged using its own contract instead.
	live := DefaultCatalog()
	if _, err := LoadSource(action, "candidate/actions.memql"); err != nil {
		t.Fatalf("live contract precondition: %v", err)
	}
	for _, required := range []bool{false, true} {
		schema := "path string @required\n"
		if required {
			schema += "scope string @required\n"
		}
		tree := fstest.MapFS{
			"candidate/memql.toml":         {Data: []byte("memql = \"1.0\"\nedition = \"2026\"\n")},
			"candidate/capabilities.memql": {Data: []byte("@sideEffect(\"read\")\ncapability fs.readFile { args {\n" + schema + "} }\n")},
			"candidate/actions.memql":      {Data: []byte(action)},
		}
		catalog, err := LoadCatalogFromFS(tree)
		if err != nil {
			t.Fatal(err)
		}
		count, err := NewRegistry().LoadFromFSWithCatalog(tree, catalog)
		if required {
			if err == nil || !strings.Contains(err.Error(), "missing required arg") || !strings.Contains(err.Error(), "scope") {
				t.Fatalf("candidate ignored its own changed contract: count=%d err=%v", count, err)
			}
		} else if err != nil || count != 1 {
			t.Fatalf("valid candidate: count=%d err=%v", count, err)
		}
		if DefaultCatalog() != live {
			t.Fatal("candidate replaced the process catalog")
		}
		if _, err := LoadSource(action, "candidate/actions.memql"); err != nil {
			t.Fatalf("candidate changed the live action contract: %v", err)
		}
	}
}
