package calls

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	callrules "github.com/znasllc-io/memql/component/calls"
)

// ResourceMediaBackend gives an untrusted encoder a revocable ingress key,
// never the JWT that the trusted ingress worker uses to publish to the room.
// Deletion must remove the stream-key mapping as well as stop its pipeline.
type ResourceMediaBackend interface {
	FindResourceIngress(context.Context, string, string) (*ResourceIngress, error)
	CreateResourceIngress(context.Context, string, string, string, callrules.ResourceSpec) (*ResourceIngress, error)
	DeleteResourceIngress(context.Context, string) error
}
type ResourceIngress struct{ ID, URL, StreamKey, Identity string }
type resourceIngressClient interface {
	CreateIngress(context.Context, *livekit.CreateIngressRequest) (*livekit.IngressInfo, error)
	ListIngress(context.Context, *livekit.ListIngressRequest) (*livekit.ListIngressResponse, error)
	DeleteIngress(context.Context, *livekit.DeleteIngressRequest) (*livekit.IngressInfo, error)
}
type liveKitResources struct{ client resourceIngressClient }

func resourceIngressInfo(v *livekit.IngressInfo) (*ResourceIngress, error) {
	if v == nil {
		return nil, nil
	}
	u, err := url.Parse(v.Url)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || (u.Scheme == "http" && u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || v.StreamKey == "" || strings.ContainsAny(v.StreamKey, "/\\?#\r\n") {
		return nil, resourceRefusal("resource_gateway_unavailable")
	}
	return &ResourceIngress{ID: v.IngressId, URL: v.Url, StreamKey: v.StreamKey, Identity: v.ParticipantIdentity}, nil
}
func (b *liveKitResources) FindResourceIngress(ctx context.Context, room, lease string) (*ResourceIngress, error) {
	res, err := b.client.ListIngress(ctx, &livekit.ListIngressRequest{RoomName: room})
	if err != nil {
		return nil, fmt.Errorf("calls: read resource ingress: %w", err)
	}
	var found *livekit.IngressInfo
	for _, v := range res.GetItems() {
		if v.Name == "callkey-resource-"+lease {
			if found != nil {
				return nil, resourceRefusal("resource_gateway_ambiguous")
			}
			found = v
		}
	}
	return resourceIngressInfo(found)
}
func (b *liveKitResources) CreateResourceIngress(ctx context.Context, room, lease, identity string, spec callrules.ResourceSpec) (*ResourceIngress, error) {
	if !callrules.ValidResourceSpec(spec) {
		return nil, callrules.ErrInvalid
	}
	transcode := true
	videoSource, audioSource := livekit.TrackSource_SCREEN_SHARE, livekit.TrackSource_SCREEN_SHARE_AUDIO
	if spec.Kind == "camera" || spec.Kind == "microphone" {
		videoSource, audioSource = livekit.TrackSource_CAMERA, livekit.TrackSource_MICROPHONE
	}
	res, err := b.client.CreateIngress(ctx, &livekit.CreateIngressRequest{
		InputType: livekit.IngressInput_WHIP_INPUT, Name: "callkey-resource-" + lease, RoomName: room, ParticipantIdentity: identity, ParticipantName: spec.Label,
		EnableTranscoding: &transcode,
		Video:             &livekit.IngressVideoOptions{Name: spec.Label, Source: videoSource, EncodingOptions: &livekit.IngressVideoOptions_Options{Options: &livekit.IngressVideoEncodingOptions{VideoCodec: livekit.VideoCodec_VP8, FrameRate: float64(spec.MaxFPS), Layers: []*livekit.VideoLayer{{Quality: livekit.VideoQuality_HIGH, Width: uint32(spec.MaxWidth), Height: uint32(spec.MaxHeight), Bitrate: uint32(spec.MaxBitrateKbps * 1000)}}}}},
		Audio:             &livekit.IngressAudioOptions{Name: spec.Label, Source: audioSource, EncodingOptions: &livekit.IngressAudioOptions_Options{Options: &livekit.IngressAudioEncodingOptions{AudioCodec: livekit.AudioCodec_OPUS, Bitrate: 64000, Channels: 1}}},
	})
	if err != nil {
		return nil, fmt.Errorf("calls: create resource ingress: %w", err)
	}
	return resourceIngressInfo(res)
}
func (b *liveKitResources) DeleteResourceIngress(ctx context.Context, id string) error {
	_, err := b.client.DeleteIngress(ctx, &livekit.DeleteIngressRequest{IngressId: id})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("calls: revoke resource ingress: %w", err)
	}
	return nil
}
func (s *Service) resourceMediaFor(ctx context.Context, room roomRow) (ResourceMediaBackend, error) {
	backend, err := s.backendFor(ctx, room)
	if err != nil {
		return nil, err
	}
	if capable, ok := backend.(ResourceMediaBackend); ok {
		return capable, nil
	}
	// Native media fakes/custom providers must opt in explicitly. A production
	// LiveKit backend uses the same operator-owned control URL and credentials.
	if _, ok := backend.(*liveKitBackend); !ok {
		return nil, resourceRefusal("resource_gateway_unavailable")
	}
	server, err := s.store.MediaServerByID(ctx, room.MediaServerID)
	if err != nil {
		return nil, err
	}
	if server == nil {
		return nil, resourceRefusal("resource_gateway_unavailable")
	}
	credential, err := s.credential(ctx, *server)
	if err != nil {
		return nil, err
	}
	return &liveKitResources{client: lksdk.NewIngressClient(server.APIURL, credential.Key, credential.Secret)}, nil
}
