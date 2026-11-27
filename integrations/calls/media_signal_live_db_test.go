package calls

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	lkauth "github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// This fixture has an isolated native database and real local LiveKit. It is
// deliberately not browser-origin acceptance and does not change tester-code
// rules or the served site's accounts. Only its own native/media rows are used.
func nativeLocalSignalOptIn(t *testing.T) (MediaCredential, *lksdk.RoomServiceClient) {
	t.Helper()
	if os.Getenv("MEMQL_TEST_MEDIA_NATIVE_LOCAL") != "1" {
		t.Skip("opt in to isolated native+local provider signalling qualification")
	}
	dsn, err := url.Parse(os.Getenv("MEMQL_DATABASE_DSN"))
	if err != nil || dsn.Host != "127.0.0.1:55432" || !strings.HasPrefix(dsn.Path, "/callkey_media_guard_") {
		t.Fatal("native media qualification requires its named isolated local test database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "kubectl", "--context", "k3d-memql", "-n", "memql", "get", "secret", "memql-calls-media", "-o", "json").Output()
	if err != nil {
		t.Fatal("local media Secret unavailable")
	}
	var secret struct {
		Data map[string]string `json:"data"`
	}
	if json.Unmarshal(raw, &secret) != nil {
		t.Fatal("local media Secret malformed")
	}
	read := func(name string) string {
		v, e := base64.StdEncoding.DecodeString(secret.Data[name])
		if e != nil {
			t.Fatal("local media Secret malformed")
		}
		return string(v)
	}
	if read(EnvMediaAPIURL) != "http://host.k3d.internal:7880" {
		t.Fatal("refusing nonlocal media configuration")
	}
	credential := MediaCredential{Key: read(EnvMediaAPIKey), Secret: read(EnvMediaAPISecret)}
	if credential.Key == "" || credential.Secret == "" {
		t.Fatal("local media credentials missing")
	}
	return credential, lksdk.NewRoomServiceClient("http://127.0.0.1:7880", credential.Key, credential.Secret)
}

func nativeLocalSignalConfigure(t *testing.T, cw *callWorld, credential MediaCredential) {
	t.Helper()
	ctx := context.Background()
	sealed, err := credential.seal()
	if err != nil {
		t.Fatal("fixture credential encoding failed")
	}
	if cw.svc.store.sealSecret(ctx, SecretMediaBootstrap, sealed, "isolated local signalling fixture") != nil {
		t.Fatal("fixture credential sealing failed")
	}
	configured, err := cw.svc.store.MediaServerByID(ctx, BootstrapMediaServerID)
	require.NoError(t, err)
	require.NotNil(t, configured)
	configured.APIURL = "http://127.0.0.1:7880"
	// Commerce helpers normally use a larger fake registry. A real local
	// fixture may never advertise more than the existing50-seat ceiling.
	if configured.Seats > 50 {
		configured.Seats = 50
	}
	require.NoError(t, cw.svc.store.updateMediaServer(ctx, *configured, configured.Version, memql.VersionTimeAfter(configured.Version, time.Now())))
	cw.svc.media = func(target MediaServer, key MediaCredential) (MediaBackend, error) {
		return NewLiveKitBackend(target.APIURL, key.Key, key.Secret)
	}
	cw.svc.now = func() time.Time { return time.Now().UTC() }
	t.Cleanup(func() {
		_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", mediaSignalConcept).Where("payload->>'siteId' = ?", siteConcept+":"+cw.site).Exec(context.Background())
	})
}

func nativeLocalSignalRoomCleanup(t *testing.T, control *lksdk.RoomServiceClient, room string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, e := control.DeleteRoom(ctx, &livekit.DeleteRoomRequest{Room: room}); e != nil && !isNotFound(e) {
			t.Error("native fixture provider room deletion failed")
		}
		left, e := control.ListRooms(ctx, &livekit.ListRoomsRequest{Names: []string{room}})
		if e != nil || len(left.GetRooms()) != 0 {
			t.Error("native fixture provider cleanup readback failed")
		}
	})
}

func nativeSignalResponse(t *testing.T, connection *websocket.Conn) *livekit.SignalResponse {
	t.Helper()
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	kind, raw, e := connection.ReadMessage()
	if e != nil {
		t.Fatal("native guarded provider response missing")
	}
	response := &livekit.SignalResponse{}
	if kind == websocket.BinaryMessage {
		e = proto.Unmarshal(raw, response)
	} else if kind == websocket.TextMessage {
		e = protojson.Unmarshal(raw, response)
	} else {
		t.Fatal("unexpected provider response framing")
	}
	if e != nil {
		t.Fatal("native guarded provider response malformed")
	}
	return response
}

