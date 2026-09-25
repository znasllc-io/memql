package memql

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/znasllc-io/memql/core/id"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/core/common"
)

// Streaming cutover onto the durable delivery substrate (memql#1266, Phase 2 of
// epic memql#1259). This file is the gRPC-handler seam that points the two live
// streaming paths -- token streaming (handleAiChatStream) and audio streaming
// (ai_transcribe_stream.go) -- at the node-library StreamSession /
// SubscribeStreamFrames primitives (component/node/stream_lifecycle.go).
//
// Topology, mirroring the chat-reply migration (memql#1264) and the client-tool
// RPC migration (memql#1265): the TRIGGER stays on the existing mesh forward
// (the bff still Forward()s the request to the worker), but the streamed
// RESPONSE delivery moves onto the substrate, addressed by the logical key
// stream:<requestId>:
//
//   - Producer (worker -- agent for token, voice for audio): instead of pushing
//     each AiStreamChunk / AiTranscribeStreamDelta + the terminal back over the
//     forwardedStream (the ad-hoc AiForwardResponse mesh push that the star-mesh
//     bug drops/duplicates across replicas), the handler opens a StreamSession on
//     stream:<requestId> and emits Start -> Delta* -> Complete/Fail. The durable
//     outbox row is the cross-replica guarantee; the mesh fast-path (memql#1289)
//     wakes the consumer instantly.
//   - Consumer (the bff replica that owns the client WebSocket): proxyAI, after
//     forwarding the trigger, SubscribeStreamFrames(stream:<requestId>) and
//     renders each ordered/replayed/deduped frame back to the client on the
//     stream it already holds. A bff replica that dies mid-stream is taken over
//     by another replica that re-subscribes the same key and REPLAYS from the
//     cursor -- no lost/reordered/duplicated chunks (the exact failure the epic
//     fixes; today a streamed chunk is lost the same way a reply is).
//
// The wire contract to the browser is UNCHANGED: the consumer reconstructs the
// same MemqlServerMessage payloads (AiChunk / AiChatResult / Transcribe Delta /
// Complete) the direct path produced, so the SPA needs no change.

// streamConsumerID is the durable cursor identity for a bff consuming a stream.
// Per-replica (node id) so a different replica taking over the WS gets its own
// clean replay from the producer's first chunk rather than inheriting our acked
// position -- exactly like ChatReplyDelivery.consumerID (memql#1264, ADR 4.2/4.4).
func (s *service) streamConsumerID() string {
	if s == nil {
		return ""
	}
	return s.selfNodeID()
}

// selfNodeID resolves this node's id for substrate addressing/audit. Falls back
// to the compiled node type when no explicit id was plumbed (single-binary dev).
func (s *service) selfNodeID() string {
	if id := strings.TrimSpace(s.streamNodeID); id != "" {
		return id
	}
	return string(node.CompiledNodeType())
}

// streamingOverSubstrate reports whether the substrate is wired on this node.
// True only on a mesh node where app bootstrap installed it; single-node /
// non-mesh binaries leave it nil and keep the direct push.
func (s *service) streamingOverSubstrate() bool {
	return s != nil && s.deliverySubstrate != nil
}

// isForwardedSession reports whether this streamSession is serving a request
// FORWARDED from a bff (the worker side: s.stream is a *forwardedStream wrapping
// AiForwardResponse), as opposed to a direct client stream (single-binary, where
// s.stream IS the browser's stream). The streaming producer routes to the
// substrate ONLY on the forwarded path: that is the cross-node delivery leg the
// substrate fixes. On a direct client stream there is no cross-node hop and no
// bff consumer, so the producer keeps the in-process direct push -- routing it to
// the substrate would publish tokens nothing consumes.
func (s *streamSession) isForwardedSession() bool {
	if s == nil {
		return false
	}
	_, ok := s.stream.(*forwardedStream)
	return ok
}

// streamProducerOverSubstrate gates the worker-side producer: substrate wired AND
// this is a forwarded (worker-serving-bff) session.
func (s *streamSession) streamProducerOverSubstrate() bool {
	return s.service.streamingOverSubstrate() && s.isForwardedSession()
}

