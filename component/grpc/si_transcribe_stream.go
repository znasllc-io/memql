package memql

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/integrations/stt"
)

// transcribeStream is the per-request worker-side state for a streaming
// transcription session. It lives on service.transcribeStreams keyed by
// the caller's request_id, not on a streamSession, because on the
// forwarded-request path each envelope (Start, each Chunk, End) lands
// in its own streamSession but all must cooperate on the same STT
// provider session and share the same outbound wire (forwardedStream).
type transcribeStream struct {
	requestId string
	correlate string // original envelope.MessageId -- the correlate_to for replies
	provider  string
	startTime time.Time

	// sttSession is the provider-side streaming session. SendAudio is
	// called from Chunk handlers; Finalize from End; Close from cancel.
	sttSession stt.StreamingSession

	// send captures the outbound wire at Start time. On the
	// direct-client path this is streamSession.sendServerMessage wrapped
	// in a function that preserves correlate_to; on the forwarded path
	// it is forwardedStream.Send, which re-wraps each
	// MemqlServerMessage into an SIForwardResponse. Using the SAME
	// send for deltas and the final Complete preserves ordering and
	// lets isTerminalServerPayload close the inflight only on Complete.
	send func(*memqlv1.MemqlServerMessage) error

	ctx    context.Context
	cancel context.CancelFunc

	// done is closed by the pump goroutine after it has drained the
	// provider's receive channel. End handlers wait on this before
	// sending Complete so no Delta can race past the terminal message.
	done chan struct{}

	// lastText dedupes consecutive identical deltas (the OpenAI Whisper
	// session emits a single "final" Text on Finalize; streaming
	// providers emit accumulating text -- both flows want to skip
	// duplicates). Guarded by mu.
	mu       sync.Mutex
	lastText string
	closed   bool
}

// handleAiTranscribeStreamStart opens a streaming transcription
// session on the local STT provider (or proxies to a Voice worker when
// running on a BFF) and keeps the session state in service.transcribeStreams.
// Subsequent Chunk / End envelopes find the session by request_id.
func (s *streamSession) handleAiTranscribeStreamStart(
	envelope *memqlv1.MemqlClientMessage,
	msg *memqlv1.SITranscribeStreamStart,
) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument,
			"si_transcribe_stream_start request missing")
	}

	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())

	// BFF short-circuit: open a forwarded stream to a Voice worker.
	// The Chunk/End envelopes will land on BFF later and flow through
	// ForwardContinuation on the same inflight entry.
	if s.shouldProxySI(nodeTargetForTranscribe()) {
		return s.proxySI(envelope, requestId, nodeTargetForTranscribe())
	}

	// Local path: open the STT session here.
	if s.service.sttProvider == nil {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.Unavailable,
			"streaming transcription is not configured")
	}

	// Duplicate-start detection: an existing session for the same
	// request_id is almost certainly a client bug (or a replay of a
	// message the server already acted on). Reject loudly.
	if s.service.lookupTranscribeStream(requestId) != nil {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.AlreadyExists,
			"streaming transcription already open for request_id")
	}

	sttCfg := stt.StreamConfig{
		Format:       pickNonEmpty(msg.GetFormat(), "pcm16"),
		SampleRate:   pickPositiveInt(int(msg.GetSampleRate()), 16000),
		Channels:     pickPositiveInt(int(msg.GetChannels()), 1),
		LanguageHint: msg.GetLanguageHint(),
	}

	// Use a long-lived context so the STT provider's goroutines outlive
	// any per-envelope dispatch. The cancel is stored on transcribeStream
	// and fires on End or on cleanupTranscribeStreams.
	sessCtx, cancel := context.WithCancel(context.Background())
	sttSess, err := s.service.sttProvider.StartStream(sessCtx, sttCfg)
	if err != nil {
		cancel()
		s.logger.Warn("streaming transcription start failed",
			"request_id", requestId, "error", err)
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.Internal,
			"failed to start streaming transcription: "+err.Error())
	}

	ts := &transcribeStream{
		requestId:  requestId,
		correlate:  envelope.GetMessageId(),
		provider:   s.service.sttProvider.Name(),
		startTime:  time.Now(),
		sttSession: sttSess,
		send: func(m *memqlv1.MemqlServerMessage) error {
			return s.stream.Send(m)
		},
		ctx:    sessCtx,
		cancel: cancel,
		done:   make(chan struct{}),
	}

	s.service.registerTranscribeStream(requestId, ts)
	go ts.pumpDeltas(s.logger)

	s.logger.Info("streaming transcription started",
		"request_id", requestId,
		"provider", ts.provider,
		"format", sttCfg.Format,
		"sample_rate", sttCfg.SampleRate,
	)
	return nil
}