func nativeSignalConnect(t *testing.T, base, token, sid string) (*websocket.Conn, *livekit.SignalResponse) {
	t.Helper()
	q := url.Values{"access_token": {token}, "protocol": {"16"}, "sdk": {"go"}}
	if sid != "" {
		q.Set("reconnect", "1")
		q.Set("sid", sid)
	}
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	connection, response, e := dialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/media/rtc?"+q.Encode(), nil)
	if e != nil {
		if response != nil {
			response.Body.Close()
		}
		t.Fatal("native guarded connection failed")
	}
	t.Cleanup(func() { connection.Close() })
	first := nativeSignalResponse(t, connection)
	if sid == "" {
		if first.GetJoin() == nil || first.GetJoin().GetServerInfo().GetVersion() != "1.13.7" {
			t.Fatal("native guard did not admit on the pinned provider")
		}
	} else if first.GetReconnect() == nil {
		t.Fatal("native guard did not resume the existing provider participant")
	}
	return connection, first
}

func nativeSignalRefreshedToken(t *testing.T, connection *websocket.Conn, credential MediaCredential) string {
	t.Helper()
	for i := 0; i < 12; i++ {
		response := nativeSignalResponse(t, connection)
		if token := response.GetRefreshToken(); token != "" {
			verifier, e := lkauth.ParseAPIToken(token)
			if e != nil {
				t.Fatal("provider refreshed token malformed")
			}
			if _, _, e = verifier.Verify(credential.Secret); e != nil {
				t.Fatal("provider refreshed token signature invalid")
			}
			return token
		}
	}
	t.Fatal("provider did not issue a refreshed JWT")
	return ""
}

func nativeSignalAwaitClosed(t *testing.T, connection *websocket.Conn) {
	t.Helper()
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	for i := 0; i < 32; i++ {
		if _, _, err := connection.ReadMessage(); err != nil {
			if timeout, ok := err.(interface{ Timeout() bool }); ok && timeout.Timeout() {
				t.Fatal("native revocation did not close established signalling")
			}
			return
		}
	}
	t.Fatal("native revocation left established signalling open")
}

func nativeSignalValidationPreservesActive(t *testing.T, cw *callWorld, base, token, roomID string, control *lksdk.RoomServiceClient) {
	t.Helper()
	ctx := context.Background()
	room, e := cw.svc.store.roomByID(ctx, cw.owner, roomID)
	require.NoError(t, e)
	require.NotNil(t, room)
	before, e := cw.svc.store.mediaSignals(ctx, *room)
	require.NoError(t, e)
	require.Len(t, before.Intents, 1)
	require.Equal(t, "active", before.Intents[0].State)
	for _, suffix := range []string{"/media/rtc/validate", "/media/rtc/v1/validate"} {
		response, e := (&http.Client{Timeout: 8 * time.Second}).Get(base + suffix + "?" + url.Values{"access_token": {token}}.Encode())
		if e != nil {
			t.Fatal("connected validation request failed")
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal("connected native validation refused current authority")
		}
	}
	after, e := cw.svc.store.mediaSignals(ctx, *room)
	require.NoError(t, e)
	require.Len(t, after.Intents, 1)
	require.Equal(t, before.Intents[0].ID, after.Intents[0].ID)
	require.Equal(t, "active", after.Intents[0].State)
	people, e := control.ListParticipants(ctx, &livekit.ListParticipantsRequest{Room: room.MediaRoom})
	if e != nil || len(people.GetParticipants()) != 1 {
		t.Fatal("validation retired the live provider participant")
	}
}

func nativeSignalAssertDeniedAndClear(t *testing.T, cw *callWorld, base, roomID string, control *lksdk.RoomServiceClient, tokens ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, token := range tokens {
		response, e := (&http.Client{Timeout: 8 * time.Second}).Get(base + "/media/rtc/validate?" + url.Values{"access_token": {token}}.Encode())
		if e != nil {
			t.Fatal("revoked native validation request failed")
		}
		response.Body.Close()
		if response.StatusCode != 404 {
			t.Fatal("revoked JWT passed native validation")
		}
		connection, response, e := (&websocket.Dialer{HandshakeTimeout: 5 * time.Second}).DialContext(ctx, "ws"+strings.TrimPrefix(base, "http")+"/media/rtc?"+url.Values{"access_token": {token}, "protocol": {"16"}}.Encode(), nil)
		if connection != nil {
			connection.Close()
			t.Fatal("revoked JWT reopened media signalling")
		}
		if e == nil || response == nil || response.StatusCode != 404 {
			t.Fatal("revoked JWT did not receive fixed admission refusal")
		}
		response.Body.Close()
	}
	for {
		status, e := cw.svc.ReconcileMediaSignals(ctx, roomID)
		if e == nil && status.State == "clear" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("native cleanup did not become provider-confirmed clear")
		case <-time.After(100 * time.Millisecond):
		}
	}
	room, e := cw.svc.store.roomByID(ctx, cw.owner, roomID)
	require.NoError(t, e)
	people, e := control.ListParticipants(ctx, &livekit.ListParticipantsRequest{Room: room.MediaRoom})
	if (e != nil && !isNotFound(e)) || len(people.GetParticipants()) != 0 {
		t.Fatal("revoked native participant remained at provider")
	}
}

