package polyphon

import (
	"context"
	"fmt"
	"time"

	"github.com/livekit/protocol/auth"
)

// LocalRoomProvider generates LiveKit JWT tokens directly in the memQL
// process. This allows browser participants to join LiveKit rooms without
// the Bridge Agent running. Room/agent management operations are not
// supported -- they require the Bridge Agent.
type LocalRoomProvider struct {
	cfg Config
}

// NewLocalRoomProvider creates a provider that mints LiveKit access tokens
// using the credentials in cfg. The caller should verify cfg.LiveKitConfigured()
// before constructing this provider.
func NewLocalRoomProvider(cfg Config) *LocalRoomProvider {
	return &LocalRoomProvider{cfg: cfg}
}

// GenerateToken creates a LiveKit JWT for a participant to join a room.
// Room name follows the convention polyphon-{spaceId}.
func (p *LocalRoomProvider) GenerateToken(_ context.Context, spaceId, participantId, displayName string) (*RoomToken, error) {
	roomName := "polyphon-" + spaceId

	at := auth.NewAccessToken(p.cfg.LiveKitAPIKey, p.cfg.LiveKitAPISecret)
	grant := &auth.VideoGrant{
		RoomJoin: true,
		Room:     roomName,
	}
	at.SetVideoGrant(grant).
		SetIdentity(participantId).
		SetName(displayName).
		SetValidFor(24 * time.Hour)

	token, err := at.ToJWT()
	if err != nil {
		return nil, fmt.Errorf("localroom: sign token: %w", err)
	}

	// Use the public URL so the browser can reach LiveKit.
	// Falls back to the internal URL if no public URL is configured.
	livekitURL := p.cfg.LiveKitPublicURL
	if livekitURL == "" {
		livekitURL = p.cfg.LiveKitURL
	}

	return &RoomToken{
		Token:      token,
		RoomName:   roomName,
		LiveKitURL: livekitURL,
		ExpiresAt:  time.Now().Add(24 * time.Hour).Unix(),
	}, nil
}

// CreateRoom is not supported without the Bridge Agent.
func (p *LocalRoomProvider) CreateRoom(_ context.Context, _ string, _ []AgentRoomConfig) (*RoomInfo, error) {
	return nil, fmt.Errorf("localroom: CreateRoom requires Bridge Agent")
}

// DestroyRoom is not supported without the Bridge Agent.
func (p *LocalRoomProvider) DestroyRoom(_ context.Context, _ string) error {
	return fmt.Errorf("localroom: DestroyRoom requires Bridge Agent")
}

// GetRoomInfo is not supported without the Bridge Agent.
func (p *LocalRoomProvider) GetRoomInfo(_ context.Context, _ string) (*RoomInfo, error) {
	return nil, fmt.Errorf("localroom: GetRoomInfo requires Bridge Agent")
}
