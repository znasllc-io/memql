package agent

import (
	"context"
	"log/slog"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// transcript_ack.go closes the #1403 silent-failure gap on the
// VoiceAgentFinalTranscript leg: the cascade and realtime executors used to
// fire the final transcript with Client.Send and discard whatever came back,
// so a server-side rejection (the FinalAck with success=false, or a
// QueryError validation reject) vanished -- the utterance never reached chat
// and nothing anywhere said why. sendFinalTranscript keeps the send
// non-blocking (the envelope is enqueued synchronously so wire ordering
// against the immediately-following VoiceAgentTurnRequest is preserved) but
// registers a correlated waiter and consumes the server's reply on a
// background goroutine, WARN-logging every rejection.

// finalAckWaitTimeout bounds how long the background consumer waits for the
// server's FinalAck. The insert is a single mutation (milliseconds in
// practice); well under the 30s turn budget is plenty, and on expiry we log
// rather than retry -- the ack consumption exists for observability, the
// send itself stays best-effort.
const finalAckWaitTimeout = 15 * time.Second

// sendFinalTranscript sends a VoiceAgentFinalTranscript and consumes its
// correlated reply instead of discarding it. The enqueue happens on the
// caller's goroutine (preserving send order with the turn request that
// follows); only the ack wait is detached.
func sendFinalTranscript(ctx context.Context, client *Client, logger *slog.Logger, executor string, msg *memqlv1.VoiceAgentFinalTranscript) {
	envelope := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_VoiceAgentFinalTranscript{
			VoiceAgentFinalTranscript: msg,
		},
	}
	sendStart := time.Now()
	replies, release, err := client.StreamRequest(ctx, envelope)
	if err != nil {
		if logger != nil {
			logger.Warn("voice-agent final transcript send failed",
				"executor", executor, "space_id", msg.GetSpaceId(), "err", err)
		}
		return
	}
	go consumeFinalTranscriptReply(ctx, logger, executor, msg.GetSpaceId(), sendStart, replies, release)
}

// consumeFinalTranscriptReply awaits the server's reply to one final
// transcript and logs the outcome: a failed FinalAck or a QueryError is a
// WARN (the user's utterance did NOT land in chat -- the exact failure class
// #1403 made invisible); a successful ack is a Debug trace.
func consumeFinalTranscriptReply(
	ctx context.Context,
	logger *slog.Logger,
	executor string,
	spaceID string,
	sendStart time.Time,
	replies <-chan *memqlv1.MemqlServerMessage,
	release func(),
) {
	defer release()
	timer := time.NewTimer(finalAckWaitTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
		if logger != nil {
			logger.Warn("voice-agent final transcript ack timed out",
				"executor", executor, "space_id", spaceID,
				"timeout", finalAckWaitTimeout.String())
		}
	case reply, ok := <-replies:
		if !ok {
			return
		}
		// #1426: final-transcript send -> server ack. The wait is off the
		// audio path (this goroutine), but the duration measures the server's
		// synchronous utterance insert (engine mutation -> DB) for the turn.
		logVoiceTiming(logger, "turn.final_transcript_ack", sendStart,
			"executor", executor, "space_id", spaceID)
		logFinalTranscriptReply(logger, executor, spaceID, reply)
	}
}

// logFinalTranscriptReply classifies one correlated reply to a final
// transcript. Split out for direct unit testing.
func logFinalTranscriptReply(logger *slog.Logger, executor, spaceID string, reply *memqlv1.MemqlServerMessage) {
	if logger == nil || reply == nil {
		return
	}
	if ack := reply.GetVoiceAgentFinalAck(); ack != nil {
		if ack.GetSuccess() {
			logger.Debug("voice-agent final transcript acked",
				"executor", executor, "space_id", spaceID,
				"request_id", ack.GetRequestId(), "utterance_id", ack.GetUtteranceId())
			return
		}
		logger.Warn("voice-agent final transcript rejected",
			"executor", executor, "space_id", spaceID,
			"request_id", ack.GetRequestId(),
			"error_code", ack.GetErrorCode(),
			"error_message", ack.GetErrorMessage())
		return
	}
	if qe := reply.GetQueryError(); qe != nil {
		logger.Warn("voice-agent final transcript rejected",
			"executor", executor, "space_id", spaceID,
			"request_id", qe.GetRequestId(),
			"error_code", qe.GetError().GetCode(),
			"error_message", qe.GetError().GetMessage())
		return
	}
	logger.Debug("voice-agent final transcript: unexpected reply payload",
		"executor", executor, "space_id", spaceID,
		"payload", ServerPayloadName(reply))
}
