package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// agentRowBundle builds the GraphBundle shape a plain `from(v1:agents:agent)
// select payload.X` query actually returns: NO shape template means
// ExecuteResult.OutputPayload() hands back the raw bundle, with the projected
// fields living in each node's Payload (applyPlanProjection writes them there).
// This is the shape the live engine produces -- the row-map shape the older
// tests fed the pure helpers never exercised it (#1387/#1448).
func agentRowBundle(id string, fields map[string]any) *memqlv1.GraphBundle {
	payload, _ := structpb.NewStruct(fields)
	return &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{
		{Id: id, Concept: "v1:agents:agent", Payload: payload},
	}}
}

// TestNormalizeResultRowsGraphBundle pins the #1387/#1448 root cause fix: a
// query result surfaces as a *GraphBundle, and normalizeResultRows MUST flatten
// it into the row-map shape every voice-agent extractor consumes. Before the
// fix this returned nil -> blank persona + tool-scope fail-open. The assertions
// below FAIL against the pre-fix code.
func TestNormalizeResultRowsGraphBundle(t *testing.T) {
	bundle := agentRowBundle("v1:agents:agent:assistant-1c65cb0c", map[string]any{
		"name":         "Assistant",
		"role":         "assistant",
		"personality":  "helpful",
		"audioControl": "always_on",
		"capabilities": map[string]any{"skillIds": []any{"todos", "notes"}},
	})

	rows := normalizeResultRows(bundle)
	require.Len(t, rows, 1, "a single-node bundle must flatten to exactly one row")
	row := rows[0]

	// Top-level hoist (so row[field] hits) ...
	assert.Equal(t, "Assistant", row["name"])
	assert.Equal(t, "assistant", row["role"])
	assert.Equal(t, "v1:agents:agent:assistant-1c65cb0c", row["id"])
	// ... and the nested payload fallback (so row["payload"][field] hits).
	inner, ok := row["payload"].(map[string]any)
	require.True(t, ok, "the nested payload map must be present for the fallback path")
	assert.Equal(t, "Assistant", inner["name"])

	// The downstream extractors must now resolve real values off the bundle.
	name, ok := extractFirstAgentField(bundle, "name")
	assert.True(t, ok)
	assert.Equal(t, "Assistant", name)

	ids, found := skillIdsFromAgentRows(normalizeResultRows(bundle))
	assert.True(t, found, "the agent row is present -> authoritative answer, not a fail-open")
	assert.Equal(t, []string{"todos", "notes"}, ids)
}

// TestResolveAgentSessionRecordViaBundleShape pins #1387: persona resolves REAL
// fields off the GraphBundle a real `select payload.X` query returns (not the
// blank zero value the row-map-only path produced). Fails against pre-fix code.
func TestResolveAgentSessionRecordViaBundleShape(t *testing.T) {
	const realId = "v1:agents:agent:assistant-1c65cb0c"
	fake := &queryRoutingResolver{bySubstr: map[string]*memqlv1.GraphBundle{
		"queryAgentById": agentRowBundle(realId, map[string]any{
			"name":         "Assistant",
			"role":         "assistant",
			"description":  "Your general assistant.",
			"personality":  "warm and concise",
			"audioControl": "always_on",
			"videoControl": "mirror_user",
		}),
	}}

	out := resolveAgentSessionRecordVia(context.Background(), fake, realId)
	assert.Equal(t, "Assistant", out.persona.name, "persona name must populate off the bundle")
	assert.Equal(t, "assistant", out.persona.role)
	assert.Equal(t, "Your general assistant.", out.persona.description)
	assert.Equal(t, "warm and concise", out.persona.personality)
	assert.Equal(t, "always_on", out.audioControl)
	assert.Equal(t, "mirror_user", out.videoControl)
}

// TestResolveAgentToolSlugsViaBundleShape pins #1448/#1454's local half: the
// tool-scope read resolves the GA's capabilities.skillIds off the bundle shape
// (authoritative found=true) and resolves those skills into tool slugs, instead
// of parsing zero rows and failing open to the 517-tool registry. Fails against
// pre-fix code (which used the retired `?.` syntax + read the empty
// payload.tools list).
func TestResolveAgentToolSlugsViaBundleShape(t *testing.T) {
	const realId = "v1:agents:agent:assistant-1c65cb0c"
	fake := &queryRoutingResolver{
		bySubstr: map[string]*memqlv1.GraphBundle{
			"queryAgentById": agentRowBundle(realId, map[string]any{
				"capabilities": map[string]any{"skillIds": []any{"todos", "notes"}},
			}),
		},
		skillBundles: map[string]memqlengine.SkillBundle{
			"todos,notes": {ToolSlugs: []string{"todosCreate", "notesCreate"}},
		},
	}

	slugs, _, found := resolveAgentToolSlugsVia(context.Background(), fake, realId, nil)
	assert.True(t, found, "the agent row resolved -> authoritative, NOT a fail-open to the full registry")
	assert.Equal(t, []string{"todosCreate", "notesCreate"}, slugs)
}

