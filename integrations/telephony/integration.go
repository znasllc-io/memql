package telephony

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/znasllc-io/memql/component/memql"
)

// Integration is the telephony IntegrationProvider. It owns the carrier
// (CarrierProvider) for DID lifecycle and the LiveKit SIP edge clients for
// trunk / dispatch-rule / participant management, and writes telephony rows
// (numbers, trunks, calls) through the engine.
//
// Telephony is core and product-agnostic: calls and numbers bind to a generic
// partition, never a CoPresent space (Amendment A).
type Integration struct {
	logger  *slog.Logger
	engine  memql.IntegrationEngineAccess
	carrier CarrierProvider

	// LiveKit edge clients. Nil when LiveKit creds are absent (telephony not
	// configured) -- capabilities then return a clear "not configured" error
	// rather than panicking, so the plug-in loads cleanly everywhere.
	sip  *lksdk.SIPClient
	room *lksdk.RoomServiceClient

	// sipEdgeURI is the carrier-reachable SIP signaling target the carrier
	// points DIDs at (e.g. "sip:edge.example.com:5061;transport=tls").
	sipEdgeURI string

	// calls tracks connected telephony legs between webhook join/leave so a
	// single append-only call row carries the real duration.
	calls *callTracker
}

// Config holds the telephony integration's resolved runtime settings.
type Config struct {
	// CarrierName selects the active carrier (MEMQL_TELEPHONY_CARRIER).
	CarrierName string
	// LiveKitURL is the ws(s):// or http(s):// LiveKit server URL.
	LiveKitURL string
	// LiveKitAPIKey / LiveKitAPISecret authenticate to the LiveKit SIP +
	// RoomService APIs (the shared POLYPHON_LIVEKIT_API_* pair).
	LiveKitAPIKey    string
	LiveKitAPISecret string
	// SIPEdgeURI is the carrier-reachable SIP signaling target.
	SIPEdgeURI string
}

// New builds a telephony Integration. The carrier is resolved eagerly (its
// own credential errors surface here); the LiveKit SIP/Room clients are built
// only when creds are present, so a cluster without telephony configured still
// loads the plug-in.
func New(cfg Config, logger *slog.Logger) (*Integration, error) {
	carrier, err := SelectCarrier(cfg.CarrierName)
	if err != nil {
		return nil, err
	}
	i := &Integration{
		logger:     logger,
		carrier:    carrier,
		sipEdgeURI: cfg.SIPEdgeURI,
	}
	if cfg.LiveKitURL != "" && cfg.LiveKitAPIKey != "" && cfg.LiveKitAPISecret != "" {
		httpURL := httpLiveKitURL(cfg.LiveKitURL)
		i.sip = lksdk.NewSIPClient(httpURL, cfg.LiveKitAPIKey, cfg.LiveKitAPISecret)
		i.room = lksdk.NewRoomServiceClient(httpURL, cfg.LiveKitAPIKey, cfg.LiveKitAPISecret)
	} else if logger != nil {
		logger.Warn("telephony: LiveKit SIP creds absent; trunk/call capabilities disabled until configured")
	}
	return i, nil
}

// SetEngine wires the engine used to read/write telephony rows.
func (i *Integration) SetEngine(e memql.IntegrationEngineAccess) { i.engine = e }

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return "telephony" }

// Carrier exposes the active carrier (used by the provisioning surface).
func (i *Integration) Carrier() CarrierProvider { return i.carrier }

// requireSIP returns the SIP client or a clear configuration error.
func (i *Integration) requireSIP() (*lksdk.SIPClient, error) {
	if i.sip == nil {
		return nil, fmt.Errorf("telephony: LiveKit SIP not configured (set LIVEKIT_URL + POLYPHON_LIVEKIT_API_KEY/SECRET)")
	}
	return i.sip, nil
}

// requireEngine returns the engine or a clear error.
func (i *Integration) requireEngine() (memql.IntegrationEngineAccess, error) {
	if i.engine == nil {
		return nil, fmt.Errorf("telephony: engine not wired")
	}
	return i.engine, nil
}

// httpLiveKitURL converts a ws(s):// LiveKit URL to the http(s):// form the
// SIP/RoomService (Twirp/HTTP) clients expect; passes through http(s).
func httpLiveKitURL(u string) string {
	switch {
	case strings.HasPrefix(u, "wss://"):
		return "https://" + strings.TrimPrefix(u, "wss://")
	case strings.HasPrefix(u, "ws://"):
		return "http://" + strings.TrimPrefix(u, "ws://")
	default:
		return u
	}
}

var _ memql.IntegrationProvider = (*Integration)(nil)

// Capabilities implements memql.IntegrationProvider. The inbound slice (4.4)
// registers trunk/dispatch provisioning + call-record writers; later slices
// extend this list (outbound tools 4.5, provisioning 4.6).
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return i.inboundCapabilities()
}

// asString coerces a tool/builtin arg to a trimmed string.
func asString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// ctxNote is a tiny helper to keep handler signatures uniform.
type handlerCtx = context.Context