// newTokenStreamSession opens a producer-side StreamSession for a token stream
// (agent reply deltas) on stream:<requestId>. topic "token".
func (s *service) newTokenStreamSession(requestId string) *node.StreamSession {
	return node.NewStreamSession(s.deliverySubstrate, requestId, "token", s.selfNodeID())
}

// newTranscriptStreamSession opens a producer-side StreamSession for an audio
// transcription stream (STT deltas) on stream:<requestId>. topic "transcript".
func (s *service) newTranscriptStreamSession(requestId string) *node.StreamSession {
	return node.NewStreamSession(s.deliverySubstrate, requestId, "transcript", s.selfNodeID())
}

// proxyAIStream is the bff-side entry for a streamed AI op migrated onto the
// substrate. It forwards the trigger envelope to the worker (so the worker runs
// its handler and produces frames to stream:<requestId>) and starts the given
// consumer, which subscribes stream:<requestId> and renders frames to the
// client. The forwarded-response channel from the worker carries only the
// terminal AiForwardResponse{Done} (the worker no longer sends streamed payloads
// over it), so it is drained-and-discarded to clean up the inflight entry; the
// substrate is the single delivery path for the streamed content.
func (s *streamSession) proxyAIStream(
	envelope *memqlv1.MemqlClientMessage,
	requestId string,
	target node.NodeType,
	consume func(ctx context.Context, correlate, requestId string),
) error {
	ctx, cancel := context.WithCancel(s.stream.Context())
	correlate := envelope.GetMessageId()
	s.activeRequests.Store(requestId, cancel)

	// Refuse locally on an unprovable authority. It matters more here than on
	// the sibling path: the forward response channel below is drained and
	// DISCARDED, so a receiver-side refusal delivered as a QueryError would
	// never reach the client and the stream would simply hang (memql#3205).
	principal, err := s.forwardedPrincipal()
	if err != nil {
		cancel()
		return s.sendQueryError(requestId, correlate, codes.Internal,
			"cannot establish forwarded authority for this session: "+err.Error())
	}
	stampEnvelopeProvenance(ctx, envelope)

	respCh, err := s.service.aiForwarder.Forward(ctx, forwardRequestKey(ctx, requestId, envelope), target, principal, envelope)
	if err != nil {
		cancel()
		return s.sendQueryError(requestId, correlate, codes.Unavailable, err.Error())
	}

	// Drain the forward responses. With the substrate cutover the worker
	// delivers the streamed CONTENT over the substrate, so this channel
	// normally carries only the terminal that closes the inflight entry -- but
	// it must still be consumed so the forwarder's cleanup runs.
	//
	// A QueryError is the exception, and it is NOT discardable (memql#3205).
	// The receiver answers a refused OPENER terminally, and that answer arrives
	// here -- so discarding it leaves consume() blocked on a substrate stream
	// the worker will never produce to, with no timeout, until the client's
	// gRPC stream dies. The client sees a hang where the contract promises a
	// typed error. Unlike the transcribe sibling there is no recovery path:
	// streaming chat sends no continuation, so nothing later trips the
	// HasInflight check.
	//
	// Reachable in normal operation, not just on a bug: an old producer talking
	// to an already-upgraded receiver is refused for the length of a rolling
	// deploy, and so is a badge whose expiry passed on the worker's clock.
	go func() {
		for msg := range respCh {
			qe := msg.GetQueryError()
			if qe == nil {
				continue
			}
			_ = s.sendQueryError(requestId, correlate,
				forwardErrorCode(qe.GetError().GetCode()), qe.GetError().GetMessage())
			cancel()
		}
	}()

	// Consume the streamed frames from the substrate and render to the client.
	go func() {
		defer cancel()
		defer s.activeRequests.Delete(requestId)
		consume(ctx, correlate, requestId)
	}()
	return nil
}

