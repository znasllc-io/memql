//go:build agent

package app

import (
	"context"
	"net/http"

	"github.com/znasllc-io/memql/component/fileprocessor"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/component/server/audiows"
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

	// Audio WebSocket
	if a.sttProvider != nil {
		sttProv := a.sttProvider.(stt.StreamingProvider)
		ttsProvider := a.engine.TTSProvider()

		audioHandler, err := audiows.New(audiows.Options{
			Logger:      a.Logger,
			STTProvider: sttProv,
			TTSProvider: ttsProvider,
			Engine:      &AudioEngineAdapter{Engine: a.engine},
		})
		if err != nil {
			a.fatal("failed to initialize audio websocket handler", "error", err)
		}
		for _, path := range server.AudioWebsocketPaths() {
			a.mux.Handle("GET "+path, http.HandlerFunc(audioHandler.ServeHTTP))
		}
		a.Logger.Info("audio websocket enabled",
			"paths", server.AudioWebsocketPaths(),
			"sttProvider", sttProv.Name(),
		)
	}

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
