package calls

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	lkauth "github.com/livekit/protocol/auth"

	callrules "github.com/znasllc-io/memql/component/calls"
)

const (
	passKey    = "APIroomkey"
	passSecret = "pass-secret-0123456789abcdefghijklmnop"
)

// decodePass verifies a pass with the secret it was signed with, through
// LiveKit's own verifier, and answers what LiveKit will read from it.
func decodePass(t *testing.T, token string) (*lkauth.ClaimGrants, time.Duration, string) {
	t.Helper()
	verifier, err := lkauth.ParseAPIToken(token)
	if err != nil {
		t.Fatalf("the pass does not parse: %v", err)
	}
	claims, grants, err := verifier.Verify(passSecret)
	if err != nil {
		t.Fatalf("the pass does not verify with its secret: %v", err)
	}
	if claims.ExpiresAt == nil || claims.NotBefore == nil {
		t.Fatalf("the pass carries no lifetime: %+v", claims)
	}
	return grants, claims.ExpiresAt.Sub(claims.NotBefore.Time), verifier.APIKey()
}

// rawVideoGrant is the pass's `video` claim as LiveKit receives it on the wire:
// the JSON object, before any SDK fills a default in.
func rawVideoGrant(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("the pass is not a JWT: %d parts", len(parts))
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("the pass's body does not decode: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(body, &claims); err != nil {
		t.Fatalf("the pass's body is not JSON: %v", err)
	}
	video, _ := claims["video"].(map[string]any)
	if video == nil {
		t.Fatalf("the pass carries no video grant: %v", claims)
	}
	return video
}

