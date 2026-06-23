package memql

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// queryRoutingResolver is a voiceParticipantResolver that records every query
// it receives and returns a per-query canned bundle, so a single fake can back
// the GA-id resolve AND a follow-up agent-row read in one test. canonicalize
// prefixes a deterministic partition (mirrors the real auto-canonicalize).
type queryRoutingResolver struct {
	mu       sync.Mutex
	queries  []string
	bySubstr map[string]*memqlv1.GraphBundle // returned when the query contains the key
	execErr  error
	// skillBundles maps a sorted skillId set (joined by ",") to the bundle
	// ResolveSkills returns; an empty map returns an empty bundle.
	skillBundles  map[string]memqlengine.SkillBundle
	resolveErr    error
	resolvedCalls [][]string
}

func (r *queryRoutingResolver) CanonicalizeIdValue(_ context.Context, value, conceptType string) (string, error) {
	if strings.HasPrefix(value, "standard:") {
		return value, nil
	}
	return "standard:" + conceptType + ":" + value, nil
}

func (r *queryRoutingResolver) Execute(_ context.Context, query string) (*memqlengine.ExecuteResult, error) {
	r.mu.Lock()
	r.queries = append(r.queries, query)
	r.mu.Unlock()
	if r.execErr != nil {
		return nil, r.execErr
	}
	for substr, bundle := range r.bySubstr {
		if strings.Contains(query, substr) {
			return &memqlengine.ExecuteResult{Bundle: bundle}, nil
		}
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

func (r *queryRoutingResolver) ResolveSkills(_ context.Context, skillIds []string) (memqlengine.SkillBundle, error) {
	r.mu.Lock()
	r.resolvedCalls = append(r.resolvedCalls, append([]string(nil), skillIds...))
	r.mu.Unlock()
	if r.resolveErr != nil {
		return memqlengine.SkillBundle{}, r.resolveErr
	}
	key := strings.Join(skillIds, ",")
	if b, ok := r.skillBundles[key]; ok {
		return b, nil
	}
	return memqlengine.SkillBundle{}, nil
}

// agentSkillsBundle builds a agentById-style result node carrying a
// capabilities.skillIds list (the post-#158 shape the tool-scope path reads).
func agentSkillsBundle(agentId string, skillIds ...string) *memqlv1.GraphBundle {
	caps := map[string]any{"skillIds": toAny(skillIds)}
	payload, _ := structpb.NewStruct(map[string]any{"capabilities": caps})
	return &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{
		{Id: agentId, Concept: "v1:agents:agent", Payload: payload},
	}}
}

func toAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

func (r *queryRoutingResolver) lastQueries() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.queries...)
}

func gaParticipantBundle(agentId string) *memqlv1.GraphBundle {
	payload, _ := structpb.NewStruct(map[string]any{
		"participantType": "si",
		"agentId":         agentId,
	})
	return &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{
		{Id: "standard:v1:cognition:participant:ga", Concept: "v1:cognition:participant", Payload: payload},
	}}
}

// TestSkillIdsFromAgentRows covers the post-#158 row-shape handling: the
// agent's skill ids live under capabilities.skillIds and arrive as []any
// (JSON-decoded) or []string, possibly nested under a `payload` map (shaped
// results). A row whose capabilities block is absent/empty is a real empty
// answer (found=true, scope to nothing), and no rows at all means the lookup
// failed (found=false, callers fail open to the unscoped registry). The old
// reader looked at a flat payload.tools list -- empty on every post-migration
// assistant -- which (with the retired `?.` parse failure) produced the
// unscoped 517 (#1454).
func TestSkillIdsFromAgentRows(t *testing.T) {
	t.Run("json-decoded any list under capabilities", func(t *testing.T) {
		ids, found := skillIdsFromAgentRows([]map[string]any{
			{"id": "a1", "capabilities": map[string]any{"skillIds": []any{"todos", "notes", "", 42}}},
		})
		assert.True(t, found)
		assert.Equal(t, []string{"todos", "notes"}, ids, "non-strings and empties are dropped")
	})

	t.Run("capabilities nested under payload (shaped result)", func(t *testing.T) {
		ids, found := skillIdsFromAgentRows([]map[string]any{
			{"id": "a1", "payload": map[string]any{"capabilities": map[string]any{"skillIds": []string{"calendar"}}}},
		})
		assert.True(t, found)
		assert.Equal(t, []string{"calendar"}, ids)
	})

	t.Run("row without capabilities is a real empty answer", func(t *testing.T) {
		ids, found := skillIdsFromAgentRows([]map[string]any{{"id": "a1"}})
		assert.True(t, found, "the agent exists; it just declares no skills")
		assert.Empty(t, ids)
	})

	t.Run("no rows fails open", func(t *testing.T) {
		_, found := skillIdsFromAgentRows(nil)
		assert.False(t, found, "no row -> caller must fall back to the unscoped registry")
	})
}

