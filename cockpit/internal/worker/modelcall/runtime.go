// Package modelcall serves the engine's ModelCall envelope from this
// machine's own model runtime (epic memql#4676, task memql-cockpit#362).
//
// The engine fixes the contract and picks the machine; everything here
// runs the call. Start / delta / end / cancel, correlated by request id,
// over the stream the worker already holds open -- the AppSession shape,
// because a generation emits tokens for as long as it runs and the caller
// wants them as they arrive.
//
// Four rules run through the package, and each one fails silently when
// broken:
//
//   - DELTAS CARRY A MONOTONIC seq. The engine drops out-of-order and
//     duplicate deltas rather than corrupting the generation, so the
//     sequence has to be produced correctly here; there is no repair on
//     the other side.
//
//   - THE ENVELOPE OWNS THE DEADLINES. Timeout, idle ceiling and
//     keepalive arrive on ModelCallLimits. A local 8B model on a cold GPU
//     is twenty seconds from start to first token, which is
//     indistinguishable from a wedged machine to anything holding only a
//     wall clock -- the keepalive is what makes the idle ceiling
//     enforceable rather than a guess, so a call with nothing to say
//     still has to say it.
//
//   - USAGE IS REPORTED, NEVER INFERRED. What the runtime said, including
//     which model it actually ran. Silence stays silence, which the
//     engine records as billing "unknown". A count derived from string
//     length would be stored as measured, and the only thing it could
//     corrupt is the loop-cap arithmetic that exists to notice a runaway.
//
//   - A SCHEMA IS HONOURED OR THE CALL FAILS. The router only ever sends
//     a response schema to a machine that advertised the capability, so
//     answering prose instead would defeat the gating that put the call
//     here -- and the parse error would surface three layers away, naming
//     nothing.
package modelcall

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
)

// Kinds, mirrored from memql component/worker/modelcall.go.
const (
	KindChat      = "chat"
	KindEmbedding = "embedding"

	// The four MODALITY kinds (engine memql#5137, record D4).
	//
	// The kind selector needs no proto change -- ModelCallStart.kind is
	// a plain string, so these four travel today. What does NOT exist
	// is anywhere to put their payloads: at the pin ModelCallMessage is
	// {role, content} with no image parts, ModelCallDelta and
	// ModelCallEnd carry only strings, and embedding_input is []string.
	// So the kinds are admitted and the payload is refused by name --
	// see payloadFor and CodePayloadUnavailable.
	//
	// This repository DEFINES these four strings, the same way it
	// defines the four label flags: memql#5137 has not merged, so there
	// is nothing upstream to transcribe.
	KindVision     = "vision"
	KindTranscribe = "transcribe"
	KindSpeak      = "speak"
	KindImage      = "image"
)

// modalityKinds maps each modality kind to the label flag a machine
// must have advertised before it will serve one, and to the operator
// word for it.
//
// A SINGLE TABLE, because the three things must agree: the kind the
// router sends, the flag the label carries, and the sentence the
// refusal prints. Three switch statements would be three places for a
// modality to be half-added.
var modalityKinds = map[string]struct {
	Advertised func(models.Attributes) bool
	Word       string
}{
	KindVision:     {func(a models.Attributes) bool { return a.Vision }, "vision"},
	KindTranscribe: {func(a models.Attributes) bool { return a.AudioIn }, "transcription"},
	KindSpeak:      {func(a models.Attributes) bool { return a.AudioOut }, "speech"},
	KindImage:      {func(a models.Attributes) bool { return a.ImageGen }, "image generation"},
}

// ServedKinds lists every kind this worker admits, in a stable order,
// for the refusal that has to name them.
func ServedKinds() []string {
	return []string{KindChat, KindEmbedding, KindVision, KindTranscribe, KindSpeak, KindImage}
}

// Finish reasons, mirrored from the same file.
const (
	FinishStop      = "stop"
	FinishLength    = "length"
	FinishCancelled = "cancelled"
	FinishTimeout   = "timeout"
	FinishError     = "error"
)

