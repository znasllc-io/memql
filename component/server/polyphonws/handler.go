// Package polyphonws provides HTTP handlers for the Polyphon multi-agent
// voice system. The main endpoint generates LiveKit room tokens for
// browser participants to join Polyphon voice rooms.
package polyphonws

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/polyphon"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/logger"
)

// ComponentName identifies this package in logs.
const ComponentName = common.ComponentName("polyphonSession")

// MemQLExecutor executes MemQL queries (used for Bridge Agent utterance insertion).
type MemQLExecutor interface {
	Execute(ctx context.Context, query string) (any, error)
}

// Handler serves Polyphon room token and status endpoints.
type Handler struct {
	scoreEngine    *polyphon.ScoreEngine
	room           polyphon.RoomProvider
	engine         MemQLExecutor
	bridgeAgentURL string // HTTP base URL of the Bridge Agent (e.g., "http://bridge-agent:50052")
	logger         *slog.Logger

	// prewarmFunc, when set by the cognition-bearing binary, is
	// called by ServePreload to warm per-space caches the moment
	// the bridge sees the user start speaking. Skipped on binaries
	// without cognition -- they 204 the endpoint.
	prewarmFunc func(partitionId string)
}

// SetPrewarmFunc wires a callback that the bridge agent triggers via
// POST /polyphon/preload when the user starts speaking. Implemented
// on cognition-bearing binaries; nil-set on the rest. Lets the
// handler stay decoupled from the cognition package.
func (h *Handler) SetPrewarmFunc(fn func(partitionId string)) {
	if h == nil {
		return
	}
	h.prewarmFunc = fn
}

// NewHandler creates a new Polyphon handler.
// The score engine and room provider may be nil if Polyphon is not configured,
// in which case all requests return 503 Service Unavailable.
// bridgeAgentURL is optional; if set, the handler notifies the Bridge Agent when tokens are generated.
// engine is optional; if set, enables the /polyphon/utterance endpoint for Bridge Agent utterance insertion.
func NewHandler(scoreEngine *polyphon.ScoreEngine, room polyphon.RoomProvider, bridgeAgentURL string, engine MemQLExecutor) *Handler {
	return &Handler{
		scoreEngine:    scoreEngine,
		room:           room,
		engine:         engine,
		bridgeAgentURL: strings.TrimRight(bridgeAgentURL, "/"),
		logger:         logger.New(ComponentName, os.Stdout, slog.LevelInfo),
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

	// Best-effort: notify Bridge Agent to join the room.
	if h.bridgeAgentURL != "" {
		go h.notifyBridgeAgent(req.PartitionId, token.RoomName)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.logger.Error("failed to encode response", "error", err)
	}
}

// notifyBridgeAgent sends a best-effort POST to the Bridge Agent so it joins
// the LiveKit room. If the Bridge Agent is unreachable, the error is logged
// and the token is still returned to the browser.
func (h *Handler) notifyBridgeAgent(partitionId, roomName string) {
	body, _ := json.Marshal(map[string]string{
		"partitionId": partitionId,
		"roomName":    roomName,
	})

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(h.bridgeAgentURL+"/join-room", "application/json", bytes.NewReader(body))
	if err != nil {
		h.logger.Debug("polyphon: bridge agent unreachable (non-fatal)",
			"bridgeAgentURL", h.bridgeAgentURL,
			"error", err,
		)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		h.logger.Warn("polyphon: bridge agent returned error",
			"status", resp.StatusCode,
			"partitionId", partitionId,
		)
		return
	}

	h.logger.Info("polyphon: bridge agent notified to join room",
		"partitionId", partitionId,
		"roomName", roomName,
	)
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

// preloadRequest is the JSON body for POST /polyphon/preload.
type preloadRequest struct {
	PartitionId string `json:"partitionId"`
}

// ServePreload handles POST /polyphon/preload from the bridge agent.
// Bridge fires this the moment it sees the user start producing
// speech-classified RTP packets so the cognition node can warm its
// per-space caches in parallel with ASR transcription. By the time
// the final transcript lands and the actual utterance handler runs,
// the caches are already hot and handlerOffsetMs drops measurably.
//
// 202 Accepted always (best-effort, no contract). 204 No Content
// when no prewarm callback is wired (e.g., BFF-only binaries).
func (h *Handler) ServePreload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.prewarmFunc == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var req preloadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.PartitionId) == "" {
		http.Error(w, "partitionId is required", http.StatusBadRequest)
		return
	}
	h.prewarmFunc(req.PartitionId)
	w.WriteHeader(http.StatusAccepted)
}

