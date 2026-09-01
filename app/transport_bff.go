//go:build bff

package app

// transportBFF sets up transport for a BFF node: gRPC (MemqlService.Stream),
// the HTTP server (for ws upgrade + the auth / attachment exceptions),
// and any BFF-specific domain endpoints added by product branches.
// AI operations (chat, speech, transcribe, agent/space/group suggest)
// live on MemqlService.Stream via AiChatMsg / AiSpeechMsg / AiTranscribeMsg /
// AiSuggestMsg and are proxied across nodes by AiForwardRouter.
//
// Polyphon: the BFF also wires the LiveKit room provider so browsers
// can fetch a room token (PolyphonRoomTokenMsg) directly from the
// BFF gRPC stream they're already connected to. The score engine
// itself runs on the cognition node; the BFF just mints LiveKit
// tokens. The bridge agent is a separate container that joins the
// room independently.
func (a *App) transportBFF() {
	a.transportBase()
	a.wirePolyphonEndpoints()
	// Attachment upload + download endpoints. The bff is the frontend-facing
	// node every browser `/spaces/{id}/attachments` request routes to (nginx +
	// the Vite dev proxy point `/spaces/...` at the bff), so the handler must
	// live here, not only on the agent. Shared with the agent build in
	// transport_attachments.go. (memql#888)
	//
	// Captured (not discarded) because the atomic bundle-publish endpoint
	// below reuses the same Azure Blob client rather than constructing a
	// second one against the same account (transport_sites.go).
	uploader, container := a.mountAttachmentEndpoints()
	// Atomic bundle-publish endpoint (POST /sites/{id}/bundles, memql#3713).
	// Bff-only per the epic's controller ruling: component/edge is a
	// library here, not a node-mounted endpoint, and the edge node itself
	// has no coherent address for a site-agnostic publish route.
	a.mountSiteBundleEndpoints(uploader, container)
	// Library artifact upload + export (POST /artifacts, GET
	// /artifacts/{id}/content, memql#4341). Bff-only: the Library is a
	// user-facing surface the portal dials, and it reuses the same blob client
	// for the same reason the bundle endpoint above does.
	a.mountLibraryArtifactEndpoints(uploader, container)
	// Inbound-delivery receiver (POST /inbound/{source}, memql#2957). The
	// counterpart to the outbound worker: a third party dials US, so it is HTTP
	// on the frontend-facing node. Deny-by-default -- with no
	// MEMQL_INBOUND_SOURCE_ALLOWLIST it answers 404 to everything.
	a.mountInboundEndpoints()
	a.mountUnsubscribeEndpoint()
	// Campaign open + click tracking (GET /t/o/{token}, GET /t/c/{token},
	// memql#4823). Bff-only for the same reason the unsubscribe endpoint
	// above is: the caller is the recipient's mail client or browser, and
	// this is the node the front door already routes external traffic to.
	//
	// BEFORE createHTTPServer(), like every mount above it.
	// requireUnsealedSurface fatals on a route registered after the
	// surface is asserted, so a mount that drifts below this line does not
	// silently fail to route -- it refuses to boot.
	a.mountTrackingEndpoints()
	a.createHTTPServer()
}