// handleAiTranscribeStreamChunk forwards an audio chunk to an existing
// streaming session identified by request_id.
func (s *streamSession) handleAiTranscribeStreamChunk(
	envelope *memqlv1.MemqlClientMessage,
	msg *memqlv1.SITranscribeStreamChunk,
) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument,
			"si_transcribe_stream_chunk request missing")
	}
	requestId := strings.TrimSpace(msg.GetRequestId())
	if requestId == "" {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument,
			"chunk envelope missing request_id")
	}

	// BFF proxy: forward the chunk to the same worker Start opened a
	// stream on. If no inflight exists, the start never went through
	// (or already closed) -- surface that as InvalidArgument.
	if s.shouldProxySI(nodeTargetForTranscribe()) {
		if !s.service.aiForwarder.HasInflight(requestId) {
			return s.sendQueryError(requestId, envelope.GetMessageId(), codes.FailedPrecondition,
				"no open transcribe stream for request_id (call Start first)")
		}
		return s.service.aiForwarder.ForwardContinuation(
			requestId,
			s.forwardedAuthClaims(),
			extractPartitionFromEnvelope(envelope),
			envelope,
		)
	}

	// Local path.
	ts := s.service.lookupTranscribeStream(requestId)
	if ts == nil {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.FailedPrecondition,
			"no open transcribe stream for request_id (call Start first)")
	}

	audio := msg.GetAudio()
	if len(audio) == 0 {
		return nil // silently drop empty chunks -- clients occasionally send them
	}
	if err := ts.sttSession.SendAudio(audio); err != nil {
		s.logger.Warn("streaming transcription chunk rejected",
			"request_id", requestId, "error", err)
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.Internal,
			"failed to forward audio chunk: "+err.Error())
	}
	return nil
}

// handleAiTranscribeStreamEnd finalizes a streaming session, emits the
// terminal Complete message, and removes the session from the service
// registry. If msg.Cancel is true the session is aborted without
// emitting Complete.
func (s *streamSession) handleAiTranscribeStreamEnd(
	envelope *memqlv1.MemqlClientMessage,
	msg *memqlv1.SITranscribeStreamEnd,
) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument,
			"si_transcribe_stream_end request missing")
	}
	requestId := strings.TrimSpace(msg.GetRequestId())
	if requestId == "" {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument,
			"end envelope missing request_id")
	}

	// BFF proxy: forward End to the same worker; the worker will emit
	// SITranscribeStreamComplete (or close silently on cancel) and the
	// BFF's inflight entry closes on the terminal response.
	if s.shouldProxySI(nodeTargetForTranscribe()) {
		if !s.service.aiForwarder.HasInflight(requestId) {
			return s.sendQueryError(requestId, envelope.GetMessageId(), codes.FailedPrecondition,
				"no open transcribe stream for request_id (call Start first)")
		}
		return s.service.aiForwarder.ForwardContinuation(
			requestId,
			s.forwardedAuthClaims(),
			extractPartitionFromEnvelope(envelope),
			envelope,
		)
	}

	// Local path.
	ts := s.service.unregisterTranscribeStream(requestId)
	if ts == nil {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.FailedPrecondition,
			"no open transcribe stream for request_id (call Start first)")
	}

	if msg.GetCancel() {
		// Emit a terminal Complete with empty text so the BFF-side
		// inflight closes promptly. Wait for the pump briefly so any
		// in-flight Delta doesn't arrive AFTER Complete.
		ts.waitForPumpDone(500 * time.Millisecond)
		cancelMsg := &memqlv1.MemqlServerMessage{
			CorrelateTo: ts.correlate,
			Payload: &memqlv1.MemqlServerMessage_SITranscribeStreamComplete{
				SITranscribeStreamComplete: &memqlv1.SITranscribeStreamComplete{
					RequestId:  requestId,
					Text:       "",
					DurationMs: time.Since(ts.startTime).Milliseconds(),
					Provider:   ts.provider,
				},
			},
		}
		if err := ts.send(cancelMsg); err != nil {
			s.logger.Debug("streaming transcription cancel-complete send failed",
				"request_id", requestId, "error", err)
		}
		ts.closeSilently()
		s.logger.Info("streaming transcription cancelled",
			"request_id", requestId,
			"provider", ts.provider,
		)
		return nil
	}

	// Not cancelled: finalize on a background context so the provider
	// HTTP call isn't tied to the NodeService stream's short-lived
	// per-envelope ctx.
	finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer finalizeCancel()
	final, err := ts.sttSession.Finalize(finalizeCtx)
	if err != nil && !errors.Is(err, context.Canceled) {
		s.logger.Warn("streaming transcription finalize failed",
			"request_id", requestId, "error", err)
		ts.closeSilently()
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.Internal,
			"failed to finalize streaming transcription: "+err.Error())
	}

	// Let the pump drain any in-flight deltas the provider might still
	// be pushing; this is bounded by the provider's own Finalize
	// timeout + a small grace.
	ts.waitForPumpDone(2 * time.Second)

	text := ""
	var durationMs int64
	if final != nil {
		text = final.Text
		durationMs = final.DurationMS
	}
	if durationMs == 0 {
		durationMs = time.Since(ts.startTime).Milliseconds()
	}

	complete := &memqlv1.MemqlServerMessage{
		CorrelateTo: ts.correlate,
		Payload: &memqlv1.MemqlServerMessage_SITranscribeStreamComplete{
			SITranscribeStreamComplete: &memqlv1.SITranscribeStreamComplete{
				RequestId:  requestId,
				Text:       text,
				DurationMs: durationMs,
				Provider:   ts.provider,
			},
		},
	}
	if err := ts.send(complete); err != nil {
		s.logger.Warn("streaming transcription complete send failed",
			"request_id", requestId, "error", err)
	}

	ts.cancel()
	s.logger.Info("streaming transcription completed",
		"request_id", requestId,
		"provider", ts.provider,
		"text_len", len(text),
		"duration_ms", durationMs,
	)
	return nil
}