// TestMintPassSetsEveryGrantExplicitly: LiveKit grants what a token leaves
// unsaid (design section 10), so a pass states every permission, each is
// exactly what the rules module says for the seat's kind, it lives for the TTL
// it was given, and the kind label is the server's.
func TestMintPassSetsEveryGrantExplicitly(t *testing.T) {
	for _, kind := range []callrules.SeatKind{callrules.KindPerson, callrules.KindAgent, callrules.KindAvatar, callrules.KindSampler} {
		t.Run(string(kind), func(t *testing.T) {
			spec, err := callrules.GrantFor(kind)
			if err != nil {
				t.Fatal(err)
			}
			// A caller's attributes ride along, and a caller naming the kind
			// attribute itself is overruled by the kind the pass is for.
			attrs := map[string]string{"memql.calls.room": "room-a", KindAttribute: "forged"}
			token, err := MintPass(passKey, passSecret, kind, "room-a", "seat-1", "Ada", attrs, 120*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			grants, ttl, issuer := decodePass(t, token)

			if issuer != passKey {
				t.Errorf("the pass names issuer %q, want the API key", issuer)
			}
			if ttl != 120*time.Second {
				t.Errorf("the pass lives %s, want 120s", ttl)
			}
			if grants.Identity != "seat-1" || grants.Name != "Ada" {
				t.Errorf("the pass names %q / %q, want seat-1 / Ada", grants.Identity, grants.Name)
			}
			if got := grants.Attributes[KindAttribute]; got != string(kind) {
				t.Errorf("attributes[%s] = %q, want %q", KindAttribute, got, kind)
			}
			if got := grants.Attributes["memql.calls.room"]; got != "room-a" {
				t.Errorf("a caller's attribute was dropped: %v", grants.Attributes)
			}

			video := grants.Video
			if video == nil {
				t.Fatal("the pass carries no video grant")
			}
			for name, ptr := range map[string]*bool{
				"canPublish": video.CanPublish, "canSubscribe": video.CanSubscribe, "canPublishData": video.CanPublishData,
				"canUpdateOwnMetadata": video.CanUpdateOwnMetadata, "canSubscribeMetrics": video.CanSubscribeMetrics,
				"canManageAgentSession": video.CanManageAgentSession,
			} {
				if ptr == nil {
					t.Errorf("%s is left unsaid, so LiveKit would decide it", name)
				}
			}
			if t.Failed() {
				return
			}
			if *video.CanPublish != spec.CanPublish || *video.CanSubscribe != spec.CanSubscribe ||
				*video.CanPublishData != spec.CanPublishData || *video.CanUpdateOwnMetadata != spec.CanUpdateOwnMetadata {
				t.Errorf("the grant differs from GrantFor(%s): %+v, want %+v", kind, video, spec)
			}
			if *video.CanSubscribeMetrics || *video.CanManageAgentSession {
				t.Errorf("the pass may read metrics or manage agent sessions: %+v", video)
			}
			if !slices.Equal(normalized(video.CanPublishSources), normalized(spec.Sources)) {
				t.Errorf("the pass may publish %v, want exactly %v", video.CanPublishSources, spec.Sources)
			}
			if !video.RoomJoin || video.Room != "room-a" || video.Hidden != spec.Hidden {
				t.Errorf("the pass joins %q (join=%v hidden=%v), want room-a hidden=%v", video.Room, video.RoomJoin, video.Hidden, spec.Hidden)
			}
			// Never a worker (video.agent) and never an administrator of anything.
			if video.Agent || video.RoomAdmin || video.RoomCreate || video.RoomList || video.RoomRecord ||
				video.IngressAdmin || video.Recorder || video.DestinationRoom != "" {
				t.Errorf("the pass carries a power no seat may hold: %+v", video)
			}
			for name, grant := range map[string]bool{
				"sip":           grants.SIP != nil && (grants.SIP.Admin || grants.SIP.Call),
				"agent":         grants.Agent != nil && (grants.Agent.Admin || grants.Agent.DispatchAdmin || grants.Agent.DatabaseAdmin || grants.Agent.SimulationAdmin),
				"inference":     grants.Inference != nil && grants.Inference.Perform,
				"observability": grants.Observability != nil && grants.Observability.Write,
			} {
				if grant {
					t.Errorf("the pass grants %s rights", name)
				}
			}
			if grants.SIP == nil || grants.Agent == nil || grants.Inference == nil || grants.Observability == nil {
				t.Errorf("a grant beside video is left unsaid: sip=%v agent=%v inference=%v observability=%v",
					grants.SIP, grants.Agent, grants.Inference, grants.Observability)
			}

			// On the wire: every pointer-valued permission is PRESENT, false
			// included, which is what "explicit" means to LiveKit's server.
			raw := rawVideoGrant(t, token)
			for _, key := range []string{"canPublish", "canSubscribe", "canPublishData", "canUpdateOwnMetadata", "canSubscribeMetrics", "canManageAgentSession"} {
				if _, present := raw[key]; !present {
					t.Errorf("the wire grant leaves %s out: %v", key, raw)
				}
			}
		})
	}
}

func normalized(sources []string) []string {
	if len(sources) == 0 {
		return []string{}
	}
	return sources
}

// TestMintPassRefusesWhatItMustNotMint: a pass with no lifetime is SIX HOURS to
// LiveKit's SDK, not zero, and an unknown kind would inherit somebody else's
// rights, so neither is minted.
func TestMintPassRefusesWhatItMustNotMint(t *testing.T) {
	ok := func() (string, string, callrules.SeatKind, string, string, time.Duration) {
		return passKey, passSecret, callrules.KindPerson, "room-a", "seat-1", 120 * time.Second
	}
	for _, tc := range []struct {
		name string
		edit func(key, secret *string, kind *callrules.SeatKind, room, identity *string, ttl *time.Duration)
	}{
		{"no lifetime", func(_, _ *string, _ *callrules.SeatKind, _, _ *string, ttl *time.Duration) { *ttl = 0 }},
		{"a negative lifetime", func(_, _ *string, _ *callrules.SeatKind, _, _ *string, ttl *time.Duration) { *ttl = -time.Second }},
		{"a lifetime past the ceiling", func(_, _ *string, _ *callrules.SeatKind, _, _ *string, ttl *time.Duration) {
			*ttl = MaxPassTTL + time.Second
		}},
		{"an unknown kind", func(_, _ *string, kind *callrules.SeatKind, _, _ *string, _ *time.Duration) { *kind = "admin" }},
		{"no kind", func(_, _ *string, kind *callrules.SeatKind, _, _ *string, _ *time.Duration) { *kind = "" }},
		{"no room", func(_, _ *string, _ *callrules.SeatKind, room, _ *string, _ *time.Duration) { *room = " " }},
		{"no identity", func(_, _ *string, _ *callrules.SeatKind, _, identity *string, _ *time.Duration) { *identity = "" }},
		{"no key", func(key, _ *string, _ *callrules.SeatKind, _, _ *string, _ *time.Duration) { *key = "" }},
		{"a short secret", func(_, secret *string, _ *callrules.SeatKind, _, _ *string, _ *time.Duration) {
			*secret = strings.Repeat("s", 31)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, secret, kind, room, identity, ttl := ok()
			tc.edit(&key, &secret, &kind, &room, &identity, &ttl)
			token, err := MintPass(key, secret, kind, room, identity, "Ada", nil, ttl)
			if err == nil || token != "" {
				t.Fatalf("%s minted a pass", tc.name)
			}
			if strings.Contains(err.Error(), passSecret) {
				t.Fatalf("the refusal repeats the secret: %v", err)
			}
		})
	}

	t.Run("the ceiling itself is allowed", func(t *testing.T) {
		key, secret, kind, room, identity, _ := ok()
		token, err := MintPass(key, secret, kind, room, identity, "", nil, MaxPassTTL)
		if err != nil {
			t.Fatal(err)
		}
		if _, ttl, _ := decodePass(t, token); ttl != MaxPassTTL {
			t.Fatalf("the pass lives %s, want %s", ttl, MaxPassTTL)
		}
	})
}
