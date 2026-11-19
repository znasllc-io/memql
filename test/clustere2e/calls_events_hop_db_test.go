package clustere2e

// calls_events_hop_db_test.go -- calls joined through one replica, their media server's
// webhooks received and applied by the other.
//
// # WHAT IS AT STAKE
//
// A media server posts its webhooks to the cluster's front door, which hands each to
// whichever bff replica it picks: the replica that let a person in is no likelier to hear
// that they connected than any other. The replica that receives a webhook stages it and
// applies it itself (a delivery stays on the node that staged it: component/node
// routing.go), reading the call and its seats from Postgres and writing them under the
// call's cross-replica gate. Anything a replica kept to itself -- a seat it admitted, a
// lock only it honours -- would leave the webhook nothing to apply on the other replica,
// or let it write a seat the first replica writes at the same moment.
//
// # WHAT THIS PROVES
//
// Two engines over one Postgres, each with its own pool, cache, gate pool and calls
// integration (calls_hop_db_test.go's world). Four calls are started and joined through
// replica A, which made every session and seat. Then, all at once:
//
//   - every connection's participant_joined, signed as LiveKit's SDK signs a webhook, is
//     received by replica B's inbound receiver and applied by B's callsApplyMediaEvent
//     builtin, as the applyMediaEvent automation runs it there;
//   - each host's screen share is applied the same way through B, which writes the room;
//   - through replica A, every guest's page joins again under a new name, which writes
//     the guest's seat and the room's join count, and every host locks the call, which
//     writes the room.
//
// Every application and every write succeeds. Afterwards every seat is in the call, seen
// at its event's time and named as A last named it, every room records its host's share
// and its lock, and every delivery was stamped processed. A delivery received twice is one
// row, worked once.
//
// # HOW TO CONFIRM IT IS LOAD-BEARING
//
// Replace the advisory gates in integrations/calls service.go (Service.gates) with a mutex
// held by the Service, a single node's lock: an event on B and a write on A to the same
// seat or room each read it at the same version, and one of them loses the row's version
// fence (measured; see the task report).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	lkauth "github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	lkjson "github.com/livekit/protocol/utils/protojson"

	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/inbound"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/calls"
)

// The webhook key pair newCallsHopWorld's bootstrap seals as calls.media.webhook.
const (
	hopWebhookKey    = "APIhopwebhook"
	hopWebhookSecret = "hop-webhook-secret-0123456789abcdefghij"
)

// webhookReceiver is a replica's inbound receiver with the cluster's registered `livekit`
// source, as the bff wires it (app/transport_inbound.go liveKitWebhookResolver, whose own
// test holds its policy to LiveKit's SDK): the sealed calls.media.webhook credential
// verifies, its key as the token's issuer.
func (w *callsHopWorld) webhookReceiver(r *callsReplica) *inbound.Handler {
	h := inbound.NewHandler(inbound.Config{Enabled: true, MaxBodyBytes: 1 << 18, Tolerance: 5 * time.Minute}, callsEngine{r.eng}, nil)
	h.SetRegisteredSource(func(ctx context.Context, name string) (inbound.SourceConfig, bool) {
		if name != calls.WebhookSource {
			return inbound.SourceConfig{}, false
		}
		plaintext, err := r.eng.ResolveSystemSecret(ctx, calls.SecretMediaWebhook)
		if err != nil {
			return inbound.SourceConfig{}, false
		}
		cred, err := calls.ParseMediaCredential(plaintext)
		if err != nil {
			return inbound.SourceConfig{}, false
		}
		return inbound.SourceConfig{Secret: cred.Secret, Scheme: inbound.SchemeJWTHS256BodySHA256, SignatureHeader: "Authorization", JWTIssuer: cred.Key}, true
	})
	return h
}

// hopWebhook is one webhook as LiveKit's notifier sends it: the event marshalled by its
// SDK, signed by its SDK's AccessToken with the body's digest, the token bare in
// Authorization.
func hopWebhook(t *testing.T, ev *livekit.WebhookEvent) *http.Request {
	t.Helper()
	body, err := lkjson.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	token, err := lkauth.NewAccessToken(hopWebhookKey, hopWebhookSecret).SetValidFor(5 * time.Minute).
		SetSha256(base64.StdEncoding.EncodeToString(sum[:])).ToJWT()
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, inbound.RoutePrefix+calls.WebhookSource, bytes.NewReader(body))
	r.Header.Set("Authorization", token)
	r.Header.Set("Content-Type", "application/webhook+json")
	return r
}

