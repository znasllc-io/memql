package storagequota

import (
	"context"
	"strings"
	"testing"
)

const engineObject = "packages/builds/2dabee4b/storefront/output.tar.gz"

// An oversized site build is stored by the workbench node, acting for the
// engine. That write is cluster-owned (an empty owner), and it must reserve
// capacity without any user row to resolve.
func TestAClusterOwnedWritePlansWithNoUserRow(t *testing.T) {
	s, _ := fixture(t)
	g := guard(t, s, engineObject, "")
	defer g.Close()
	must(t, g.Plan(context.Background(), 40<<20))
}

// The name of a node ("workbench-build:<node>") is not a user, and a write
// charged to one is refused: this is the refusal a Fylo storefront deploy hit
// in the cloud when the quota took the engine's own name for an owner. The
// callers that act for the engine are expected to arrive here as the cluster
// (integrations/azureblob quotaOwner), never by name.
func TestANodeNameIsNotAUserLookup(t *testing.T) {
	s, _ := fixture(t)
	g, err := s.Guard(context.Background(), "account", engineObject, "workbench-build:workbench-5fdd9c4d6b-26gcp")
	if err == nil {
		defer g.Close()
		err = g.Plan(context.Background(), 40<<20)
	}
	if err == nil || !strings.Contains(err.Error(), "cannot resolve storage owner") {
		t.Fatalf("a node name was accepted as an owner: %v", err)
	}
}
