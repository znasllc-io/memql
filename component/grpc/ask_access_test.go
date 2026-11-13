package memql

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	"google.golang.org/grpc/codes"
)

type askAccessCatalog struct{}

func (askAccessCatalog) Rank(slug string) (int, bool) {
	switch slug {
	case "owner":
		return 400, true
	case "fylo-moderator":
		return 20, true
	}
	return 0, false
}
func (askAccessCatalog) Name(slug string) string { return slug }
func (askAccessCatalog) Holds(slug, verb, resource string) bool {
	return slug == "owner" || slug == "fylo-moderator" && verb == auth.VerbRead && resource == auth.ResourceData
}
func (askAccessCatalog) Grants(string) []auth.VerbResource { return nil }
func (askAccessCatalog) Scope(slug string) string {
	if slug == "fylo-moderator" {
		return "fylo"
	}
	return ""
}
func (c askAccessCatalog) Active(slug string) bool { _, ok := c.Rank(slug); return ok }

type askAccessGrants struct {
	user, group                            []auth.Grant
	groups                                 []string
	reads                                  int
	userError, groupError, membershipError error
}

func (g *askAccessGrants) ActiveGrantsForUser(context.Context, string) ([]auth.Grant, error) {
	g.reads++
	return g.user, g.userError
}
func (g *askAccessGrants) ActiveGrantsForGroups(context.Context, []string) ([]auth.Grant, error) {
	return g.group, g.groupError
}
func (g *askAccessGrants) ActiveGroupIdsForUser(ctx context.Context, _ string) []string {
	auth.NoteReadFailure(ctx, g.membershipError)
	return g.groups
}

func installAskAccessFixture(t *testing.T) *askAccessGrants {
	t.Helper()
	oldCatalog, oldGrants, oldMembers := auth.InstalledCapabilityCatalog(), auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	t.Cleanup(func() {
		auth.SetCapabilityCatalog(oldCatalog)
		auth.SetGrantSource(oldGrants)
		auth.SetMembershipSource(oldMembers)
	})
	g := &askAccessGrants{}
	auth.SetCapabilityCatalog(askAccessCatalog{})
	auth.SetGrantSource(g)
	auth.SetMembershipSource(g)
	return g
}

type askAccessModel struct {
	calls    atomic.Int32
	contexts chan context.Context
}

func (m *askAccessModel) CallChat(ctx context.Context, _ []common.ChatMessage) (string, error) {
	m.calls.Add(1)
	if m.contexts != nil {
		m.contexts <- ctx
	}
	return "fixture answer", nil
}
func (m *askAccessModel) CallChatStream(context.Context, []common.ChatMessage) (<-chan common.StreamChunk, error) {
	m.calls.Add(1)
	result := make(chan common.StreamChunk, 1)
	result <- common.StreamChunk{Content: "fixture answer", Done: true}
	close(result)
	return result, nil
}
func (m *askAccessModel) ResolveFor(context.Context, airoute.ResolveRequest) (memqlengine.ResolvedProvider, error) {
	return memqlengine.ResolvedProvider{Client: m}, nil
}

func askAccessSession(t *testing.T, actor *auth.AccessContext) (*streamSession, *captureStream, *askAccessModel) {
	t.Helper()
	model := &askAccessModel{}
	engine := &memqlengine.MemQLEngine{}
	engine.SetAIResolver(model)
	stream := newCaptureStream(t)
	// The stream may predate RotateAuth; only the current session actor counts.
	stream.ctx = auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "old-owner", Role: auth.RoleOwner})
	return &streamSession{service: &service{engine: engine, logger: testLogger()}, stream: stream, logger: testLogger(), access: actor, accessLoaded: true, credentialClass: auth.ForwardedClassUser, badgeStamped: true}, stream, model
}