// receiveAndApply is one webhook on one replica, as a bff works it: its receiver stages
// the delivery, and the applyMediaEvent automation's builtin applies it there, under the
// actor the executor mints for that automation of the engine's own tree. It answers the
// staged row's id and the builtin's outcome.
func receiveAndApply(ctx context.Context, receiver *inbound.Handler, r *callsReplica, req *http.Request) (string, string, error) {
	rec := httptest.NewRecorder()
	receiver.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != http.StatusAccepted {
		return "", "", fmt.Errorf("the receiver answered %d: %s", rec.Code, rec.Body.String())
	}
	var staged map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &staged); err != nil {
		return "", "", err
	}
	requestID := "v1:platform:inboundRequest:" + staged["id"]
	call, err := langparser.RenderCall("callsApplyMediaEvent", map[string]any{"requestId": requestID})
	if err != nil {
		return "", "", err
	}
	automation := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, auth.AutomationActor(auth.AutomationOriginEngine, "applyMediaEvent")))
	res, err := r.eng.Execute(automation, "builtin "+call)
	if err != nil {
		return requestID, "", err
	}
	rows := memql.MaterializeRows(res)
	if len(rows) != 1 {
		return requestID, "", fmt.Errorf("the builtin answered %d rows", len(rows))
	}
	outcome, _ := rows[0]["outcome"].(string)
	return requestID, outcome, nil
}

// latestPayload is a row's newest stored version, straight off the table.
func (w *callsHopWorld) latestPayload(concept, id string) map[string]any {
	w.t.Helper()
	var payloads []json.RawMessage
	err := w.a.db.NewRaw(`SELECT payload FROM "MemoryNodes" WHERE concept = ? AND id = ? ORDER BY "createdAt" DESC LIMIT 1`,
		concept, concept+":"+memql.BareShortId(id)).Scan(context.Background(), &payloads)
	if err != nil {
		w.t.Fatal(err)
	}
	if len(payloads) != 1 {
		w.t.Fatalf("%s %s is not stored", concept, id)
	}
	var row map[string]any
	if err := json.Unmarshal(payloads[0], &row); err != nil {
		w.t.Fatal(err)
	}
	return row
}

func hopTime(row map[string]any, field string) time.Time {
	s, _ := row[field].(string)
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return at.UTC()
}