// TestVoiceAgentScopedToolNamesProxiedReceiver is the CLUSTER-AWARE test that
// the #1442 single-node tests never had (and why #1448 shipped broken). It
// models the proxied agent-node RECEIVER: the receiving session has NO locally
// bound voiceAgentSpaceId/voiceAgentGaAgentId (those live only on the bff
// session that handled VoiceAgentSessionStart). Scope must STILL be applied
// because the bff threaded the spaceId through the ListToolsMsg.
//
// Pre-fix (today's code) this path read s.voiceAgentSpaceId off the receiver
// session, found it empty, and failed open to the full 517-tool registry. The
// fix threads the scope through the request and resolves from THAT.
func TestVoiceAgentScopedToolNamesProxiedReceiver(t *testing.T) {
	const realId = "v1:agents:agent:assistant-1c65cb0c"
	const spaceId = "space-1"

	// One fake backs both reads the receiver issues: the GA-id resolve off the
	// threaded space, then the tool-surface read (queryAgentById -> skillIds ->
	// ResolveSkills) off the resolved real id.
	newFake := func() *queryRoutingResolver {
		return &queryRoutingResolver{
			bySubstr: map[string]*memqlv1.GraphBundle{
				"queryGroupGAForSpace": gaParticipantBundle(realId),
				"queryAgentById": agentRowBundle(realId, map[string]any{
					"capabilities": map[string]any{"skillIds": []any{"todos", "notes"}},
				}),
			},
			skillBundles: map[string]memqlengine.SkillBundle{
				"todos,notes": {ToolSlugs: []string{"todosCreate", "notesCreate"}},
			},
		}
	}

	t.Run("threaded spaceId scopes the surface even with no local session state", func(t *testing.T) {
		fake := newFake()
		// Empty local scope (this is the proxied receiver); spaceId arrives
		// ONLY via the threaded request.
		set, _, scoped := voiceAgentScopedToolNamesVia(context.Background(), fake, spaceId, "", nil)
		require.True(t, scoped, "scope must apply off the THREADED spaceId; failing open here is the #1448 bug")
		assert.Equal(t, map[string]struct{}{
			"todosCreate": {},
			"notesCreate": {},
		}, set, "the surface must be the GA's resolved tool list, not the full registry")
	})

	t.Run("no threaded scope and no local scope -> fail open (non-voice caller)", func(t *testing.T) {
		fake := newFake()
		_, _, scoped := voiceAgentScopedToolNamesVia(context.Background(), fake, "", "", nil)
		assert.False(t, scoped, "a caller with no bound scope at all keeps the unscoped registry")
	})
}

// TestStampVoiceAgentScopeOnListTools pins the bff SENDER half of the #1448
// threading: a voice-agent session stamps its bound (spaceId, gaAgentId) onto
// the ListToolsMsg before it is proxied; a non-voice session leaves it empty so
// the proxied request is unchanged for text/browser callers.
func TestStampVoiceAgentScopeOnListTools(t *testing.T) {
	t.Run("voice session stamps its bound scope", func(t *testing.T) {
		s := &streamSession{voiceAgentSpaceId: "space-1", voiceAgentGaAgentId: "v1:agents:agent:assistant-1c65cb0c"}
		msg := &memqlv1.ListToolsMsg{}
		s.stampVoiceAgentScopeOnListTools(msg)
		assert.Equal(t, "space-1", msg.GetVoiceAgentSpaceId())
		assert.Equal(t, "v1:agents:agent:assistant-1c65cb0c", msg.GetVoiceAgentGaAgentId())
	})

	t.Run("non-voice session leaves the scope empty", func(t *testing.T) {
		s := &streamSession{}
		msg := &memqlv1.ListToolsMsg{}
		s.stampVoiceAgentScopeOnListTools(msg)
		assert.Empty(t, msg.GetVoiceAgentSpaceId())
		assert.Empty(t, msg.GetVoiceAgentGaAgentId())
	})
}