func driveAskAccess(t *testing.T, session *streamSession, id string, streaming bool, conversation string) {
	t.Helper()
	msg := &memqlv1.AiChatMsg{RequestId: id, Stream: streaming, ConversationId: conversation, Messages: []*memqlv1.AiChatMessage{{Role: "user", Content: "fixture question"}}}
	envelope := &memqlv1.MemqlClientMessage{MessageId: "envelope-" + id, Payload: &memqlv1.MemqlClientMessage_AiChat{AiChat: msg}}
	require.NoError(t, session.handleAiChat(envelope, msg))
}

func requireAskAccessRefusal(t *testing.T, stream *captureStream, id string) {
	t.Helper()
	response := stream.lastSent()
	require.NotNil(t, response)
	require.Equal(t, "envelope-"+id, response.GetCorrelateTo())
	require.Equal(t, id, response.GetQueryError().GetRequestId())
	require.Equal(t, codes.PermissionDenied.String(), response.GetQueryError().GetError().GetCode())
}

func waitAskAccessResult(t *testing.T, stream *captureStream, id string) {
	t.Helper()
	require.Eventually(t, func() bool {
		response := stream.lastSent()
		return response != nil && response.GetAiChatResult().GetRequestId() == id
	}, time.Second, time.Millisecond)
}

func TestAskAccessRefusesEveryChatFormWithoutGrant(t *testing.T) {
	installAskAccessFixture(t)
	for _, tc := range []struct {
		name  string
		actor *auth.AccessContext
	}{
		{"missing", nil},
		{"blank-person", &auth.AccessContext{Role: auth.RoleOwner}},
		{"anonymous", auth.AnonymousActor()},
		{"connector", auth.ConnectorActor("fixture")},
		{"scoped-business-role", &auth.AccessContext{UserId: "moderator", Role: "fylo-moderator"}},
	} {
		for _, streaming := range []bool{false, true} {
			for _, conversation := range []string{"", "private-conversation"} {
				t.Run(fmt.Sprintf("%s/stream=%t/conversation=%s", tc.name, streaming, conversation), func(t *testing.T) {
					session, stream, model := askAccessSession(t, tc.actor)
					driveAskAccess(t, session, "denied", streaming, conversation)
					requireAskAccessRefusal(t, stream, "denied")
					require.Zero(t, model.calls.Load(), "no direct model call can precede the gate")
				})
			}
		}
	}
}

func TestAskAccessAllowsAuthorizedDirectChatAndTrustedSystemPolicy(t *testing.T) {
	installAskAccessFixture(t)
	for _, actor := range []*auth.AccessContext{{UserId: "owner", Role: auth.RoleOwner}, auth.SystemActor("fixture")} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", actor.UserId, streaming), func(t *testing.T) {
				session, stream, model := askAccessSession(t, actor)
				driveAskAccess(t, session, "allowed", streaming, "")
				waitAskAccessResult(t, stream, "allowed")
				require.Equal(t, int32(1), model.calls.Load())
			})
		}
	}
}

func TestAskAccessReadsRevocationsOnTheSameSession(t *testing.T) {
	g := installAskAccessFixture(t)
	allow := auth.Grant{Verb: auth.VerbRead, Resource: "app:ask", Effect: "allow"}
	deny := auth.Grant{Verb: auth.VerbRead, Resource: "app:ask", Effect: "deny"}
	session, stream, model := askAccessSession(t, &auth.AccessContext{UserId: "moderator", Role: "fylo-moderator"})
	g.groups, g.group = []string{"business-group"}, []auth.Grant{allow}
	driveAskAccess(t, session, "group-allowed", false, "")
	waitAskAccessResult(t, stream, "group-allowed")
	g.groups = nil
	driveAskAccess(t, session, "membership-revoked", true, "")
	requireAskAccessRefusal(t, stream, "membership-revoked")
	g.user = []auth.Grant{allow}
	driveAskAccess(t, session, "person-allowed", true, "")
	waitAskAccessResult(t, stream, "person-allowed")
	g.user = []auth.Grant{deny}
	driveAskAccess(t, session, "person-denied", false, "")
	requireAskAccessRefusal(t, stream, "person-denied")
	require.Equal(t, 4, g.reads)
	require.Equal(t, int32(2), model.calls.Load())
}