// TestParticipantEventsUpdateSeatsOnAnotherReplica: the file note's claim. Calls joined
// through replica A have their webhooks received and applied through replica B while A
// writes the same seats and rooms, and every event lands, every write lands, and nothing
// is lost between them.
func TestParticipantEventsUpdateSeatsOnAnotherReplica(t *testing.T) {
	w := newCallsHopWorld(t, 50)
	if w == nil {
		return
	}
	// Each engine dispatches its own replica's calls builtins, as a bff's does.
	for _, r := range []*callsReplica{w.a, w.b} {
		if err := r.eng.RegisterIntegration(r.in); err != nil {
			t.Fatal(err)
		}
	}
	receiver := w.webhookReceiver(w.b)
	ctx := context.Background()

	const callCount, guestsPerCall = 4, 3
	type seat struct {
		sess     callrules.Session
		identity string
	}
	type call struct {
		id     string
		host   seat
		guests []seat
	}
	all := make([]call, callCount)
	for i := range all {
		hostSess := w.hostSession()
		started, err := w.a.in.Service().Start(ctx, hostSess, callrules.StartBody{Name: "Host", Confirm: hopConfirmed})
		if err != nil || started.Join.Outcome != callrules.OutcomeAdmit {
			t.Fatalf("start call %d on replica A: %+v, %v", i, started.Join, err)
		}
		if hostSess, err = w.a.in.Service().Session(ctx, w.ref, hostSess.Token); err != nil {
			t.Fatal(err)
		}
		all[i] = call{id: started.Call.ID, host: seat{sess: hostSess, identity: started.Join.Identity}}
		for j := 0; j < guestsPerCall; j++ {
			res, sess, err := w.a.in.Service().Join(ctx, w.ref, nil, callrules.JoinBody{
				Token: started.Call.GuestToken, Name: fmt.Sprintf("Guest %d.%d", i, j), Confirm: hopConfirmed,
			})
			if err != nil || res.Outcome != callrules.OutcomeAdmit {
				t.Fatalf("guest %d of call %d on replica A: %+v, %v", j, i, res, err)
			}
			all[i].guests = append(all[i].guests, seat{sess: sess, identity: res.Identity})
		}
	}
	// The media server's clock, strictly after every admission above.
	connected := time.Now().Add(2 * time.Second).Truncate(time.Second)
	shared := connected.Add(time.Second)

	var mu sync.Mutex
	var failures []string
	var deliveries []string
	fail := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		failures = append(failures, fmt.Sprintf(format, args...))
	}
	apply := func(what string, ev *livekit.WebhookEvent, want string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		requestID, outcome, err := receiveAndApply(ctx, receiver, w.b, hopWebhook(t, ev))
		if err != nil || outcome != want {
			fail("%s through replica B answered %q, %v; want %q", what, outcome, err, want)
		}
		mu.Lock()
		deliveries = append(deliveries, requestID)
		mu.Unlock()
	}
	joined := func(room, identity string) *livekit.WebhookEvent {
		return &livekit.WebhookEvent{Event: "participant_joined", Id: "EV_" + identity, CreatedAt: connected.Unix(),
			Room: &livekit.Room{Sid: "RM_" + room, Name: room}, Participant: &livekit.ParticipantInfo{Sid: "PA_" + identity, Identity: identity}}
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	run := func(f func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			f()
		}()
	}
	for i, c := range all {
		run(func() {
			apply(fmt.Sprintf("call %d host's participant_joined", i), joined(c.id, c.host.identity), "applied")
		})
		run(func() {
			apply(fmt.Sprintf("call %d host's screen share", i), &livekit.WebhookEvent{
				Event: "track_published", Id: "EV_share_" + c.id, CreatedAt: shared.Unix(),
				Room:        &livekit.Room{Sid: "RM_" + c.id, Name: c.id},
				Participant: &livekit.ParticipantInfo{Sid: "PA_" + c.host.identity, Identity: c.host.identity},
				Track:       &livekit.TrackInfo{Sid: "TR_share_" + c.id, Type: livekit.TrackType_VIDEO, Source: livekit.TrackSource_SCREEN_SHARE},
			}, "applied")
		})
		run(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := w.a.in.Service().Host(ctx, c.host.sess, callrules.HostActionRequest{Action: callrules.HostLock}); err != nil {
				fail("the host of call %d could not lock it through replica A: %v", i, err)
			}
		})
		for j, g := range c.guests {
			run(func() {
				apply(fmt.Sprintf("guest %d.%d's participant_joined", i, j), joined(c.id, g.identity), "applied")
			})
			run(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				res, _, err := w.a.in.Service().Join(ctx, w.ref, &g.sess, callrules.JoinBody{Name: fmt.Sprintf("Renamed %d.%d", i, j), Confirm: hopConfirmed})
				if err != nil || res.Outcome != callrules.OutcomeAdmit {
					fail("guest %d.%d's page could not join again through replica A: %+v, %v", i, j, res, err)
				}
			})
		}
	}
	close(start)
	wg.Wait()
	for _, f := range failures {
		t.Error(f)
	}

	for i, c := range all {
		host := w.latestPayload("v1:calls:seat", c.host.sess.SeatID)
		if host["status"] != "in_call" || !hopTime(host, "lastSeenAt").Equal(connected) {
			t.Errorf("call %d's host seat stands %v, last seen %v: want in_call, seen at %v", i, host["status"], host["lastSeenAt"], connected)
		}
		for j, g := range c.guests {
			got := w.latestPayload("v1:calls:seat", g.sess.SeatID)
			if got["status"] != "in_call" || !hopTime(got, "lastSeenAt").Equal(connected) {
				t.Errorf("guest %d.%d's seat stands %v, last seen %v: want in_call, seen at %v", i, j, got["status"], got["lastSeenAt"], connected)
			}
			if want := fmt.Sprintf("Renamed %d.%d", i, j); got["displayName"] != want {
				t.Errorf("guest %d.%d's seat is named %v, want %q: replica A's write was lost", i, j, got["displayName"], want)
			}
		}
		room := w.latestRoom(c.id)
		if room["locked"] != true {
			t.Errorf("call %d is not locked: replica A's lock was lost", i)
		}
		if memql.BareShortId(fmt.Sprint(room["shareSeatId"])) != c.host.sess.SeatID || !hopTime(room, "shareStartedAt").Equal(shared) {
			t.Errorf("call %d records the share of %v since %v: want its host's since %v", i, room["shareSeatId"], room["shareStartedAt"], shared)
		}
	}
	for _, requestID := range deliveries {
		if requestID == "" {
			continue
		}
		if got := w.latestPayload("v1:platform:inboundRequest", requestID)["status"]; got != "processed" {
			t.Errorf("delivery %s stands %v, want processed", requestID, got)
		}
	}

	// A webhook received twice -- a sender's retry, on either replica -- is one row,
	// worked once.
	again := all[0].guests[0]
	retry := &livekit.WebhookEvent{Event: "participant_left", Id: "EV_retry", CreatedAt: shared.Add(time.Second).Unix(),
		Room: &livekit.Room{Sid: "RM_" + all[0].id, Name: all[0].id}, Participant: &livekit.ParticipantInfo{Sid: "PA_" + again.identity, Identity: again.identity}}
	first, outcome, err := receiveAndApply(ctx, receiver, w.b, hopWebhook(t, retry))
	if err != nil || outcome != "applied" {
		t.Fatalf("the first delivery answered %q, %v", outcome, err)
	}
	second, outcome, err := receiveAndApply(ctx, w.webhookReceiver(w.a), w.a, hopWebhook(t, retry))
	if err != nil || outcome != "already_worked" {
		t.Errorf("the same webhook received again through replica A answered %q, %v: want already_worked", outcome, err)
	}
	if first != second {
		t.Errorf("one webhook received twice staged two rows: %s and %s", first, second)
	}
}