// utteranceRequest is the JSON body for POST /polyphon/utterance.
type utteranceRequest struct {
	PartitionId   string `json:"partitionId"`
	ParticipantId string `json:"participantId"`
	Text          string `json:"text"`
}

// ServeUtterance handles POST /polyphon/utterance requests.
// This is an internal endpoint called by the Bridge Agent to insert
// human utterances from the Polyphon voice pipeline. It bypasses
// standard auth (registered as a public path) since the Bridge Agent
// is a trusted service running in the same Docker network.
func (h *Handler) ServeUtterance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if h.engine == nil {
		http.Error(w, "engine not configured", http.StatusServiceUnavailable)
		return
	}

	var req utteranceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if strings.TrimSpace(req.PartitionId) == "" || strings.TrimSpace(req.ParticipantId) == "" || strings.TrimSpace(req.Text) == "" {
		http.Error(w, "partitionId, participantId, and text are required", http.StatusBadRequest)
		return
	}

	// The bridge passes ParticipantId in canonical form
	// (default:v1:cognition:participant:participant-XXX). Building the
	// utterance id by gluing that whole string into "utt-{participantId}-{ts}"
	// produces a node id with internal `:v1:cognition:participant:`
	// segments, which the engine's ParseNodeId can't disambiguate when
	// downstream queries resolve ids -- the row lands in the DB and
	// fires graph events (so cognition reacts) but the same-context
	// spaceUtterances() cannot find it. The greeting works because
	// autoJoinAI's mutation hashes the inputs into a bare slug; the
	// polyphon HTTP path bypasses that mutation and built the id by
	// raw concatenation.
	//
	// Fix: strip ParticipantId to its bare slug before concatenating.
	// The resulting id has no internal colons and round-trips through
	// ParseNodeId + canonicalId() cleanly.
	bareParticipantId := req.ParticipantId
	if idx := strings.LastIndex(bareParticipantId, ":"); idx >= 0 {
		bareParticipantId = bareParticipantId[idx+1:]
	}
	utteranceId := fmt.Sprintf("utt-%s-%d", bareParticipantId, time.Now().UnixNano())
	textJSON, _ := json.Marshal(req.Text)

	query := fmt.Sprintf(`insert("%s", id="%s", payload={
		"partitionId": "%s",
		"participantId": "%s",
		"participantType": "human",
		"utteranceType": "text",
		"text": %s,
		"source": {
			"inputMethod": "stt",
			"pipeline": "polyphon"
		}
	})`, memoryNodes.ConceptCognitionUtterance, utteranceId, req.PartitionId, req.ParticipantId, string(textJSON))

	ctx := contextWithSystemActor(context.Background())

	if _, err := h.engine.Execute(ctx, query); err != nil {
		h.logger.Error("polyphon utterance insert failed",
			"error", err,
			"partitionId", req.PartitionId,
		)
		http.Error(w, "failed to insert utterance", http.StatusInternalServerError)
		return
	}

	h.logger.Info("polyphon utterance inserted",
		"partitionId", req.PartitionId,
		"participantId", req.ParticipantId,
		"utteranceId", utteranceId,
	)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"utteranceId": utteranceId})
}

// contextWithSystemActor injects a system actor identity into the context
// so the MemQL engine can execute queries on behalf of the Bridge Agent.
func contextWithSystemActor(ctx context.Context) context.Context {
	claims := map[string]any{
		"sub":   "polyphon-bridge-agent",
		"email": "polyphon-bridge-agent@memql.internal",
		"role":  "system",
	}
	token := auth.BuildTokenInfo(claims)
	ctx = auth.ContextWithClaims(ctx, claims)
	return auth.ContextWithToken(ctx, token)
}
