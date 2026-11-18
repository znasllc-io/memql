package calls

import "fmt"

// The track sources a seat may publish. LiveKit grants anything a token
// leaves unsaid, so every GrantSpec field is set explicitly.
const (
	sourceMicrophone       = "microphone"
	sourceCamera           = "camera"
	sourceScreenShare      = "screen_share"
	sourceScreenShareAudio = "screen_share_audio"
)

// GrantSpec is the complete set of permissions a seat's media token carries.
type GrantSpec struct {
	RoomJoin             bool
	CanPublish           bool
	CanSubscribe         bool
	CanPublishData       bool
	CanUpdateOwnMetadata bool
	Hidden               bool
	Sources              []string
}

// GrantFor returns the grants for a seat kind (design section 10, "Pass
// rules"). CanPublish is true exactly when there is a source to publish.
// CanUpdateOwnMetadata is false for every kind: an identity that could write
// its own metadata could rename itself and defeat the kind labels. Only the
// sampler is hidden. An unknown kind, the empty string included, is an error
// so a new kind cannot silently inherit someone else's rights. Sources is a
// fresh slice each call so a caller cannot change a shared table.
func GrantFor(k SeatKind) (GrantSpec, error) {
	var sources []string
	var data, hidden bool
	switch k {
	case KindPerson, KindAgent:
		sources = []string{sourceMicrophone, sourceCamera, sourceScreenShare, sourceScreenShareAudio}
		data = true
	case KindAvatar:
		sources = []string{sourceMicrophone, sourceCamera}
		data = true
	case KindSampler:
		sources = []string{}
		hidden = true
	default:
		return GrantSpec{}, fmt.Errorf("unknown seat kind %q", string(k))
	}
	return GrantSpec{
		RoomJoin:             true,
		CanPublish:           len(sources) > 0,
		CanSubscribe:         true,
		CanPublishData:       data,
		CanUpdateOwnMetadata: false,
		Hidden:               hidden,
		Sources:              sources,
	}, nil
}
