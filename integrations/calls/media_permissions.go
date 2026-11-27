package calls

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/livekit/protocol/livekit"
	callrules "github.com/znasllc-io/memql/component/calls"
)

// MediaPermissionBackend is optional until a native presenter/audience policy
// invokes it. Permission changes return provider readback; callers must retain
// pending enforcement when this operation fails or the response differs.
type MediaPermissionBackend interface {
	UpdatePermissions(context.Context, string, string, callrules.GrantSpec) (callrules.GrantSpec, error)
}
type roomPermissionService interface {
	UpdateParticipant(context.Context, *livekit.UpdateParticipantRequest) (*livekit.ParticipantInfo, error)
}

func nativeParticipantPermission(spec callrules.GrantSpec) (*livekit.ParticipantPermission, error) {
	if !spec.RoomJoin || spec.CanUpdateOwnMetadata || spec.CanPublish && len(spec.Sources) == 0 || !spec.CanPublish && len(spec.Sources) != 0 {
		return nil, errors.New("calls: invalid native publication permission")
	}
	p := &livekit.ParticipantPermission{CanSubscribe: spec.CanSubscribe, CanPublish: spec.CanPublish, CanPublishData: spec.CanPublishData, Hidden: spec.Hidden}
	seen := map[string]bool{}
	for _, v := range spec.Sources {
		if seen[v] {
			return nil, errors.New("calls: duplicate publication source")
		}
		seen[v] = true
		switch v {
		case "microphone":
			p.CanPublishSources = append(p.CanPublishSources, livekit.TrackSource_MICROPHONE)
		case "camera":
			p.CanPublishSources = append(p.CanPublishSources, livekit.TrackSource_CAMERA)
		case "screen_share":
			p.CanPublishSources = append(p.CanPublishSources, livekit.TrackSource_SCREEN_SHARE)
		case "screen_share_audio":
			p.CanPublishSources = append(p.CanPublishSources, livekit.TrackSource_SCREEN_SHARE_AUDIO)
		default:
			return nil, errors.New("calls: unknown publication source")
		}
	}
	if seen["screen_share_audio"] && !seen["screen_share"] {
		return nil, errors.New("calls: screen audio requires screen publication")
	}
	return p, nil
}
func participantPermissionSpec(p *livekit.ParticipantPermission) callrules.GrantSpec {
	if p == nil {
		return callrules.GrantSpec{}
	}
	spec := callrules.GrantSpec{RoomJoin: true, CanPublish: p.CanPublish, CanSubscribe: p.CanSubscribe, CanPublishData: p.CanPublishData, CanUpdateOwnMetadata: p.CanUpdateMetadata, Hidden: p.Hidden, Sources: []string{}}
	for _, v := range p.CanPublishSources {
		spec.Sources = append(spec.Sources, strings.ToLower(v.String()))
	}
	return spec
}
func (b *liveKitBackend) UpdatePermissions(ctx context.Context, room, identity string, spec callrules.GrantSpec) (callrules.GrantSpec, error) {
	p, e := nativeParticipantPermission(spec)
	if e != nil {
		return callrules.GrantSpec{}, e
	}
	service, ok := b.rooms.(roomPermissionService)
	if !ok {
		return callrules.GrantSpec{}, errors.New("calls: provider cannot update participant permission")
	}
	result, e := service.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{Room: room, Identity: identity, Permission: p})
	if e != nil {
		return callrules.GrantSpec{}, e
	}
	got := participantPermissionSpec(result.GetPermission())
	wantSources, gotSources := slices.Clone(spec.Sources), slices.Clone(got.Sources)
	slices.Sort(wantSources)
	slices.Sort(gotSources)
	if result.GetIdentity() != identity || got.RoomJoin != spec.RoomJoin || got.CanPublish != spec.CanPublish || got.CanSubscribe != spec.CanSubscribe || got.CanPublishData != spec.CanPublishData || got.CanUpdateOwnMetadata != spec.CanUpdateOwnMetadata || got.Hidden != spec.Hidden || !slices.Equal(wantSources, gotSources) || result.GetPermission().GetAgent() || result.GetPermission().GetRecorder() || result.GetPermission().GetCanManageAgentSession() || result.GetPermission().GetCanSubscribeMetrics() {
		return got, errors.New("calls: provider permission readback differs")
	}
	return got, nil
}