// Envelope defaults, mirrored from the same file. A ModelCallLimits that
// states nothing gets these rather than "no limit": an unbounded
// generation on somebody's laptop is a resource leak nobody is watching.
const (
	DefaultTimeout     = 10 * time.Minute
	DefaultIdleTimeout = 90 * time.Second
	DefaultKeepalive   = 20 * time.Second
)

// Message is one turn handed to the model.
//
// The last three fields carry a TOOL ROUND TRIP: the assistant turn in
// which the model asked for a call, and the role="tool" turn that answers
// it. They are replayed to the runtime rather than summarised into
// Content, because a model that cannot see its own call has no way to
// match a result to it and answers as though the tool was never run.
type Message struct {
	Role    string
	Content string
	// ToolCallID names the assistant tool call a role="tool" turn is the
	// answer to. Both runtimes spell this one the same way; what they
	// disagree about is the tool NAME below.
	ToolCallID string
	// Name is the tool that produced a role="tool" turn's content.
	// Ollama reads it from `tool_name` and the OpenAI-compatible surface
	// from `name`, so the spelling is chosen inside each client and never
	// here -- a caller that had to know which runtime answered would
	// carry that fork into every layer above.
	Name string
	// ToolCalls are the calls an assistant turn asked for.
	ToolCalls []ToolCall
	// Images are the pictures a vision turn carries (memql#5137).
	//
	// A turn with images renders its content as an ARRAY of parts
	// rather than a string, which is the only shape an
	// OpenAI-compatible server accepts one in. The field is on the
	// message rather than beside it so the mapping stays one function
	// with one shape -- a second parameter threaded through
	// openAIMessages would have to be kept in step with the slice it
	// indexes, and a mismatch there attaches an image to the wrong turn.
	Images []ImagePart
}

// ToolCall is one call the model asked for -- as it came back from the
// runtime, and as it goes back in on the assistant turn of a round trip.
//
// ArgumentsJSON is a STRING of JSON rather than a decoded map, and that
// normalisation is the reason this type exists at all: Ollama's
// /api/chat returns tool-call arguments as a JSON OBJECT while every
// OpenAI-compatible runtime returns them as a STRING. Storing the text
// means each client re-shapes the arguments exactly once, on the way in
// and on the way out, and nothing above this package ever asks which
// runtime answered.
type ToolCall struct {
	// ID correlates this call with the role="tool" message that answers
	// it. It is passed through EMPTY when the runtime minted none --
	// Ollama's native surface usually does -- rather than filled in here:
	// an id this side invented would match nothing the model ever said,
	// and the pairing is done on this exact string.
	ID string
	// Name is the tool the model chose.
	Name string
	// ArgumentsJSON is the arguments object as JSON TEXT, unparsed.
	ArgumentsJSON string
}

// Tool is one tool offered to the model for this call.
//
// ParametersJSON is the tool's JSON Schema FORWARDED VERBATIM. Nothing in
// this package parses it, re-marshals it or validates it, and that is
// deliberate: the schema belongs to the caller, and a round trip through
// a Go map sorts every key, drops what encoding/json does not model and
// reformats the numbers -- each of which changes what the model is told
// it may call. The damage surfaces as the model calling the tool wrongly,
// three layers away, naming nothing here. It is the same rule
// ChatRequest.Schema already follows for the response schema.
type Tool struct {
	Name           string
	Description    string
	ParametersJSON string
}

// Params are the generation knobs. Each optional knob carries an explicit
// Set companion because zero is a MEANINGFUL value for both temperature
// and top_p: temperature 0 is what a structured-output prompt actually
// wants, and it is not the same request as "the caller said nothing".
type Params struct {
	Temperature     float64
	TemperatureSet  bool
	TopP            float64
	TopPSet         bool
	MaxOutputTokens int64
	// ContextTokens is the total working window the engine requires, including output.
	ContextTokens int64
	Stop          []string
	Seed          int64
	SeedSet       bool
}