// TestScopeSetFromSlugs pins the #1442 fail-open policy: the ONLY path that
// fails open to the unscoped 517-tool registry is a genuine lookup failure
// (found=false). A row found with a real tool list scopes to that list; a row
// found with NO tools scopes to the empty set and does NOT fall open. This is
// the privilege/latency footgun the regression rode: an authoritative empty
// answer was being treated like a lookup error.
func TestScopeSetFromSlugs(t *testing.T) {
	t.Run("real id with tools scopes to the slug-expanded set", func(t *testing.T) {
		// Unknown (non-capability) entries pass through verbatim, so the
		// expansion is exactly these two names.
		set, scoped := scopeSetFromSlugs([]string{"webSearch", "produceArtifact"}, true)
		assert.True(t, scoped, "row found -> scope is authoritative")
		assert.Equal(t, map[string]struct{}{
			"webSearch":       {},
			"produceArtifact": {},
		}, set)
	})

	t.Run("real id with empty tools scopes to empty -- does NOT fail open", func(t *testing.T) {
		set, scoped := scopeSetFromSlugs(nil, true)
		assert.True(t, scoped, "found=true is authoritative even with no tools")
		assert.Empty(t, set, "a GA with no tools scopes to nothing")
		// The handler gates on `scoped`, not on len(set): scoped=true with an
		// empty set means EVERY registry tool is filtered out (exposed_count=0),
		// NOT the full 517-tool surface.
		assert.NotNil(t, set, "empty-but-non-nil so the handler treats it as a real scope")
	})

	t.Run("genuine lookup failure fails open", func(t *testing.T) {
		set, scoped := scopeSetFromSlugs(nil, false)
		assert.False(t, scoped, "found=false -> fail open to the unscoped registry (loud WARN at the call site)")
		assert.Nil(t, set)
	})
}

// TestResolveGroupGAAgentIdVia pins the #1442 real-id resolve: the space's
// active group-GA participant's agentId is returned, the query filters on the
// CANONICALIZED space id, and a query error / empty result yields "" so the
// caller falls back to the wire id.
func TestResolveGroupGAAgentIdVia(t *testing.T) {
	ctx := context.Background()
	const realId = "v1:agents:agent:assistant-1c65cb0c"

	t.Run("resolves the GA participant's real agent id", func(t *testing.T) {
		fake := &queryRoutingResolver{bySubstr: map[string]*memqlv1.GraphBundle{
			"groupGAForSpace": gaParticipantBundle(realId),
		}}
		got := resolveGroupGAAgentIdVia(ctx, fake, "space-1", nil)
		assert.Equal(t, realId, got)
		require.NotEmpty(t, fake.lastQueries())
		// Participant rows carry the canonical space id; the resolve must
		// canonicalize the bare slug before filtering.
		assert.Contains(t, fake.lastQueries()[0], `"standard:v1:cognition:space:space-1"`)
	})

	t.Run("no GA participant -> empty (caller falls back to wire id)", func(t *testing.T) {
		fake := &queryRoutingResolver{} // default empty bundle
		assert.Equal(t, "", resolveGroupGAAgentIdVia(ctx, fake, "space-1", nil))
	})

	t.Run("query error -> empty", func(t *testing.T) {
		fake := &queryRoutingResolver{execErr: fmt.Errorf("db down")}
		assert.Equal(t, "", resolveGroupGAAgentIdVia(ctx, fake, "space-1", nil))
	})
}

