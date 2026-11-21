package calls

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	lkauth "github.com/livekit/protocol/auth"

	callrules "github.com/znasllc-io/memql/component/calls"
)

// pass.go -- the media server pass a seat joins with (design section 10, "Pass
// rules").
//
// LIVEKIT GRANTS WHAT A TOKEN LEAVES UNSAID. A video grant with no publish permission
// set may publish every source, and one with no subscribe permission may subscribe, so
// a pass that names only what it allows allows everything else. So MintPass states
// EVERY permission, false included: each pointer-valued permission is set, the
// sources are exactly the rules module's (calls.GrantFor), and the grants beside video
// (SIP, agent, inference, observability) are present and empty. It never mints a
// worker (video.agent), which would let its holder take jobs and passes from other
// rooms, and never an administrator of anything.
//
// THE KIND LABEL IS THE SERVER'S. Every pass carries attributes["memql.calls.kind"],
// set here from the seat it was minted for, over anything the caller passed under that
// name: a participant may not rename itself (CanUpdateOwnMetadata is false for every
// kind), and clients label people and agents by this attribute and the identity, never
// by the display name.

// KindAttribute is the participant attribute carrying the seat's kind.
const KindAttribute = "memql.calls.kind"

// MaxPassTTL is the longest a pass may live. A pass is for joining, which takes
// seconds, and a self-hosted server cannot revoke one, so a long pass is a long window
// for one that leaked. The shipped policy mints for 120 seconds.
const MaxPassTTL = 15 * time.Minute

// MintPass signs a pass for one seat of one room with the media server's API key pair.
// The TTL must be positive and at most MaxPassTTL: a zero TTL is not "expired" to
// LiveKit's SDK, it is SIX HOURS (its default), so it is refused rather than passed on.
func MintPass(key, secret string, kind callrules.SeatKind, room, identity, name string, attrs map[string]string, ttl time.Duration) (string, error) {
	return mintPass(key, secret, kind, room, identity, name, attrs, ttl, true)
}
func mintPass(key, secret string, kind callrules.SeatKind, room, identity, name string, attrs map[string]string, ttl time.Duration, peerData bool) (string, error) {
	switch {
	case strings.TrimSpace(key) == "":
		return "", errors.New("calls: a pass is signed with the media server's API key, and there is none")
	case len(secret) < minMediaSecretLength:
		return "", fmt.Errorf("calls: a pass is signed with an API secret of at least %d characters", minMediaSecretLength)
	case strings.TrimSpace(room) == "":
		return "", errors.New("calls: a pass names the room it joins")
	case strings.TrimSpace(identity) == "":
		return "", errors.New("calls: a pass names the identity it joins as")
	case ttl <= 0 || ttl > MaxPassTTL:
		return "", fmt.Errorf("calls: a pass lives more than no time and at most %s", MaxPassTTL)
	}
	spec, err := callrules.GrantFor(kind)
	if err != nil {
		return "", fmt.Errorf("calls: no pass for that seat: %w", err)
	}
	// An empty source list is not "nothing" to LiveKit, it is "everything", whenever
	// publishing is allowed at all (VideoGrant.GetCanPublishSource). The rules module
	// never pairs the two, and a pass is not minted if it ever does.
	if spec.CanPublish && len(spec.Sources) == 0 {
		return "", fmt.Errorf("calls: the grant for %q may publish with no source named, which LiveKit reads as every source", kind)
	}

	video := &lkauth.VideoGrant{
		RoomJoin: true,
		Room:     room,
		Hidden:   spec.Hidden,
		// Powers no seat holds, said in so many words.
		RoomCreate:   false,
		RoomList:     false,
		RoomRecord:   false,
		RoomAdmin:    false,
		IngressAdmin: false,
		Recorder:     false,
		Agent:        false,
	}
	video.SetCanPublish(spec.CanPublish)
	video.SetCanSubscribe(spec.CanSubscribe)
	video.SetCanPublishData(spec.CanPublishData && peerData)
	video.SetCanUpdateOwnMetadata(spec.CanUpdateOwnMetadata)
	video.SetCanSubscribeMetrics(false)
	video.SetCanManageAgentSession(false)
	video.CanPublishSources = append([]string{}, spec.Sources...)

	labels := maps.Clone(attrs)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[KindAttribute] = string(kind)

	token, err := lkauth.NewAccessToken(key, secret).
		SetIdentity(identity).
		SetName(name).
		SetVideoGrant(video).
		SetSIPGrant(&lkauth.SIPGrant{}).
		SetAgentGrant(&lkauth.AgentGrant{}).
		SetInferenceGrant(&lkauth.InferenceGrant{}).
		SetObservabilityGrant(&lkauth.ObservabilityGrant{}).
		SetAttributes(labels).
		SetValidFor(ttl).
		ToJWT()
	if err != nil {
		return "", fmt.Errorf("calls: sign the pass: %w", err)
	}
	return token, nil
}