func TestAskAccessDiscardsAStreamGrantMemoBeforeLiveDenial(t *testing.T) {
	g := installAskAccessFixture(t)
	session, stream, model := askAccessSession(t, &auth.AccessContext{UserId: "owner", Role: auth.RoleOwner})
	stream.ctx = auth.ContextWithGrantMemo(auth.ContextWithAccess(stream.ctx, session.access))
	subject, ok := auth.SubjectFromContext(stream.ctx)
	require.True(t, ok)
	require.True(t, auth.CapableFor(stream.ctx, subject, auth.VerbRead, "app:ask"))
	g.user = []auth.Grant{{Verb: auth.VerbRead, Resource: "app:ask", Effect: "deny"}}
	// The inherited memo really is stale, so this tests refresh rather than
	// merely making two unrelated authorization calls with fresh contexts.
	require.True(t, auth.CapableFor(stream.ctx, subject, auth.VerbRead, "app:ask"))
	driveAskAccess(t, session, "fresh-deny", false, "")
	requireAskAccessRefusal(t, stream, "fresh-deny")
	require.Equal(t, 2, g.reads)
	require.Zero(t, model.calls.Load())
}

func TestAskAccessLocalChatCarriesRotatedActorAndAuthority(t *testing.T) {
	g := installAskAccessFixture(t)
	g.user = []auth.Grant{{Verb: auth.VerbRead, Resource: "app:ask", Effect: "allow"}}
	actor := &auth.AccessContext{UserId: "moderator", Role: "fylo-moderator"}
	session, stream, model := askAccessSession(t, actor)
	oldAuthority, err := auth.ForwardedAuthorityForUser(&auth.AccessContext{UserId: "old-owner", Role: auth.RoleOwner}, auth.ForwardedClassUser, "", time.Time{}, time.Now())
	require.NoError(t, err)
	stream.ctx = auth.ContextWithForwardedAuthority(stream.ctx, oldAuthority)
	model.contexts = make(chan context.Context, 1)
	driveAskAccess(t, session, "current-actor", false, "")
	waitAskAccessResult(t, stream, "current-actor")
	ctx := <-model.contexts
	gotActor, ok := auth.AccessFromContext(ctx)
	require.True(t, ok)
	require.Same(t, actor, gotActor)
	authority, ok := auth.ForwardedAuthorityFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, actor.UserId, authority.Subject)
	require.Equal(t, actor.Role, authority.Role)
}

func TestAskAccessFailsClosedWhenAuthorizationReadsFail(t *testing.T) {
	for _, kind := range []string{"person-grants", "group-grants", "memberships"} {
		t.Run(kind, func(t *testing.T) {
			g := installAskAccessFixture(t)
			g.groups = []string{"business-group"}
			err := errors.New("fixture authorization read failed")
			switch kind {
			case "person-grants":
				g.userError = err
			case "group-grants":
				g.groupError = err
			case "memberships":
				g.membershipError = err
			}
			session, stream, model := askAccessSession(t, &auth.AccessContext{UserId: "owner", Role: auth.RoleOwner})
			for _, streaming := range []bool{false, true} {
				driveAskAccess(t, session, kind, streaming, "")
				requireAskAccessRefusal(t, stream, kind)
			}
			require.Zero(t, model.calls.Load())
		})
	}
}

func driveAskAccessVoice(t *testing.T, session *streamSession, id string) {
	t.Helper()
	msg := &memqlv1.AskVoiceStartMsg{RequestId: id, ConversationId: "private-conversation", Voice: "female"}
	envelope := &memqlv1.MemqlClientMessage{MessageId: "envelope-" + id, Payload: &memqlv1.MemqlClientMessage_AskVoiceStart{AskVoiceStart: msg}}
	require.NoError(t, session.handleAskVoiceStart(envelope, msg))
}

