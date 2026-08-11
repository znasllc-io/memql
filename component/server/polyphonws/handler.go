// Package polyphonws provides HTTP handlers for the Polyphon multi-agent
// voice system. The main endpoint generates LiveKit room tokens for
// browser participants to join Polyphon voice rooms.
package polyphonws

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/znasllc-io/memql/component/polyphon"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/logger"
)

// ComponentName identifies this package in logs.
const ComponentName = common.ComponentName("polyphonSession")

// Handler serves Polyphon room token and status endpoints.
//
// It once also carried an OUTBOUND call: on every room-token request it fired
// a best-effort goroutine POSTing to $MEMQL_POLYPHON_BRIDGE_AGENT_URL/join-room
// so the Bridge Agent would join the LiveKit room. Removed in memql#3453, with
// no behaviour change -- the sole construction site
// (app/transport_voice.go) passed a hardcoded "" for that URL from the file's
// first commit, so the guard never opened and the call never ran.
//
// It once also served POST /polyphon/utterance and POST /polyphon/preload,
// both of which were removed in memql#3531. Those two were registered in
// server.PublicPaths(), so they bypassed the identity verifier, and the
// utterance handler executed a graph insert under a system actor from
// caller-supplied values. Their auth bypass was justified by the Bridge
// Agent being "a trusted service running in the same Docker network" -- a
// component retired before this, and a network retired with the Compose
// stack (memql#2068 / #2088). Utterance insertion lives on the
// AUTHENTICATED gRPC stream as PolyphonUtteranceMsg
// (component/grpc/polyphon_handlers.go), which is where a service-to-service
// call belongs under the gRPC-first policy.
type Handler struct {
	scoreEngine *polyphon.ScoreEngine
	room        polyphon.RoomProvider
	logger      *slog.Logger
}

// NewHandler creates a new Polyphon handler.
// The score engine and room provider may be nil if Polyphon is not configured,
// in which case all requests return 503 Service Unavailable.
func NewHandler(scoreEngine *polyphon.ScoreEngine, room polyphon.RoomProvider) *Handler {
	return &Handler{
		scoreEngine: scoreEngine,
		room:        room,
		logger:      logger.New(ComponentName, os.Stdout, slog.LevelInfo),
	}
}

// roomTokenRequest is the JSON body for POST /polyphon/room-token.
type roomTokenRequest struct {
	PartitionId   string `json:"partitionId"`
	ParticipantId string `json:"participantId"`
	DisplayName   string `json:"displayName"`
}

// roomTokenResponse is returned to the frontend.
type roomTokenResponse struct {
	Token      string `json:"token"`
	RoomName   string `json:"roomName"`
	LiveKitURL string `json:"livekitUrl"`
	ExpiresAt  int64  `json:"expiresAt"`
}

// statusResponse is returned by GET /polyphon/status.
type statusResponse struct {
	Enabled        bool   `json:"enabled"`
	ActiveSessions int    `json:"activeSessions"`
	Platform       string `json:"platform"`
}

// ServeRoomToken handles POST /polyphon/room-token requests.
// Creates or joins a LiveKit room and returns a token for the browser.
func (h *Handler) ServeRoomToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if h.scoreEngine == nil || h.room == nil {
		http.Error(w, "polyphon not configured", http.StatusServiceUnavailable)
		return
	}

	var req roomTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if strings.TrimSpace(req.PartitionId) == "" || strings.TrimSpace(req.ParticipantId) == "" {
		http.Error(w, "partitionId and participantId are required", http.StatusBadRequest)
		return
	}

	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		displayName = "Participant"
	}

	// Generate a room token via the room provider.
	token, err := h.room.GenerateToken(r.Context(), req.PartitionId, req.ParticipantId, displayName)
	if err != nil {
		h.logger.Error("failed to generate room token",
			"partitionId", req.PartitionId,
			"participantId", req.ParticipantId,
			"error", err,
		)
		http.Error(w, "failed to generate room token", http.StatusBadGateway)
		return
	}

	resp := roomTokenResponse{
		Token:      token.Token,
		RoomName:   token.RoomName,
		LiveKitURL: token.LiveKitURL,
		ExpiresAt:  token.ExpiresAt,
	}

	h.logger.Debug("polyphon room token generated",
		"partitionId", req.PartitionId,
		"participantId", req.ParticipantId,
		"roomName", token.RoomName,
	)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.logger.Error("failed to encode response", "error", err)
	}
}

// ServeStatus handles GET /polyphon/status requests.
// Returns whether the score engine is active and LiveKit transport is available.
func (h *Handler) ServeStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	resp := statusResponse{
		Enabled:        h.scoreEngine != nil && h.room != nil,
		ActiveSessions: 0,
		Platform:       string(polyphon.PlatformOpenAI),
	}

	if h.scoreEngine != nil {
		resp.ActiveSessions = h.scoreEngine.Sessions().ActiveSessions()
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.logger.Error("failed to encode response", "error", err)
	}
}
