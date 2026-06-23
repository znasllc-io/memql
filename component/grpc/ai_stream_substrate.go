package memql

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"

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
	ctx := s.stream.Context()
	correlate := envelope.GetMessageId()

	claims := s.forwardedAuthClaims()
	partition := extractPartitionFromEnvelope(envelope)
	stampEnvelopeProvenance(ctx, envelope)

	respCh, err := s.service.aiForwarder.Forward(ctx, requestId, target, claims, partition, envelope)
	if err != nil {
		return s.sendQueryError(requestId, correlate, codes.Unavailable, err.Error())
	}

	// Drain-and-discard the forward responses: with the substrate cutover the
	// worker delivers the streamed content over the substrate, so the forward
	// channel only carries the terminal that closes the inflight entry. We must
	// still consume it so the forwarder's cleanup runs.
	go func() {
		for range respCh { //nolint:revive // intentional drain
		}
	}()

	// Consume the streamed frames from the substrate and render to the client.
	go consume(ctx, correlate, requestId)
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
	// The streamed content rides the substrate, but the forward inflight on the
	// bff still needs a terminal AiForwardResponse{Done} to be cleaned up (the
	// content no longer flows over forwardedStream). Emit one bare terminal over
	// the forwardedStream on EVERY exit path. The bff's proxyAIStream drains and
	// DISCARDS forward responses, so this terminal closes the inflight without
	// being rendered to the client (no double delivery).
	defer s.closeForwardInflight(requestId)

	sess := s.service.newTokenStreamSession(requestId)
	if _, err := sess.Start(ctx, nil); err != nil {
		if s.logger != nil {
			s.logger.Warn("token stream substrate Start failed", "request_id", requestId, "error", err)
		}
		return
	}

	var fullContent strings.Builder
	for chunk := range chunks {
		if chunk.Error != nil {
			_, _ = sess.Fail(ctx, chunk.Error.Error())
			return
		}
		if chunk.Content != "" {
			fullContent.WriteString(chunk.Content)
			if _, err := sess.Delta(ctx, chunk.Content); err != nil {
				if s.logger != nil {
					s.logger.Warn("token stream substrate Delta failed", "request_id", requestId, "error", err)
				}
				return
			}
		}
		if chunk.Done {
			if _, err := sess.Complete(ctx, fullContent.String(), nil); err != nil && s.logger != nil {
				s.logger.Warn("token stream substrate Complete failed", "request_id", requestId, "error", err)
			}
			return
		}
	}
	// Provider closed without an explicit Done: still terminate the stream so the
	// consumer closes cleanly with whatever was assembled.
	if _, err := sess.Complete(ctx, fullContent.String(), nil); err != nil && s.logger != nil {
		s.logger.Warn("token stream substrate Complete (implicit) failed", "request_id", requestId, "error", err)
	}
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
// single-pump producer, so no extra lock is needed.
func (s *service) newTranscriptStreamSink(ctx context.Context, requestId string, rawSend func(*memqlv1.MemqlServerMessage) error) func(*memqlv1.MemqlServerMessage) error {
	sess := s.newTranscriptStreamSession(requestId)
	started := false
	ensureStart := func() error {
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
		switch p := m.GetPayload().(type) {
		case *memqlv1.MemqlServerMessage_AiTranscribeStreamDelta:
			if err := ensureStart(); err != nil {
				return err
			}
			d := p.AiTranscribeStreamDelta
			_, err := sess.DeltaWithMeta(ctx, d.GetText(), map[string]any{
				"isFinal":    d.GetIsFinal(),
				"confidence": float64(d.GetConfidence()),
			})
			return err
		case *memqlv1.MemqlServerMessage_AiTranscribeStreamComplete:
			if err := ensureStart(); err != nil {
				return err
			}
			c := p.AiTranscribeStreamComplete
			_, err := sess.Complete(ctx, c.GetText(), map[string]any{
				"durationMs": float64(c.GetDurationMs()),
				"provider":   c.GetProvider(),
			})
			closeInflight()
			return err
		case *memqlv1.MemqlServerMessage_QueryError:
			if err := ensureStart(); err != nil {
				return err
			}
			_, err := sess.Fail(ctx, p.QueryError.GetError().GetMessage())
			closeInflight()
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
	frames, ack, err := node.SubscribeStreamFrames(ctx, s.service.deliverySubstrate, requestId, s.service.streamConsumerID())
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
						RequestId:  requestId,
						Text:       frame.Data,
						IsFinal:    metaBool(frame.Meta, "isFinal"),
						Confidence: float32(metaFloat(frame.Meta, "confidence")),
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