func TestAskAccessVoiceUsesCurrentSessionAndFreshDenials(t *testing.T) {
	g := installAskAccessFixture(t)
	session, stream, _ := askAccessSession(t, &auth.AccessContext{UserId: "moderator", Role: "fylo-moderator"})
	// The inherited stream actor is an owner; the rotated session is not.
	driveAskAccessVoice(t, session, "voice-rotated")
	requireAskAccessRefusal(t, stream, "voice-rotated")
	session.access = &auth.AccessContext{UserId: "owner", Role: auth.RoleOwner}
	stream.ctx = auth.ContextWithGrantMemo(auth.ContextWithAccess(stream.ctx, session.access))
	subject, _ := auth.SubjectFromContext(stream.ctx)
	require.True(t, auth.CapableFor(stream.ctx, subject, auth.VerbRead, "app:ask"))
	g.user = []auth.Grant{{Verb: auth.VerbRead, Resource: "app:ask", Effect: "deny"}}
	driveAskAccessVoice(t, session, "voice-revoked")
	requireAskAccessRefusal(t, stream, "voice-revoked")
	g.user = nil
	g.userError = errors.New("fixture read failed")
	driveAskAccessVoice(t, session, "voice-read-failed")
	requireAskAccessRefusal(t, stream, "voice-read-failed")
}

func TestAskAccessRefusesBeforeDispatchToAvailableBFFPeer(t *testing.T) {
	if node.CompiledNodeType() != node.NodeTypeBFF {
		t.Skip("BFF forwarding is compiled out of this node type")
	}
	g := installAskAccessFixture(t)
	remote, address := startDisconnectPeer(t, node.NodeTypeAgent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	identity := &node.Identity{ID: "ask-access-bff", Type: node.NodeTypeBFF}
	peers := node.NewPeerManager(identity, testLogger())
	router := NewAiForwardRouter(peers, testLogger())
	dialer := node.NewWorkerDialer(identity, peers, nil, nil, []node.WorkerTarget{{NodeType: node.NodeTypeAgent, Address: address}}, testLogger())
	dialer.SetAiForwardResponseSink(router)
	dialer.Start(ctx)
	awaitDisconnectCondition(t, func() bool { return router.hasDispatchablePeer(node.NodeTypeAgent) }, "no local fixture peer")
	session, stream, model := askAccessSession(t, &auth.AccessContext{UserId: "moderator", Role: "fylo-moderator"})
	session.service.aiForwarder = router
	require.True(t, session.shouldProxyAI(node.NodeTypeAgent))
	for _, conversation := range []string{"", "private-conversation"} {
		driveAskAccess(t, session, "bff-denied"+conversation, true, conversation)
		requireAskAccessRefusal(t, stream, "bff-denied"+conversation)
	}
	driveAskAccessVoice(t, session, "voice-bff-denied")
	requireAskAccessRefusal(t, stream, "voice-bff-denied")
	select {
	case request := <-remote.requests:
		t.Fatalf("denied request reached peer: %s", request.RequestId)
	default:
	}
	require.Zero(t, model.calls.Load())
	// The same peer/stream is usable after an actual grant; denial was not a
	// missing model or unavailable transport masquerading as authorization.
	g.user = []auth.Grant{{Verb: auth.VerbRead, Resource: "app:ask", Effect: "allow"}}
	driveAskAccess(t, session, "bff-allowed", false, "")
	select {
	case <-remote.requests:
	case <-time.After(time.Second):
		t.Fatal("authorized request did not reach fixture peer")
	}
	driveAskAccessVoice(t, session, "voice-bff-allowed")
	select {
	case <-remote.requests:
	case <-time.After(time.Second):
		t.Fatal("authorized voice request did not reach fixture peer")
	}
	close(remote.drop)
}
