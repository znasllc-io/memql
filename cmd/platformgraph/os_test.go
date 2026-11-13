package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/architecture/model"
)

func TestOSPassPreservesMovedSectionRequirements(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, osNavigation), `[{"id":"identity","name":"Identity","requires":"app:identity","sections":[{"id":"keys","name":"Keys","requires":"app:settings/keys","sourceRequires":"app:settings"}],"records":[]}]`)
	b := newBuilder()
	if err := osPass(b, root); err != nil {
		t.Fatal(err)
	}
	section, ok := b.nodes[model.OSSectionID("identity", "keys")]
	if !ok || section.Attrs["requires"] != "app:settings/keys" || section.Attrs["sourceRequires"] != "app:settings" {
		t.Fatalf("moved section lost its original grant conjunction: %+v", section)
	}
	if _, err := b.graph(); err != nil {
		t.Fatal(err)
	}
}

func TestOSPassStillRefusesUnknownNavigationFields(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, osNavigation), `[{"id":"identity","name":"Identity","requires":"app:identity","sections":[{"id":"keys","name":"Keys","sourceRequires":"app:settings","unknownRequirement":"app:other"}],"records":[]}]`)
	if err := osPass(newBuilder(), root); err == nil || !strings.Contains(err.Error(), "unknownRequirement") {
		t.Fatalf("unknown navigation field was not refused: %v", err)
	}
}
