//go:build agent

package app

import (
	"github.com/znasllc-io/memql/component/server"
	agentworker "github.com/znasllc-io/memql/integrations/agent/worker"
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

	// Azure Blob uploader + container (resolveBlobStore lives in
	// transport_blobstore.go, shared with the bff build). Returns the blob
	// uploader + container so the agent can additionally hand them to the
	// workbench / worker integrations below.
	uploader, blobContainer := a.resolveBlobStore()
	// Materializer automation steps execute here, with this node's own
	// registered integration, AI router and shared Library blob container.
	a.wireComposeIntegration(uploader, blobContainer)

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

	// THE VISION HALF OF THE APP DOOR (issue memql#5523). A vision call
	// through an app is an image ON THAT MACHINE'S DISK and a prompt that
	// names it, so the engine has to land the bytes somewhere the cockpit can
	// pull them from -- which is the Library, which is this blob container.
	//
	// Wired HERE and not beside the door itself because the blob store is a
	// transport-phase resolution. Without a container the door still serves
	// chat and structured chat, and a vision call refuses by name rather than
	// running without its images.
	if ai, ok := a.appInference.(*agentworker.AppInference); ok && ai != nil {
		if uploader != nil {
			ai.SetVisionStager(a.engine, uploader, blobContainer)
			a.Logger.Info("app door: vision calls can stage images into a session workspace",
				"container", blobContainer)
		} else {
			a.Logger.Warn("app door: no blob storage on this replica, so a vision call through an " +
				"app will be refused rather than sent without its images")
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