// produceTokenStreamToSubstrate is the worker-side producer for token streaming.
// It drains the provider's chunk channel and emits ordered StreamSession frames
// to stream:<requestId>: a Start, one Delta per non-empty token, and a terminal
// Complete carrying the assembled final text (or Fail on a provider error). Each
// emit is a synchronous durable Publish (the backpressure floor); ordering +
// replay are the substrate's guarantees. The WS-owning bff renders these back to
// the client via consumeTokenStream.
func (s *streamSession) produceTokenStreamToSubstrate(ctx context.Context, requestId string, chunks <-chan common.StreamChunk) {
	// A bare completion is safe only after the durable terminal was written.
	// Otherwise the browser has no row to consume and must receive the failure
	// over the forwarding hop while that transport is still available.
	var publishErr error
	defer func() {
		if publishErr != nil {
			if s.logger != nil {
				s.logger.Warn("token stream durable publication failed", "request_id", requestId, "error", publishErr)
			}
			_ = s.sendQueryError(requestId, requestId, codes.Unavailable, "AI response could not be delivered; please try again")
			return
		}
		s.closeForwardInflight(requestId)
	}()

	sess := s.service.newTokenStreamSession(requestId)
	if _, publishErr = sess.Start(ctx, nil); publishErr != nil {
		return
	}

	var fullContent strings.Builder
	for chunk := range chunks {
		if chunk.Error != nil {
			_, publishErr = sess.Fail(ctx, chunk.Error.Error())
			return
		}
		if len(chunk.Metadata) != 0 {
			if _, publishErr = sess.DeltaWithMeta(ctx, "", chunk.Metadata); publishErr != nil {
				return
			}
		}
		if chunk.Content != "" {
			fullContent.WriteString(chunk.Content)
			if _, publishErr = sess.Delta(ctx, chunk.Content); publishErr != nil {
				return
			}
		}
		if chunk.Done {
			_, publishErr = sess.Complete(ctx, fullContent.String(), nil)
			return
		}
	}
	// A provider may close without an explicit Done; publication failure must
	// still remain an error, including a canceled request's implicit completion.
	_, publishErr = sess.Complete(ctx, fullContent.String(), nil)
}

// closeForwardInflight sends a single bare terminal over the worker's
// forwardedStream so the bff's Forward() inflight entry is cleaned up once the
// streamed content has been delivered over the substrate. forwardedStream.Send
// marks the enclosing AiForwardResponse Done because the payload is a terminal
// type (AiChatResult). It is a no-op when s.stream is not a forwardedStream
// (single-binary), which never reaches the substrate producer anyway. The
// payload here is intentionally empty: it exists only to close the inflight and
// is discarded by the bff (proxyAIStream drains forward responses).
func (s *streamSession) closeForwardInflight(requestId string) {
	if _, ok := s.stream.(*forwardedStream); !ok {
		return
	}
	_ = s.stream.Send(&memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_AiChatResult{
			AiChatResult: &memqlv1.AiChatResult{RequestId: requestId},
		},
	})
}

