//go:build agent

package app

import (
	"context"

	"github.com/znasllc-io/memql/component/fileprocessor"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/gcs"
	"github.com/znasllc-io/memql/integrations/stt"
)

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

	// Attachment upload endpoints
	var uploader server.FileUploader
	gcsBucket := gcs.BucketFromEnv()
	if gcsBucket != "" {
		gcsClient, err := gcs.New(context.Background())
		if err != nil {
			a.Logger.Warn("GCS uploader unavailable", "error", err)
		} else {
			uploader = gcsClient
		}
	}
	processor := fileprocessor.NewDefaultProcessor(a.engine.VisionProvider())
	engineAdapter := &AttachmentEngineAdapter{Engine: a.engine}
	store := server.NewEngineAttachmentStore(engineAdapter)
	planStore := server.NewEnginePlanStore(engineAdapter)
	attachmentHandler := server.NewAttachmentHandler(server.AttachmentHandlerOptions{
		Logger:    a.Logger,
		Bucket:    gcsBucket,
		Uploader:  uploader,
		Extractor: processor,
		Store:     store,
		PlanStore: planStore,
	})
	for _, path := range server.SpaceAttachmentPaths() {
		a.mux.Handle("POST "+path, attachmentHandler)
	}

	// AI endpoints live on MemqlService.Stream: SIChatMsg, SISuggestMsg,
	// SISpeechMsg, SITranscribeMsg. The legacy /si/* HTTP endpoints are
	// gone; cross-node proxying rides SIForwardRequest.

	a.createHTTPServer()
}
