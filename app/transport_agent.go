//go:build agent

package app

import (
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/skills"
	"github.com/znasllc-io/memql/integrations/stt"
)

// blobFetcherOrNil narrows the attachment uploader to the streaming half a
// script read needs. A checked assertion rather than a cast: the in-memory
// uploader a test wires satisfies FileUploader and streams nothing, and a
// blind cast would panic on it.
func blobFetcherOrNil(uploader server.FileUploader) skills.BlobFetcher {
	if uploader == nil {
		return nil
	}
	fetcher, ok := uploader.(skills.BlobFetcher)
	if !ok {
		return nil
	}
	return fetcher
}

// transportAgent sets up transport for an agent node.
// Includes: gRPC, WebSocket, AI HTTP, audio WS, attachments, worker.
func (a *App) transportAgent() {
	a.transportBase()

	// Worker service: depends on the gRPC server existing and the
	// engine being ready. Lands FIRST (before the agent replier)
	// so the replier can pick up the worker registry as its
	// computer_use availability source.
	a.setupWorkerService()

	// Install the agent reply handler now that the gRPC server +
	// the worker subsystem exist. Integration setup runs before
	// transport, so we can't register the handler in
	// integrationsAgent; doing it here keeps ordering clean
	// without making the grpc server a dependency of the
	// integration registration phase.
	a.setupAgentReplier()

	// STT on gRPC server
	if a.sttProvider != nil {
		a.grpcServer.SetSTTProvider(a.sttProvider.(stt.StreamingProvider))
	}

	// Audio WebSocket (shared with the voice node; see transport_audio.go).
	a.setupAudioWebsocket()

	// Attachment upload + download endpoints (mountAttachmentEndpoints lives in
	// transport_attachments.go, shared with the bff build). Returns the blob
	// uploader + container so the agent can additionally hand them to the
	// workbench / worker integrations below.
	uploader, blobContainer := a.mountAttachmentEndpoints()

	// memql#733/#801: hand the workbench integration the Azure Blob uploader so a
	// successful LOCAL fs_write uploads its bytes to v1:common:attachment
	// and the Library generatedOutput row carries a real attachmentId
	// (cluster writes stay inline pointers). Only on the agent node, only
	// when a container is configured; the integration was already
	// materialized (with SetEngine) during integrationsAgent, which runs
	// before transport. nil-safe inside the setter / promotion path.
	if uploader != nil {
		if wb := a.lookupWorkbenchIntegration(); wb != nil {
			wb.SetAttachmentUploader(uploader, blobContainer)
		}
		// memql#794: same uploader for the computer-use path so a worker
		// fs_write's bytes (which the agent already forwarded) upload to a
		// v1:common:attachment and the Library row is downloadable. Without a
		// container, computer-use rows stay worker-local pointers (memql#789).
		if wo := a.lookupWorkerIntegration(); wo != nil {
			wo.SetAttachmentUploader(uploader, blobContainer)
		}
	}

	// `runScript` / `captureScript` (epic memql#4970, spec section C). LAST of
	// the three, and the order is load-bearing: it reads the workbench's and
	// the worker's `dispatchHost` capability handlers off the registry, so
	// both integrations have to be registered first. The blob fetcher is the
	// same Azure client, which is why this needs the transport phase rather
	// than integrationsAgent.
	a.setupSkillsIntegration(blobFetcherOrNil(uploader), uploader, blobContainer)

	// AI endpoints live on MemqlService.Stream: AiChatMsg, AiSuggestMsg,
	// AiSpeechMsg, AiTranscribeMsg. The legacy /si/* HTTP endpoints are
	// gone; cross-node proxying rides AiForwardRequest.

	a.createHTTPServer()
}
