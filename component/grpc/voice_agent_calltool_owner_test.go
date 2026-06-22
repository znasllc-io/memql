package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/common"
)

// spaceOwnerBundle builds a querySpaceMeta-style result node carrying the
// spaceFull payload's ownerUserId (the field #1503 added to the shape so the
// owner reaches the voice CallTool auto-injection layer).
func spaceOwnerBundle(spaceId, ownerUserId string) *memqlv1.GraphBundle {
	payload, _ := structpb.NewStruct(map[string]any{"ownerUserId": ownerUserId})
	return &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{
		{Id: spaceId, Concept: "v1:cognition:space", Payload: payload},
	}}
}

// resolveVoiceSpaceOwnerVia is the resolver helper that backs the @autoInjected
// ownerUserId default on the realtime voice CallTool proxy hop. The voice-agent
// authenticates as a service identity, so the human owner has to be resolved
// from the bound space's ownerUserId rather than actor.userId.
func TestResolveVoiceSpaceOwnerVia_FromSpaceMeta(t *testing.T) {
	fake := &queryRoutingResolver{
		bySubstr: map[string]*memqlv1.GraphBundle{
			"querySpaceMeta": spaceOwnerBundle("standard:v1:cognition:space:demo", "user-jose"),
		},
	}
	owner := resolveVoiceSpaceOwnerVia(context.Background(), fake, "demo")
	assert.Equal(t, "user-jose", owner)

	// It must canonicalize the bare slug + read through querySpaceMeta.
	require.NotEmpty(t, fake.queries)
	assert.Contains(t, fake.queries[0], "querySpaceMeta")
	assert.Contains(t, fake.queries[0], "standard:v1:cognition:space:demo")
}

func TestResolveVoiceSpaceOwnerVia_MissReturnsEmpty(t *testing.T) {
	// No matching bundle (space row absent) -> empty so the caller leaves the
	// default unset rather than stamping a wrong owner.
	fake := &queryRoutingResolver{bySubstr: map[string]*memqlv1.GraphBundle{}}
	assert.Equal(t, "", resolveVoiceSpaceOwnerVia(context.Background(), fake, "demo"))

	// Empty space id -> no engine call, empty result.
	assert.Equal(t, "", resolveVoiceSpaceOwnerVia(context.Background(), fake, "  "))

	// Nil engine -> empty result, never panics.
	assert.Equal(t, "", resolveVoiceSpaceOwnerVia(context.Background(), nil, "demo"))
}

func TestResolveVoiceSpaceOwnerVia_QueryErrorReturnsEmpty(t *testing.T) {
	fake := &queryRoutingResolver{execErr: assertAnError}
	assert.Equal(t, "", resolveVoiceSpaceOwnerVia(context.Background(), fake, "demo"))
}

// assertAnError is a throwaway non-nil error for the query-error path.
var assertAnError = &simpleErr{"boom"}

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }

// TestVoiceCallToolDefaultsVia_StampsOwnerAcrossProxyHop is the cross-node
// proxy-path regression guard for #1503. The realtime voice CallTool is proxied
// bff->agent; the agent-node session has NO local voice scope, so the human
// owner can only come from the scope threaded on the CallToolMsg
// (VoiceAgentScopeId). This test exercises that hop: given ONLY the threaded
// space id (the exact shape stampVoiceAgentScopeOnCallTool produces), the
// resolved ToolDefaults must carry a non-empty ownerUserId (= the space owner)
// + spaceId so produceArtifact's @autoInjected fields can be stamped and the
// plan mints.
//
// It FAILS against current main (which threads no owner across the hop and sets
// no ToolDefaults at all on the agent-node CallTool path -> empty ownerUserId ->
// "requires 'ownerUserId' field in argument") and PASSES with the fix.
func TestVoiceCallToolDefaultsVia_StampsOwnerAcrossProxyHop(t *testing.T) {
	fake := &queryRoutingResolver{
		bySubstr: map[string]*memqlv1.GraphBundle{
			"querySpaceMeta":       spaceOwnerBundle("standard:v1:cognition:space:demo", "user-jose"),
			"queryGroupGAForSpace": gaParticipantBundle("v1:agents:agent:sofia"),
		},
	}

	// The bff stamped ONLY the space scope on the proxied CallToolMsg (the GA
	// agent id is NOT threaded on CallToolMsg -- the agent node resolves it).
	msg := &memqlv1.CallToolMsg{
		Name:              "produceArtifact",
		VoiceAgentScopeId: "demo",
	}

	ctx := voiceCallToolDefaultsVia(context.Background(), fake, msg, nil)
	defaults := common.ToolDefaultsFromContext(ctx)
	require.NotNil(t, defaults, "voice CallTool hop must set tool defaults on ctx")
	assert.Equal(t, "user-jose", defaults["ownerUserId"],
		"ownerUserId must resolve to the space owner across the bff->agent proxy hop (#1503)")
	assert.Equal(t, "demo", defaults["spaceId"])
	assert.Equal(t, "v1:agents:agent:sofia", defaults["agentId"])
}

// TestVoiceCallToolDefaultsVia_NoOpForBrowserCall asserts the no-op guard: a
// plain (non-voice) CallTool carries no VoiceAgentScopeId, so the ctx is left
// untouched and the hot browser path pays no space lookup.
func TestVoiceCallToolDefaultsVia_NoOpForBrowserCall(t *testing.T) {
	fake := &queryRoutingResolver{}
	base := context.Background()
	ctx := voiceCallToolDefaultsVia(base, fake, &memqlv1.CallToolMsg{Name: "uiClick"}, nil)
	assert.Nil(t, common.ToolDefaultsFromContext(ctx), "no voice scope -> no tool defaults stamped")
	assert.Empty(t, fake.queries, "no engine reads on the non-voice path")
}
