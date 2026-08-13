package harness

// planner_adapters_declared_fields_3696_test.go pins what EngineAgentFactory is
// allowed to write onto an agent row: only fields v1:agents:agent DECLARES.
//
// It used to write TOP-LEVEL `knowledgeDomains` and `tools`, the flat capability
// surface #158 replaced with capabilities.skillIds. The top level of a concept
// is closed (`additionalProperties: false` has always been emitted there), so
// both mutations were refused -- CreateAgent swallowing the refusal as
// "best effort" and taking the lineage stamp down with it, UpgradeAgent
// returning it, which was its entire body (memql#3696).
//
// The assertion is against the REAL concept rather than a hardcoded key list,
// so it pins the CLASS: any future write of a field the concept does not
// declare fails here, not at runtime, and it keeps holding when the concept's
// field set moves.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// recordingExecutor captures every query the factory issues.
type recordingExecutor struct{ queries []string }

func (r *recordingExecutor) Execute(_ context.Context, query string) (any, error) {
	r.queries = append(r.queries, query)
	return map[string]any{}, nil
}

// agentDeclaredFields reads the real v1:agents:agent concept and returns its
// declared TOP-LEVEL field names.
func agentDeclaredFields(t *testing.T) map[string]bool {
	t.Helper()
	// component/harness is a module; the tree sits two levels up.
	path := filepath.Join("..", "..", "dsl", "agents", "concepts.memql")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	// The file declares several concepts; ParseConceptMemQL takes one, so cut
	// the `concept agent { ... }` block out by brace depth.
	block, err := conceptBlock(string(src), "agent")
	if err != nil {
		t.Fatalf("locating `concept agent`: %v", err)
	}
	c, err := memorynodes.ParseConceptMemQL([]byte(block), "v1/agents/agent")
	if err != nil {
		t.Fatalf("parse concept agent: %v", err)
	}
	declared := map[string]bool{}
	for _, f := range c.DeclaredFields() {
		declared[f] = true
	}
	if len(declared) == 0 {
		t.Fatal("v1:agents:agent declares no fields; the concept shape changed and this guard " +
			"now permits everything")
	}
	return declared
}

var conceptOpenRe = regexp.MustCompile(`(?m)^concept[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

// conceptBlock extracts `concept <name> { ... }` plus the annotation lines
// directly above it, by brace depth.
func conceptBlock(src, name string) (string, error) {
	for _, m := range conceptOpenRe.FindAllStringSubmatchIndex(src, -1) {
		if src[m[2]:m[3]] != name {
			continue
		}
		depth := 0
		for i := m[1] - 1; i < len(src); i++ {
			switch src[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					return src[m[0] : i+1], nil
				}
			}
		}
		return "", fmt.Errorf("unterminated block")
	}
	return "", fmt.Errorf("no `concept %s` in source", name)
}

var mutationPayloadRe = regexp.MustCompile(`payload:(\{.*\})\)`)

// payloadKeys pulls the top-level keys out of a `mutation updateAgent(... payload:{...})`.
func payloadKeys(t *testing.T, query string) []string {
	t.Helper()
	m := mutationPayloadRe.FindStringSubmatch(query)
	if m == nil {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(m[1]), &payload); err != nil {
		t.Fatalf("payload is not JSON (%v): %s", err, m[1])
	}
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	return keys
}

// TestCreateAgentWritesOnlyDeclaredFields is the memql#3696 lock for the create
// path. Before the fix the follow-up updateAgent carried `knowledgeDomains` and
// `tools`, so it was refused and the lineage stamp -- the Plan attribution the
// roster reads -- never landed.
func TestCreateAgentWritesOnlyDeclaredFields(t *testing.T) {
	declared := agentDeclaredFields(t)
	exec := &recordingExecutor{}
	f := NewEngineAgentFactory(exec, nil)

	if _, err := f.CreateAgent(context.Background(), ComposedAgent{
		Name:             "Specialist",
		RoleSlug:         "specialist",
		Description:      "d",
		SystemPrompt:     "p",
		Tools:            []string{"workbenchHost"},
		KnowledgeDomains: []string{"workbench"},
	}, "user-1", "plan-1"); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	sawUpdate := false
	for _, q := range exec.queries {
		if !strings.Contains(q, "mutation updateAgent") {
			continue
		}
		sawUpdate = true
		for _, k := range payloadKeys(t, q) {
			if !declared[k] {
				t.Errorf("updateAgent payload carries %q, which v1:agents:agent does not declare. "+
					"The top level of a concept is CLOSED, so the whole mutation is refused -- taking "+
					"every other field in the payload down with it (memql#3696).\n  %s", k, q)
			}
		}
	}
	if !sawUpdate {
		t.Fatal("CreateAgent issued no updateAgent; the lineage stamp is what that call is for, " +
			"and this guard now checks nothing")
	}
}

// TestUpgradeAgentWritesNothing pins the other half. UpgradeAgent's only write
// was the retired flat surface, so with it gone the function issues no mutation
// at all -- it re-warms the capability embedding, which is what the roster
// actually reads.
//
// If a future change gives it a write again, that write has to be checked
// against the concept the way the create path is above; this failing is the
// prompt to do that rather than to delete the assertion.
func TestUpgradeAgentWritesNothing(t *testing.T) {
	exec := &recordingExecutor{}
	f := NewEngineAgentFactory(exec, nil)

	if err := f.UpgradeAgent(context.Background(), "v1:agents:agent:a1",
		[]string{"workbench"}, []string{"workbenchHost"}, "plan-1"); err != nil {
		t.Fatalf("UpgradeAgent must succeed: every call returned an error while its only mutation "+
			"wrote fields the concept does not declare (memql#3696): %v", err)
	}
	if len(exec.queries) != 0 {
		t.Errorf("UpgradeAgent issued %d mutation(s); it writes nothing to the agent row: %v",
			len(exec.queries), exec.queries)
	}
}