// ChatRequest is one chat generation.
type ChatRequest struct {
	Model    string
	Messages []Message
	Params   Params
	// Schema is a JSON Schema for structured output. Nil means free text.
	Schema []byte
	// Tools are the tools offered to the model on this turn. Empty means
	// none is offered, and no `tools` key is sent -- which is not the
	// same request as offering an empty list, and some runtimes answer
	// the two differently.
	Tools []Tool
	// Level is the engine's level for the call. It never changes the model
	// the router chose; the Ollama client reads it only to turn thinking off
	// for fast calls (ollamaThink).
	Level string
}

// EmbedRequest is one embedding call.
type EmbedRequest struct {
	Model string
	Input []string
}

// Usage is what the RUNTIME REPORTED. Known separates "it told us zero"
// from "it told us nothing", which the engine needs in order to record
// billing "unknown" instead of a confident zero.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	Known        bool
	// Model is what the runtime ACTUALLY ran, which is not always what
	// was asked for -- a quantisation alias, a tag resolving to a digest.
	// A token count without the model it was spent on cannot be read.
	Model string
}

// Result is a finished call.
type Result struct {
	FinishReason string
	// Embeddings is the KindEmbedding result, one vector per input, in
	// input order.
	Embeddings [][]float32
	Usage      Usage
	// ToolCalls are the calls the model asked for, COMPLETE and in the
	// order it asked. A caller never sees a partial arguments string:
	// Ollama emits each call whole, and the OpenAI-compatible client
	// reassembles the streamed fragments before returning. A call whose
	// stream was cut mid-arguments reports none at all rather than a
	// fragment, because a fragment would be dispatched as if the model
	// had finished writing it.
	ToolCalls []ToolCall
}

// The MODALITY capabilities, as optional interfaces on top of Client.
//
// Optional rather than methods on Client, because not every runtime
// serves every modality and a mandatory method would force each client
// to carry a stub that refuses -- which reads as a runtime that CAN do
// the thing and chose not to. A type assertion that fails is the
// honest shape: this runtime does not serve this.
//
// Which client implements which is decided by which probe can set the
// flag. `vision` and `imagegen` come from Ollama's own capability list,
// so the native client implements both; `audioin` and `audioout` can
// only be true through a declared runtime, which clientFor reaches as
// an openAIClient.
type (
	// VisionClient serves kind="vision".
	VisionClient interface {
		Vision(ctx context.Context, req VisionRequest, emit Emit) (Result, error)
	}
	// Transcriber serves kind="transcribe".
	Transcriber interface {
		Transcribe(ctx context.Context, req TranscribeRequest) (TranscribeResult, error)
	}
	// Speaker serves kind="speak".
	Speaker interface {
		Speak(ctx context.Context, req SpeakRequest) (SpeakResult, error)
	}
	// ImageGenerator serves kind="image".
	ImageGenerator interface {
		GenerateImage(ctx context.Context, req ImageRequest) (ImageResult, error)
	}
)

// Emit receives one piece of generated text. Returning an error stops
// the generation -- it means the stream back to the cluster is gone, and
// continuing would spend this machine's GPU on output nobody will read.
//
// Exported alongside Client for the same reason: a caller outside this
// package cannot implement or supply one otherwise.
type Emit func(content string) error

// emitFunc is the internal spelling, kept so the existing call sites
// read unchanged.
type emitFunc = Emit

// Client is one runtime family. Both implementations are stateless; the
// per-call state lives on the call.
//
// It is EXPORTED because two callers now need to reach a runtime: the
// call manager here, and internal/worker/probe, which measures a model
// against the suite. They must reach it the same way -- a probe that
// measured a model through a different path than the one serving would
// measure the wrong thing, and the figure it produced would rank a
// machine on a code path no caller uses.
type Client interface {
	Chat(ctx context.Context, req ChatRequest, emit emitFunc) (Result, error)
	Embed(ctx context.Context, req EmbedRequest) (Result, error)
}
