// Package voice is the SDK's voice subpackage. It exposes higher-
// level voice operations built on top of the wire-level
// AiTranscribeStream* protocol family carried by MemqlService.Stream.
//
// PushToTalk is the canonical entry point: open a streaming session,
// feed audio bytes from a caller-supplied reader, surface partial
// transcripts as they arrive, and resolve a final transcript when the
// session closes. The caller owns the audio source (terminal apps
// typically use malgo / miniaudio; browsers use MediaStream); the SDK
// owns the protocol.
//
// The SDK does NOT itself write the resulting utterance row into the
// space. The contract: the caller receives the final transcript via
// FinalTranscript.Text, and is responsible for invoking
// mutationSendTextUtterance (or any other downstream side-effect) on
// the active space. This keeps PushToTalk a pure transcription
// primitive that's reusable for non-chat callers (note-taking,
// command-line dictation, etc.).
package voice

import (
	"context"
	"fmt"
	"io"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/sdk/go/client"
)

// AudioFormat describes the encoding of the audio bytes being streamed.
type AudioFormat struct {
	Encoding   string // "pcm16", "opus", "webm", "wav"
	SampleRate int32  // typically 16000
	Channels   int32  // 1 (mono) or 2 (stereo)
}

// PartialTranscript fires every time the server emits a delta.
// Text is the FULL accumulated transcript so far, not an incremental
// token -- the caller can render it directly as the user types.
type PartialTranscript struct {
	Text       string
	IsFinal    bool
	Confidence float32
}

// FinalTranscript is the resolved result of a transcription session.
type FinalTranscript struct {
	Text       string
	DurationMs int64
	Provider   string
}

// Options configures a PushToTalk session.
type Options struct {
	// Audio describes the format of the bytes the caller will feed
	// into the audio Reader.
	Audio AudioFormat

	// Language is an optional ISO-639-1 hint (e.g. "en") to bias
	// transcription toward a specific language. Empty defers to the
	// provider's default.
	Language string

	// Provider optionally overrides the cluster's default transcription
	// provider (e.g. "openai-realtime", "openai-whisper").
	// Empty uses the cluster default.
	Provider string

	// ChunkBytes is the size of each AiTranscribeStreamChunk payload
	// read from the audio source. Smaller chunks = lower latency,
	// more gRPC frames. Recommended: 1280 bytes (40ms of 16kHz mono
	// PCM16). Defaults to 1280 when zero.
	ChunkBytes int

	// OnPartial is invoked from the SDK's reader goroutine on every
	// delta. Safe to be nil (caller doesn't care about partials).
	// Must not block significantly -- a slow OnPartial back-pressures
	// the stream listener.
	OnPartial func(PartialTranscript)
}