// TestResolveAgentToolSlugsVia pins that the tool-scope read is keyed on the
// REAL agent id it is handed (the #1442 defect was that it was handed the
// placeholder), and that a query error fails open (found=false) while a clean
// read is authoritative.
func TestResolveAgentToolSlugsVia(t *testing.T) {
	ctx := context.Background()
	const realId = "v1:agents:agent:assistant-1c65cb0c"

	t.Run("queries the real id via agentById (not a placeholder, not retired ?. syntax)", func(t *testing.T) {
		fake := &queryRoutingResolver{}
		resolveAgentToolSlugsVia(ctx, fake, realId, nil)
		require.NotEmpty(t, fake.lastQueries())
		q := fake.lastQueries()[0]
		assert.Contains(t, q, `agentById(`, "must use the tested named query, not a raw from() select")
		assert.NotContains(t, q, "?.", "must NOT use the retired optional-chain syntax the parser rejects (#1454)")
		assert.Contains(t, q, realId, "the scope read must use the resolved real id")
		assert.NotContains(t, q, "-ga", "must NOT carry the <space>-ga placeholder")
	})

	t.Run("resolves skillIds -> tool slugs (the #1454 fix)", func(t *testing.T) {
		fake := &queryRoutingResolver{
			bySubstr: map[string]*memqlv1.GraphBundle{
				"agentById": agentSkillsBundle(realId, "todos", "notes"),
			},
			skillBundles: map[string]memqlengine.SkillBundle{
				"todos,notes": {ToolSlugs: []string{"todosCreate", "notesCreate"}},
			},
		}
		slugs, _, found := resolveAgentToolSlugsVia(ctx, fake, realId, nil)
		assert.True(t, found)
		assert.Equal(t, []string{"todosCreate", "notesCreate"}, slugs)
		require.Len(t, fake.resolvedCalls, 1)
		assert.Equal(t, []string{"todos", "notes"}, fake.resolvedCalls[0],
			"ResolveSkills must be handed the agent's capabilities.skillIds")
	})

	t.Run("row with no skills scopes to empty (does NOT fail open)", func(t *testing.T) {
		fake := &queryRoutingResolver{bySubstr: map[string]*memqlv1.GraphBundle{
			"agentById": agentSkillsBundle(realId), // capabilities present, skillIds empty
		}}
		slugs, _, found := resolveAgentToolSlugsVia(ctx, fake, realId, nil)
		assert.True(t, found, "found=true is authoritative even with no skills")
		assert.Empty(t, slugs)
		assert.Empty(t, fake.resolvedCalls, "no ResolveSkills call when there are no skill ids")
	})

	t.Run("query error fails open", func(t *testing.T) {
		fake := &queryRoutingResolver{execErr: fmt.Errorf("db down")}
		_, _, found := resolveAgentToolSlugsVia(ctx, fake, realId, nil)
		assert.False(t, found, "a genuine query error -> found=false -> caller fails open")
	})

	t.Run("skill resolution error fails open", func(t *testing.T) {
		fake := &queryRoutingResolver{
			bySubstr:   map[string]*memqlv1.GraphBundle{"agentById": agentSkillsBundle(realId, "todos")},
			resolveErr: fmt.Errorf("skill catalog down"),
		}
		_, _, found := resolveAgentToolSlugsVia(ctx, fake, realId, nil)
		assert.False(t, found, "a ResolveSkills error -> found=false -> caller fails open (never serves 0 off a botched resolve)")
	})
}

// TestResolveAgentSessionRecordVia pins #1387's half: persona is read off the
// agent row by the REAL resolved id, never the placeholder.
func TestResolveAgentSessionRecordVia(t *testing.T) {
	ctx := context.Background()
	const realId = "v1:agents:agent:assistant-1c65cb0c"

	t.Run("persona query is keyed on the real id via agentById", func(t *testing.T) {
		fake := &queryRoutingResolver{}
		resolveAgentSessionRecordVia(ctx, fake, realId)
		require.NotEmpty(t, fake.lastQueries())
		q := fake.lastQueries()[0]
		assert.Contains(t, q, `agentById(`, "persona read uses the tested named query (agentFull carries name/role/personality)")
		assert.NotContains(t, q, "?.", "must NOT use the retired optional-chain syntax (#1454)")
		assert.Contains(t, q, realId, "persona must be resolved by the real id, not the placeholder")
		assert.NotContains(t, q, "-ga")
	})

	t.Run("query error yields zero persona (best-effort)", func(t *testing.T) {
		fake := &queryRoutingResolver{execErr: fmt.Errorf("db down")}
		out := resolveAgentSessionRecordVia(ctx, fake, realId)
		assert.Equal(t, agentRecordFields{}, out)
	})
}
