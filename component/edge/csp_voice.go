package edge

import (
	"net/url"
	"strings"
)

// API-enabled sites can start authenticated voice sessions through the cluster.
// LiveKit signaling uses a separate origin: both its WebSocket and HTTP probes
// must be admitted by the document, before either can reach the network. The
// edge receives the same public URL the agent puts in the join credentials.
func voiceOriginForSite(site *Site, env func(string) string) string {
	if site == nil || !site.APIProxy {
		return ""
	}
	raw := strings.TrimSpace(env("MEMQL_LIVEKIT_PUBLIC_URL"))
	if strings.HasPrefix(raw, "wss://") {
		raw = "https://" + strings.TrimPrefix(raw, "wss://")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return ""
	}
	return originOf(raw)
}