// PushToTalk runs a streaming transcription session end-to-end:
// opens the gRPC stream session, copies audio from the supplied
// io.Reader to the server in ChunkBytes-sized AiTranscribeStreamChunk
// messages, calls opts.OnPartial as deltas arrive, and returns the
// resolved FinalTranscript on completion.
//
// Lifecycle:
//   - The reader is consumed until EOF (clean end-of-utterance) or
//     a non-EOF read error (session aborted with AiTranscribeStreamEnd
//     cancel=true).
//   - On ctx cancellation, the session is aborted similarly.
//   - On stream-level failure (server crash, network drop), an error
//     is returned and no Complete event arrives.
//
// Concurrency: PushToTalk registers a stream listener with the
// dispatcher keyed on a fresh request_id. Multiple concurrent
// PushToTalk calls against the same dispatcher are safe -- each gets
// its own request_id and its own listener slot.
func PushToTalk(
	ctx context.Context,
	dispatcher *client.Dispatcher,
	audio io.Reader,
	opts Options,
) (*FinalTranscript, error) {
	if dispatcher == nil {
		return nil, fmt.Errorf("voice.PushToTalk: dispatcher is nil")
	}
	if audio == nil {
		return nil, fmt.Errorf("voice.PushToTalk: audio reader is nil")
	}
	if opts.Audio.Encoding == "" || opts.Audio.SampleRate == 0 || opts.Audio.Channels == 0 {
		return nil, fmt.Errorf("voice.PushToTalk: Options.Audio must specify Encoding + SampleRate + Channels")
	}
	chunkBytes := opts.ChunkBytes
	if chunkBytes <= 0 {
		chunkBytes = 1280
	}

	requestId := id.NewShortId()
	replies, unregister := dispatcher.RegisterStream(requestId)
	defer unregister()

	// Send the session-start envelope. Server picks up the request_id
	// here and routes every Delta/Complete back to it.
	start := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_AiTranscribeStreamStart{
			AiTranscribeStreamStart: &memqlv1.AiTranscribeStreamStart{
				RequestId:    requestId,
				Format:       opts.Audio.Encoding,
				SampleRate:   opts.Audio.SampleRate,
				Channels:     opts.Audio.Channels,
				LanguageHint: opts.Language,
				Provider:     opts.Provider,
			},
		},
	}
	if _, err := dispatcher.Send(start); err != nil {
		return nil, fmt.Errorf("voice.PushToTalk: send Start: %w", err)
	}

	// Stream audio in a goroutine so we can concurrently read replies.
	// readErr carries the eventual outcome of the audio-pump goroutine
	// so we can decide whether to send End{cancel:false} (clean EOF)
	// or End{cancel:true} (read error / ctx cancel).
	readErrCh := make(chan error, 1)
	go func() {
		readErrCh <- streamAudio(ctx, dispatcher, requestId, audio, chunkBytes)
	}()

	// Drain replies until Complete or stream death.
	var final *FinalTranscript
	for {
		select {
		case <-ctx.Done():
			// Best-effort abort. Don't block on Send failure -- the
			// dispatcher is likely already torn down.
			_ = sendEnd(dispatcher, requestId, true)
			<-readErrCh
			return nil, ctx.Err()

		case <-dispatcher.Done():
			return nil, fmt.Errorf("voice.PushToTalk: dispatcher closed before Complete")

		case readErr := <-readErrCh:
			// Audio pump finished. Drain Start completion path:
			// either clean EOF (we already sent End{cancel:false}) or
			// read error / ctx cancel handled by sendEnd cancel=true.
			if readErr != nil && readErr != io.EOF {
				return nil, fmt.Errorf("voice.PushToTalk: audio read: %w", readErr)
			}
			// Fall through to keep draining replies until Complete.
			readErrCh = nil

		case msg, ok := <-replies:
			if !ok {
				return nil, fmt.Errorf("voice.PushToTalk: reply channel closed before Complete")
			}
			switch p := msg.Payload.(type) {
			case *memqlv1.MemqlServerMessage_AiTranscribeStreamDelta:
				if opts.OnPartial != nil {
					opts.OnPartial(PartialTranscript{
						Text:       p.AiTranscribeStreamDelta.GetText(),
						IsFinal:    p.AiTranscribeStreamDelta.GetIsFinal(),
						Confidence: p.AiTranscribeStreamDelta.GetConfidence(),
					})
				}
			case *memqlv1.MemqlServerMessage_AiTranscribeStreamComplete:
				final = &FinalTranscript{
					Text:       p.AiTranscribeStreamComplete.GetText(),
					DurationMs: p.AiTranscribeStreamComplete.GetDurationMs(),
					Provider:   p.AiTranscribeStreamComplete.GetProvider(),
				}
				return final, nil
			}
		}
	}
}

// streamAudio copies the audio reader to the server in ChunkBytes-
// sized Chunk messages, then sends End{cancel:false} on clean EOF.
// On ctx cancel or non-EOF read error, sends End{cancel:true}.
//
// Returns nil on clean EOF, ctx.Err() on cancellation, or the read
// error.
func streamAudio(
	ctx context.Context,
	dispatcher *client.Dispatcher,
	requestId string,
	audio io.Reader,
	chunkBytes int,
) error {
	buf := make([]byte, chunkBytes)
	for {
		select {
		case <-ctx.Done():
			_ = sendEnd(dispatcher, requestId, true)
			return ctx.Err()
		default:
		}
		n, err := audio.Read(buf)
		if n > 0 {
			chunk := &memqlv1.MemqlClientMessage{
				Payload: &memqlv1.MemqlClientMessage_AiTranscribeStreamChunk{
					AiTranscribeStreamChunk: &memqlv1.AiTranscribeStreamChunk{
						RequestId: requestId,
						// Copy the slice -- buf is reused next iteration.
						Audio: append([]byte(nil), buf[:n]...),
					},
				},
			}
			if _, sendErr := dispatcher.Send(chunk); sendErr != nil {
				return fmt.Errorf("send Chunk: %w", sendErr)
			}
		}
		if err == io.EOF {
			return sendEnd(dispatcher, requestId, false)
		}
		if err != nil {
			_ = sendEnd(dispatcher, requestId, true)
			return err
		}
	}
}

func sendEnd(dispatcher *client.Dispatcher, requestId string, cancel bool) error {
	end := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_AiTranscribeStreamEnd{
			AiTranscribeStreamEnd: &memqlv1.AiTranscribeStreamEnd{
				RequestId: requestId,
				Cancel:    cancel,
			},
		},
	}
	if _, err := dispatcher.Send(end); err != nil {
		return fmt.Errorf("send End: %w", err)
	}
	return nil
}