// newTranscriptStreamSink returns a send func that the worker-side transcribe
// handler uses in place of the direct stream.Send: it translates each outgoing
// AiTranscribeStreamDelta / AiTranscribeStreamComplete MemqlServerMessage into an
// ordered StreamSession frame on stream:<requestId>. The session is started
// lazily on first use and the Complete message ends it; the isFinal / confidence
// / duration / provider fields ride the frame Meta so the consumer reconstructs
// the wire message byte-for-byte. Calls are serialized by the transcribe stream's
// send lock, including router observations and terminal messages.
func (s *service) newTranscriptStreamSink(ctx context.Context, requestId string, rawSend func(*memqlv1.MemqlServerMessage) error) func(*memqlv1.MemqlServerMessage) error {
	sess := s.newTranscriptStreamSession(transcriptionKey(ctx, requestId))
	started := false
	ensureStart := func(ctx context.Context) error {
		if started {
			return nil
		}
		started = true
		_, err := sess.Start(ctx, nil)
		return err
	}
	// closeInflight sends a bare terminal over the raw forwardedStream so the
	// bff's Forward() inflight is cleaned up once the transcript terminal has gone
	// to the substrate. The bff discards forward responses under proxyAIStream, so
	// this never reaches the client.
	closeInflight := func() {
		if rawSend == nil {
			return
		}
		_ = rawSend(&memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_AiTranscribeStreamComplete{
				AiTranscribeStreamComplete: &memqlv1.AiTranscribeStreamComplete{RequestId: requestId},
			},
		})
	}
	return func(m *memqlv1.MemqlServerMessage) error {
		// Cancellation stops inference first. The terminal still needs a short,
		// detached delivery budget to release the remote consumer and inflight.
		deliveryCtx := ctx
		if m.GetAiTranscribeStreamComplete() != nil || m.GetQueryError() != nil {
			var cancel context.CancelFunc
			deliveryCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			defer closeInflight()
		}
		switch p := m.GetPayload().(type) {
		case *memqlv1.MemqlServerMessage_AiTranscribeStreamDelta:
			if err := ensureStart(deliveryCtx); err != nil {
				return err
			}
			d := p.AiTranscribeStreamDelta
			_, err := sess.DeltaWithMeta(deliveryCtx, d.GetText(), map[string]any{
				"isFinal":      d.GetIsFinal(),
				"confidence":   float64(d.GetConfidence()),
				"metadataJson": d.GetMetadataJson(),
			})
			return err
		case *memqlv1.MemqlServerMessage_AiTranscribeStreamComplete:
			if err := ensureStart(deliveryCtx); err != nil {
				return err
			}
			c := p.AiTranscribeStreamComplete
			_, err := sess.Complete(deliveryCtx, c.GetText(), map[string]any{
				"durationMs": float64(c.GetDurationMs()),
				"provider":   c.GetProvider(),
			})
			return err
		case *memqlv1.MemqlServerMessage_QueryError:
			if err := ensureStart(deliveryCtx); err != nil {
				return err
			}
			_, err := sess.Fail(deliveryCtx, p.QueryError.GetError().GetMessage())
			return err
		default:
			// Non-transcribe payloads are not part of the stream lifecycle; drop.
			return nil
		}
	}
}

// consumeTokenStream subscribes the bff to a token stream and renders each frame
// to the client as the AiChunk deltas + the terminal AiChatResult, exactly as
// the direct streaming path produced them. It runs in its own goroutine for the
// life of the stream (until terminal or ctx cancellation) and Acks each frame to
// advance the durable cursor. The Start frame carries no client-visible payload
// (it opens the durable stream); Delta frames carry token text; the Complete
// frame carries the final assembled text for the AiChatResult.
func (s *streamSession) consumeTokenStream(ctx context.Context, correlate, requestId string) {
	frames, ack, err := node.SubscribeStreamFrames(ctx, s.service.deliverySubstrate, requestId, s.service.streamConsumerID())
	if err != nil {
		_ = s.sendQueryError(requestId, correlate, codes.Unavailable, "stream subscribe failed: "+err.Error())
		return
	}
	var idx int64
	for frame := range frames {
		switch frame.Phase {
		case node.StreamPhaseStart:
			// Opening frame: no client-visible payload.
		case node.StreamPhaseDelta:
			if len(frame.Meta) != 0 {
				metadata, marshalErr := structpb.NewStruct(frame.Meta)
				if marshalErr == nil {
					s.renderToClient(correlate, &memqlv1.MemqlServerMessage{Payload: &memqlv1.MemqlServerMessage_AiChunk{AiChunk: &memqlv1.AiStreamChunk{StreamId: requestId, RequestId: requestId, Index: idx, Chunk: &memqlv1.AiStreamChunk_Metadata{Metadata: metadata}}}})
					idx++
				}
			}
			if frame.Data != "" {
				s.renderToClient(correlate, &memqlv1.MemqlServerMessage{
					Payload: &memqlv1.MemqlServerMessage_AiChunk{
						AiChunk: &memqlv1.AiStreamChunk{
							StreamId:  requestId,
							RequestId: requestId,
							Index:     idx,
							Chunk:     &memqlv1.AiStreamChunk_TextDelta{TextDelta: frame.Data},
						},
					},
				})
				idx++
			}
		case node.StreamPhaseComplete:
			s.renderToClient(correlate, &memqlv1.MemqlServerMessage{
				Payload: &memqlv1.MemqlServerMessage_AiChatResult{
					AiChatResult: &memqlv1.AiChatResult{
						RequestId: requestId,
						Message: &memqlv1.AiChatMessage{
							Role:    "assistant",
							Content: frame.Data,
						},
					},
				},
			})
		case node.StreamPhaseError:
			_ = s.sendQueryError(requestId, correlate, codes.Internal, frame.Err)
		case node.StreamPhaseCancel:
			// Cancelled stream: close out quietly; the client already hung up.
		}
		_ = ack(frame.Seq)
	}
}

