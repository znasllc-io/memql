package local

import (
	"strings"
	"testing"
)

// TestLocalStaysOneEnvironment is the "make up is unchanged" gate (epic
// memql#3748 / memql#3766).
//
// # What the epic did, and what it deliberately did not do
//
// Two environments arrived as two NAMESPACES in the cloud cluster, each with a
// memql-environment ConfigMap carrying MEMQL_ENVIRONMENT and the schema search
// path that separates their data. The wiring that reads that ConfigMap is
// SHAPE, so it lives in base and every overlay renders it -- this one included.
// The VALUES are per-overlay, and this overlay supplies none.
//
// Local stays ONE environment. Development is not staging, and a second local
// namespace would make the inner loop slower to prove nothing.
//
// # Why an absent ConfigMap is the correct local configuration
//
// MEMQL_DB_SEARCH_PATH unset means "no search path is applied at all", which is
// the single-environment case and exactly what every local cluster ran before
// this existed (component/database/search_path.go). So the local behaviour that
// must not change is produced by the ConfigMap NOT being here -- which makes
// its absence a thing worth asserting rather than a thing that merely happens
// to be true.
//
// # The failure this catches
//
// `optional: true` on the envFrom is the whole reason base can carry the wiring
// for an environment this overlay does not have. Drop it -- in base, in a
// component, in a patch here -- and every pod in the local cluster comes up
// CreateContainerConfigError, because it is waiting on a ConfigMap nobody
// creates. `make up` breaks completely, and it breaks for a change made in a
// file that has nothing to do with local development.
func TestLocalStaysOneEnvironment(t *testing.T) {
	rendered := render(t)
	docs := strings.Split(rendered, "\n---\n")

	// No environment ConfigMap: this cluster is one environment, and the engine
	// reads an unset search path as exactly that.
	for _, doc := range docs {
		if strings.Contains(doc, "kind: ConfigMap") &&
			strings.Contains(doc, "\n  name: memql-environment\n") {
			t.Error("the local overlay renders a memql-environment ConfigMap; local is ONE environment and " +
				"an unset MEMQL_DB_SEARCH_PATH is what says so")
		}
	}

	// Every node still references it, and optionally -- the reference is base's
	// shape, the optionality is what makes the shape survive an overlay with no
	// values for it.
	for _, node := range nodes {
		var found bool
		for _, doc := range docs {
			if !strings.Contains(doc, "kind: Deployment") ||
				!strings.Contains(doc, "\n  name: "+node+"\n") {
				continue
			}
			found = true
			if !strings.Contains(doc, "name: memql-environment") {
				t.Errorf("%s does not reference the memql-environment ConfigMap; base declares that wiring for "+
					"every node and an overlay must not drop it", node)
				continue
			}
			// `optional: true` is emitted on the line after the name, inside
			// the same configMapRef block. Matched by scanning lines rather
			// than by embedding the indentation in a literal: the renderer
			// normalises the leading whitespace, so a literal would be
			// asserting about kustomize's formatting rather than about the
			// flag, and would break on a renderer that indents differently.
			if !optionalFollowsName(doc, "memql-environment") {
				t.Errorf("%s references memql-environment without optional:true -- this overlay creates no such "+
					"ConfigMap, so that is every pod in the local cluster stuck on CreateContainerConfigError", node)
			}
		}
		if !found {
			t.Errorf("no Deployment named %s in the rendered overlay", node)
		}
	}
}

// optionalFollowsName reports whether every `name: <cm>` line in doc is
// immediately followed by `optional: true`.
func optionalFollowsName(doc, cm string) bool {
	lines := strings.Split(doc, "\n")
	var sawAny bool
	for i, line := range lines {
		if strings.TrimSpace(line) != "name: "+cm {
			continue
		}
		sawAny = true
		if i+1 >= len(lines) || strings.TrimSpace(lines[i+1]) != "optional: true" {
			return false
		}
	}
	return sawAny
}
