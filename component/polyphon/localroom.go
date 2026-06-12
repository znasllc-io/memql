package polyphon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/livekit/protocol/auth"
)

// LocalRoomProvider generates LiveKit JWT tokens directly in the memQL
// process. This allows browser participants to join LiveKit rooms without
// the Bridge Agent running. Room deletion (DestroyRoom) is supported via
// the LiveKit RoomService Twirp HTTP API; CreateRoom/GetRoomInfo are not
// needed (LiveKit auto-creates rooms on join) and remain unimplemented.
type LocalRoomProvider struct {
	cfg Config

	// httpClient serves the RoomService admin calls. Defaults to a
	// 10-second-timeout client; tests may swap it.
	httpClient *http.Client
}

// NewLocalRoomProvider creates a provider that mints LiveKit access tokens
// using the credentials in cfg. The caller should verify cfg.LiveKitConfigured()
// before constructing this provider.
func NewLocalRoomProvider(cfg Config) *LocalRoomProvider {
	return &LocalRoomProvider{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
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

// CreateRoom is not supported -- LiveKit auto-creates rooms when the
// first participant joins with a token minted by GenerateToken.
func (p *LocalRoomProvider) CreateRoom(_ context.Context, _ string, _ []AgentRoomConfig) (*RoomInfo, error) {
	return nil, fmt.Errorf("localroom: CreateRoom not supported (rooms are auto-created on join)")
}

// DestroyRoom deletes the LiveKit room backing the given space via the
// RoomService Twirp HTTP API (POST /twirp/livekit.RoomService/DeleteRoom).
// Room name follows the same polyphon-{spaceId} convention GenerateToken
// uses, so callers pass the space id, not the room name.
//
// Idempotent by contract: a room that does not exist (already deleted,
// never created because nobody ever joined, or LiveKit's twirp not_found)
// returns nil. This is what the daily-space rollover sweep (memql#1384)
// relies on -- multiple replicas can race the same deletion and every
// one of them succeeds.
func (p *LocalRoomProvider) DestroyRoom(ctx context.Context, spaceId string) error {
	roomName := "polyphon-" + spaceId

	base, err := livekitHTTPBaseURL(p.cfg.LiveKitURL)
	if err != nil {
		return fmt.Errorf("localroom: DestroyRoom(%q): %w", roomName, err)
	}

	// Admin token: DeleteRoom requires the roomCreate grant per the
	// LiveKit RoomService permission model.
	at := auth.NewAccessToken(p.cfg.LiveKitAPIKey, p.cfg.LiveKitAPISecret)
	at.SetVideoGrant(&auth.VideoGrant{RoomCreate: true}).
		SetIdentity("memql-room-admin").
		SetValidFor(time.Minute)
	token, err := at.ToJWT()
	if err != nil {
		return fmt.Errorf("localroom: DestroyRoom(%q): sign admin token: %w", roomName, err)
	}

	body, err := json.Marshal(map[string]string{"room": roomName})
	if err != nil {
		return fmt.Errorf("localroom: DestroyRoom(%q): marshal request: %w", roomName, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/twirp/livekit.RoomService/DeleteRoom", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("localroom: DestroyRoom(%q): build request: %w", roomName, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := p.httpClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("localroom: DestroyRoom(%q): %w", roomName, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	// Already-gone rooms are success: twirp encodes not_found as HTTP 404
	// with {"code":"not_found",...}.
	if resp.StatusCode == http.StatusNotFound || strings.Contains(string(respBody), "not_found") {
		return nil
	}
	return fmt.Errorf("localroom: DestroyRoom(%q): livekit returned %d: %s",
		roomName, resp.StatusCode, strings.TrimSpace(string(respBody)))
}

// livekitHTTPBaseURL maps the configured LiveKit WebSocket URL to the
// HTTP(S) base the Twirp RoomService listens on (same host+port,
// ws->http / wss->https). Plain http(s) URLs pass through unchanged.
func livekitHTTPBaseURL(url string) (string, error) {
	url = strings.TrimRight(strings.TrimSpace(url), "/")
	switch {
	case strings.HasPrefix(url, "ws://"):
		return "http://" + strings.TrimPrefix(url, "ws://"), nil
	case strings.HasPrefix(url, "wss://"):
		return "https://" + strings.TrimPrefix(url, "wss://"), nil
	case strings.HasPrefix(url, "http://"), strings.HasPrefix(url, "https://"):
		return url, nil
	case url == "":
		return "", fmt.Errorf("LiveKit URL not configured")
	default:
		return "", fmt.Errorf("unsupported LiveKit URL scheme: %q", url)
	}
}

// GetRoomInfo is not supported without the Bridge Agent.
func (p *LocalRoomProvider) GetRoomInfo(_ context.Context, _ string) (*RoomInfo, error) {
	return nil, fmt.Errorf("localroom: GetRoomInfo requires Bridge Agent")
}