// consumeTranscriptStream subscribes the bff to an audio transcription stream
// and renders each frame to the client as the AiTranscribeStreamDelta deltas +
// the terminal AiTranscribeStreamComplete. The Delta frames carry the interim
// transcript (with isFinal/confidence packed into Meta); the Complete frame
// carries the final transcript + duration/provider in Meta.
func (s *streamSession) consumeTranscriptStream(ctx context.Context, correlate, requestId string) {
	frames, ack, err := node.SubscribeStreamFrames(ctx, s.service.deliverySubstrate, transcriptionKey(ctx, requestId), s.service.streamConsumerID())
	if err != nil {
		_ = s.sendQueryError(requestId, correlate, codes.Unavailable, "transcript subscribe failed: "+err.Error())
		return
	}
	for frame := range frames {
		switch frame.Phase {
		case node.StreamPhaseStart:
			// Opening frame: no client-visible payload.
		case node.StreamPhaseDelta:
			s.renderToClient(correlate, &memqlv1.MemqlServerMessage{
				Payload: &memqlv1.MemqlServerMessage_AiTranscribeStreamDelta{
					AiTranscribeStreamDelta: &memqlv1.AiTranscribeStreamDelta{
						RequestId:    requestId,
						Text:         frame.Data,
						IsFinal:      metaBool(frame.Meta, "isFinal"),
						Confidence:   float32(metaFloat(frame.Meta, "confidence")),
						MetadataJson: metaString(frame.Meta, "metadataJson"),
					},
				},
			})
		case node.StreamPhaseComplete:
			s.renderToClient(correlate, &memqlv1.MemqlServerMessage{
				Payload: &memqlv1.MemqlServerMessage_AiTranscribeStreamComplete{
					AiTranscribeStreamComplete: &memqlv1.AiTranscribeStreamComplete{
						RequestId:  requestId,
						Text:       frame.Data,
						DurationMs: int64(metaFloat(frame.Meta, "durationMs")),
						Provider:   metaString(frame.Meta, "provider"),
					},
				},
			})
		case node.StreamPhaseError:
			_ = s.sendQueryError(requestId, correlate, codes.Internal, frame.Err)
		case node.StreamPhaseCancel:
			s.renderToClient(correlate, &memqlv1.MemqlServerMessage{
				Payload: &memqlv1.MemqlServerMessage_AiTranscribeStreamComplete{
					AiTranscribeStreamComplete: &memqlv1.AiTranscribeStreamComplete{
						RequestId: requestId,
						Text:      "",
						Provider:  metaString(frame.Meta, "provider"),
					},
				},
			})
		}
		_ = ack(frame.Seq)
	}
}

// renderToClient sends one reconstructed MemqlServerMessage to the originating
// client, rewriting CorrelateTo so it matches the client's envelope and minting
// a MessageId -- the same normalization relayForwardedResponses does for the
// direct forward path.
func (s *streamSession) renderToClient(correlate string, msg *memqlv1.MemqlServerMessage) {
	msg.CorrelateTo = s.safeCorrelate(correlate)
	if msg.MessageId == "" {
		msg.MessageId = id.NewShortId()
	}
	s.sendMu.Lock()
	s.stampEventContinuityLocked(msg)
	err := s.stream.Send(msg)
	s.sendMu.Unlock()
	if err != nil && s.logger != nil {
		s.logger.Warn("stream substrate relay send failed", "error", err)
	}
}

// --- small meta accessors (the Start/Complete frame carries a map[string]any) -

func metaBool(m map[string]any, k string) bool {
	if m == nil {
		return false
	}
	b, _ := m[k].(bool)
	return b
}

func metaFloat(m map[string]any, k string) float64 {
	if m == nil {
		return 0
	}
	switch v := m[k].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}

func metaString(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}