// pumpDeltas drains the STT provider's interim result channel and
// relays each as an SITranscribeStreamDelta. Exits when the provider
// closes its Receive channel (Finalize / Close). Duplicate consecutive
// texts are skipped: providers that emit accumulated text would
// otherwise send the same final sentence again on every "utterance
// end" heartbeat.
func (ts *transcribeStream) pumpDeltas(logger interface {
	Warn(msg string, args ...any)
}) {
	defer close(ts.done)

	for result := range ts.sttSession.Receive() {
		text := strings.TrimSpace(result.Text)
		if text == "" {
			continue
		}

		ts.mu.Lock()
		if text == ts.lastText {
			ts.mu.Unlock()
			continue
		}
		ts.lastText = text
		ts.mu.Unlock()

		delta := &memqlv1.MemqlServerMessage{
			CorrelateTo: ts.correlate,
			Payload: &memqlv1.MemqlServerMessage_SITranscribeStreamDelta{
				SITranscribeStreamDelta: &memqlv1.SITranscribeStreamDelta{
					RequestId:  ts.requestId,
					Text:       text,
					IsFinal:    result.IsFinal,
					Confidence: result.Confidence,
				},
			},
		}
		if err := ts.send(delta); err != nil {
			// Stream to the client has dropped; downstream close will
			// tear everything down on its own.
			if logger != nil {
				logger.Warn("streaming transcription delta send failed",
					"request_id", ts.requestId, "error", err)
			}
			return
		}
	}
}

// waitForPumpDone blocks until the pump goroutine exits or the timeout
// elapses. Called from the End handler before sending Complete so we
// know no Delta is still in flight.
func (ts *transcribeStream) waitForPumpDone(timeout time.Duration) {
	if ts == nil {
		return
	}
	select {
	case <-ts.done:
	case <-time.After(timeout):
	}
}

// closeSilently cancels the STT session and context without emitting
// any further messages. Used for cancel-flagged End and for fault
// paths.
func (ts *transcribeStream) closeSilently() {
	if ts == nil {
		return
	}
	ts.mu.Lock()
	if ts.closed {
		ts.mu.Unlock()
		return
	}
	ts.closed = true
	ts.mu.Unlock()

	if ts.sttSession != nil {
		_ = ts.sttSession.Close()
	}
	if ts.cancel != nil {
		ts.cancel()
	}
}

// -----------------------------------------------------------------------------
// service registry helpers
// -----------------------------------------------------------------------------

func (s *service) registerTranscribeStream(requestId string, ts *transcribeStream) {
	s.transcribeStreamsMu.Lock()
	defer s.transcribeStreamsMu.Unlock()
	if s.transcribeStreams == nil {
		s.transcribeStreams = make(map[string]*transcribeStream)
	}
	s.transcribeStreams[requestId] = ts
}

func (s *service) lookupTranscribeStream(requestId string) *transcribeStream {
	s.transcribeStreamsMu.Lock()
	defer s.transcribeStreamsMu.Unlock()
	return s.transcribeStreams[requestId]
}

// unregisterTranscribeStream atomically removes the session for the
// given request_id and returns the removed entry (or nil). The caller
// owns cleanup; use the returned transcribeStream's closeSilently or
// explicit Finalize.
func (s *service) unregisterTranscribeStream(requestId string) *transcribeStream {
	s.transcribeStreamsMu.Lock()
	defer s.transcribeStreamsMu.Unlock()
	ts, ok := s.transcribeStreams[requestId]
	if !ok {
		return nil
	}
	delete(s.transcribeStreams, requestId)
	return ts
}

// -----------------------------------------------------------------------------
// small helpers
// -----------------------------------------------------------------------------

func pickNonEmpty(primary, fallback string) string {
	if strings.TrimSpace(primary) != "" {
		return primary
	}
	return fallback
}

func pickPositiveInt(primary, fallback int) int {
	if primary > 0 {
		return primary
	}
	return fallback
}

// init asserts the streaming handlers stay in sync with the
// single-shot handler on where transcription is routed. If
// nodeTargetForTranscribe ever stops returning NodeTypeVoice the BFF
// proxy path here would silently stop working.
func init() {
	if nodeTargetForTranscribe() != node.NodeTypeVoice {
		panic("nodeTargetForTranscribe must resolve to NodeTypeVoice; the streaming transcription handlers rely on it")
	}
}