func TestMediaSignalLiveNativeCredentialRevocationAndConnectedValidation(t *testing.T) {
	credential, control := nativeLocalSignalOptIn(t)
	cw := newCallWorld(t, "live-native-credential", 10)
	if cw == nil {
		return
	}
	nativeLocalSignalConfigure(t, cw, credential)
	ctx := context.Background()
	_, started := cw.started("Native fixture host")
	room, e := cw.svc.store.roomByID(ctx, cw.owner, started.Call.ID)
	require.NoError(t, e)
	nativeLocalSignalRoomCleanup(t, control, room.MediaRoom)
	first := accountRegister(t, cw, newSoftwareAuthenticator(t))
	begin, e := cw.svc.Account(ctx, cw.ref, first.result.Session.Token, &callrules.AccountRequest{Action: "add_begin"})
	require.NoError(t, e)
	challenge, rp, _ := accountCreation(t, begin)
	backup := newSoftwareAuthenticator(t)
	_, e = cw.svc.Account(ctx, cw.ref, first.result.Session.Token, &callrules.AccountRequest{Action: "add_finish", ChallengeID: begin.ChallengeID, Credential: backup.Attest(challenge, rp, "https://"+rp)})
	require.NoError(t, e)
	other := first
	other.auth = backup
	manager, e := accountAssert(t, cw, other, "", "login", "")
	require.NoError(t, e)
	joined, session, e := cw.svc.Join(ctx, cw.ref, first.result.Session, callrules.JoinBody{Token: started.Call.GuestToken, Name: "Credential fixture", Confirm: confirmed})
	require.NoError(t, e)
	h := httptest.NewServer(server.NewMediaSignalHandler(cw.svc))
	defer h.Close()
	connection, initial := nativeSignalConnect(t, h.URL, joined.Pass, "")
	refreshed := nativeSignalRefreshedToken(t, connection, credential)
	nativeSignalValidationPreservesActive(t, cw, h.URL, joined.Pass, started.Call.ID, control)
	resumed, _ := nativeSignalConnect(t, h.URL, refreshed, initial.GetJoin().GetParticipant().GetSid())
	_, e = cw.svc.Account(ctx, cw.ref, manager.Session.Token, &callrules.AccountRequest{Action: "revoke", CredentialID: first.result.Credentials[0].ID})
	require.NoError(t, e)
	_, e = cw.svc.Session(ctx, cw.ref, session.Token)
	require.ErrorIs(t, e, callrules.ErrNoSession)
	nativeSignalAwaitClosed(t, resumed)
	nativeSignalAssertDeniedAndClear(t, cw, h.URL, started.Call.ID, control, joined.Pass, refreshed)
	t.Log("real native credential/session revocation denied issued and provider-refreshed JWTs; connected validation and overlapping reconnect preserved authority before revoke; cleanup clear")
}

func TestMediaSignalLiveNativeDelegationRevocation(t *testing.T) {
	credential, control := nativeLocalSignalOptIn(t)
	cw, k, _ := commerceFixture(t, "live-native-delegation")
	if cw == nil {
		return
	}
	nativeLocalSignalConfigure(t, cw, credential)
	cleanupDelegationTestRows(t, cw)
	commerceVerified(t, cw, k)
	ctx := context.Background()
	agent, grant := delegationTestGrant(t, cw, k, delegationTestRequest(cw))
	quoted, e := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, agent, callrules.AgentRequest{Action: "quote", ParticipantLimit: 2, DurationMinutes: 60})
	require.NoError(t, e)
	result, e := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, agent, callrules.AgentRequest{Action: "start", IdempotencyKey: "live-native-agent-start", QuoteID: quoted.(*callrules.CallQuote).QuoteID, Name: "Native fixture agent", Terms: callrules.AgentTermsVersion, Participation: "adults_only"})
	require.NoError(t, e)
	started := result.(callrules.StartResult)
	room, e := cw.svc.store.roomByID(ctx, cw.owner, started.Call.ID)
	require.NoError(t, e)
	nativeLocalSignalRoomCleanup(t, control, room.MediaRoom)
	h := httptest.NewServer(server.NewMediaSignalHandler(cw.svc))
	defer h.Close()
	connection, _ := nativeSignalConnect(t, h.URL, started.Join.Pass, "")
	refreshed := nativeSignalRefreshedToken(t, connection, credential)
	_, e = cw.svc.Delegation(ctx, cw.ref, k.result.Session.Token, &callrules.DelegationRequest{Action: "revoke", GrantID: grant})
	require.NoError(t, e)
	nativeSignalAwaitClosed(t, connection)
	nativeSignalAssertDeniedAndClear(t, cw, h.URL, started.Call.ID, control, started.Join.Pass, refreshed)
	t.Log("real native delegation revocation denied issued and provider-refreshed JWTs; provider cleanup readback clear")
}
