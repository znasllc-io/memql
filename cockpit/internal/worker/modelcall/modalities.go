package modelcall

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Modality calls use the worker's existing authenticated ModelCall stream.
// Vision uses OpenAI chat image parts; ASR uses a declared OpenAI multipart,
// whisper.cpp or chat-audio endpoint; speech uses an OpenAI-compatible runtime.
// payload.go maps actual protobuf media fields before dispatch.

// ImagePart is one image handed to a vision call.
//
// Bytes plus a media type, never a URL. A worker that fetched a URL
// would be making an outbound request on behalf of whoever composed the
// prompt, from inside somebody's home network -- the exact shape the
// http tool's block_private_net policy exists to prevent, arriving
// through a door that has no policy at all.
type ImagePart struct {
	MediaType string
	Data      []byte
}

// dataURL renders the part the way both surfaces accept an inline
// image. base64 rather than a multipart upload because the chat route
// is JSON end to end and this is what its image_url field takes.
func (p ImagePart) dataURL() string {
	media := strings.TrimSpace(p.MediaType)
	if media == "" {
		// A media type the runtime cannot read is worse than a guess it
		// can: every runtime here decodes PNG, and a data URL with an
		// empty type is rejected by the URL parser before any decoder
		// sees it.
		media = "image/png"
	}
	return "data:" + media + ";base64," + base64Of(p.Data)
}

// VisionRequest is a chat turn carrying images.
type VisionRequest struct {
	Model    string
	Messages []Message
	// Images ride the LAST user turn, which is where every
	// OpenAI-compatible server expects them and the only placement that
	// survives a multi-turn conversation: attaching them to the first
	// turn asks the model about an image several turns of context away
	// from the question.
	Images []ImagePart
	Params Params
	// Schema is a JSON Schema for structured output, exactly as
	// ChatRequest carries one.
	//
	// A VISION CALL IS A CHAT CALL, so a schema is as meaningful here as
	// it is there -- and the package's rule is that a schema is honoured
	// or the call FAILS. Dropping it silently would answer prose to a
	// call the router only sent here because this machine advertised
	// structured output, and the parse failure would surface three
	// layers away naming nothing.
	Schema []byte
}

// TranscribeRequest is audio in, text out.
type TranscribeRequest struct {
	Model string
	// Audio is the raw bytes. Format is the container ("wav", "mp3"),
	// which the OpenAI-compatible input_audio field requires by name --
	// it is not sniffed from the bytes, because a wrong guess produces
	// a transcript of noise rather than an error.
	Audio  []byte
	Format string
	// Prompt is optional context ("this recording uses these product
	// names"), passed through when the caller supplies one.
	Prompt string
}

// TranscriptSegment is one timed span of a transcript.
//
// The wire carries these on both Delta and End (memql#5137): a windowed
// transcriber emits one Delta per window with that window's timings,
// and End carries the authoritative full set. The cockpit's own
// transcribe path is one-shot, so it fills End's -- and it fills them
// only when the RUNTIME reported timings, never by dividing the
// duration up, which would be inventing a fact the caller would then
// seek on.
type TranscriptSegment struct {
	StartSeconds float64
	EndSeconds   float64
	Text         string
}

// TranscribeResult is what came back.
type TranscribeResult struct {
	Text     string
	Segments []TranscriptSegment
	Usage    Usage
}

// SpeakRequest is text in, audio out.
type SpeakRequest struct {
	Model string
	Text  string
	// Voice names the speaker. Empty lets the runtime choose its own
	// default rather than this code inventing one: a voice id is a
	// property of the model that is installed, and a name from here
	// that the runtime does not have is a call that fails.
	Voice string
	// Format is the container asked for ("mp3", "wav"). Empty means the
	// runtime's default.
	Format string
	// Speed is a rate multiplier, and SpeedSet says the caller asked
	// for one. The pair rather than a bare float for the reason
	// Params.Temperature carries one: 0 is a meaningful value in the
	// type and is not the same request as "no preference" -- and a
	// speed of 0 sent as a preference is a call that generates nothing.
	Speed    float64
	SpeedSet bool
}

// SpeakResult is the generated audio.
type SpeakResult struct {
	Audio     []byte
	MediaType string
	Usage     Usage
}

// ImageRequest is a prompt in, image bytes out.
type ImageRequest struct {
	Model  string
	Prompt string
	// Width, Height and Count are the caller's knobs; zero means the
	// runtime's own default rather than a dimension this code chose.
	// Ollama's image models each have a native resolution, and a size
	// invented here would be upscaled or refused depending on the
	// model.
	Width, Height, Count int
	// Format is the container asked for ("png", "jpeg", "webp").
	Format string
}

// ImageResult is the generated image.
type ImageResult struct {
	Images []ImagePart
	Usage  Usage
}

// -----------------------------------------------------------------------------
// Vision -- the OpenAI-compatible chat route with image parts
// -----------------------------------------------------------------------------

// Vision runs a chat turn carrying images.
//
// It reuses the CHAT route rather than a separate one, because that is
// what the surface offers: a vision call is a chat call whose last user
// turn has a content ARRAY instead of a string. The result streams like
// any other generation, so the caller's emit sees tokens as they arrive.
func (c *openAIClient) Vision(ctx context.Context, req VisionRequest, emit Emit) (Result, error) {
	if len(req.Images) == 0 {
		return Result{}, fmt.Errorf("vision: no image was supplied")
	}
	return c.Chat(ctx, ChatRequest{
		Model:    req.Model,
		Messages: attachImages(req.Messages, req.Images),
		Params:   req.Params,
		Schema:   req.Schema,
	}, emit)
}

// attachImages marks the last user turn as carrying images.
//
// The marker is a sentinel on the Message rather than a second
// parameter threaded through openAIMessages, so the mapping stays one
// function with one shape. A conversation with NO user turn gets one
// appended: an image with no turn to belong to would otherwise be
// dropped silently, and a vision call that quietly became a text call
// answers confidently about nothing.
func attachImages(in []Message, images []ImagePart) []Message {
	for _, message := range in {
		if len(message.Images) > 0 {
			return in
		}
	}
	out := append([]Message(nil), in...)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Role == "user" {
			out[i].Images = images
			return out
		}
	}
	return append(out, Message{Role: "user", Images: images})
}

// Vision runs a chat turn carrying images against Ollama's NATIVE
// route.
//
// It has to exist, and the reason is worth stating: `vision=1` is set
// by the NATIVE probe -- Ollama's /api/show reports the capability --
// so a model discovered that way is advertised as seeing, and
// clientFor hands it an ollamaClient. Without this method the machine
// would advertise a modality its own serving path could not reach, and
// the call would be refused on a flag this cockpit set itself.
func (c *ollamaClient) Vision(ctx context.Context, req VisionRequest, emit Emit) (Result, error) {
	if len(req.Images) == 0 {
		return Result{}, fmt.Errorf("vision: no image was supplied")
	}
	return c.Chat(ctx, ChatRequest{
		Model:    req.Model,
		Messages: attachImages(req.Messages, req.Images),
		Params:   req.Params,
		Schema:   req.Schema,
	}, emit)
}

// -----------------------------------------------------------------------------
// Transcription -- input_audio on the same chat route
// -----------------------------------------------------------------------------

// Transcribe sends audio and returns the text.
//
// Through the CHAT route with an `input_audio` part rather than through
// /audio/transcriptions, and the reason is which servers implement
// which: Ollama's OpenAI-compatible surface serves audio-capable models
// (gemma4:e4b) on /chat/completions and does not implement the
// transcription route at all. A dedicated transcriber declared as its
// own runtime is reached the same way, because that is the route this
// cockpit's declared-runtime contract already promises.
func (c *openAIClient) Transcribe(ctx context.Context, req TranscribeRequest) (TranscribeResult, error) {
	if c.transcription == "openai" || c.transcription == "whisper-cpp" {
		return c.transcribeFile(ctx, req)
	}
	if len(req.Audio) == 0 {
		return TranscribeResult{}, fmt.Errorf("transcribe: no audio was supplied")
	}
	format := strings.TrimSpace(req.Format)
	if format == "" {
		// Named rather than sniffed: a wrong container guess produces a
		// transcript of noise where a stated one produces an error the
		// caller can read.
		return TranscribeResult{}, fmt.Errorf("transcribe: the audio format was not stated")
	}

	content := []map[string]any{{
		"type": "input_audio",
		"input_audio": map[string]any{
			"data":   base64Of(req.Audio),
			"format": format,
		},
	}}
	if p := strings.TrimSpace(req.Prompt); p != "" {
		content = append(content, map[string]any{"type": "text", "text": p})
	}

	var text strings.Builder
	res, err := c.chatRaw(ctx, map[string]any{
		"model":          req.Model,
		"messages":       []map[string]any{{"role": "user", "content": content}},
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}, func(chunk string) error {
		text.WriteString(chunk)
		return nil
	})
	if err != nil {
		return TranscribeResult{}, err
	}
	// NO SEGMENTS. The chat route returns text and nothing about when
	// each word was said, so this result carries none rather than
	// dividing the duration up by word count -- which would be a
	// timestamp the caller could seek on and land nowhere near.
	return TranscribeResult{Text: text.String(), Usage: res.Usage}, nil
}

// -----------------------------------------------------------------------------
// Speech -- /audio/speech on a declared runtime
// -----------------------------------------------------------------------------

// Speak generates audio.
//
// It is a method on the ORDINARY OpenAI-compatible client rather than
// on a client of its own, and that is the simplification worth
// noticing: `audioout` can only ever be true through a declared runtime
// (no probe anywhere reports it), a declared runtime is reached by
// clientFor as an openAIClient at the base URL the operator gave, and
// Kokoro's route is OpenAI-shaped. A separate speech client would be a
// second code path reached by nothing, and a machine advertising
// speech would then have to be special-cased into it.
//
// The response is BYTES, not JSON: /audio/speech answers with the audio
// file itself and states its container in Content-Type. Reading the
// header rather than assuming the requested format is what keeps the
// result honest when a runtime silently serves wav for an mp3 request,
// which several do.
func (c *openAIClient) Speak(ctx context.Context, req SpeakRequest) (SpeakResult, error) {
	if strings.TrimSpace(req.Text) == "" {
		return SpeakResult{}, fmt.Errorf("speak: no text was supplied")
	}
	body := map[string]any{"model": req.Model, "input": req.Text}
	if v := strings.TrimSpace(req.Voice); v != "" {
		if alias := c.voices[v]; alias != "" {
			v = alias
		}
		body["voice"] = v
	}
	if f := strings.TrimSpace(req.Format); f != "" {
		body["response_format"] = f
	}
	if req.SpeedSet {
		body["speed"] = req.Speed
	}

	resp, err := c.post(ctx, "/audio/speech", body)
	if err != nil {
		return SpeakResult{}, err
	}
	defer resp.Body.Close()

	audio, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil {
		return SpeakResult{}, err
	}
	if len(audio) > 16<<20 {
		return SpeakResult{}, fmt.Errorf("speech response exceeds 16 MB")
	}
	if len(audio) == 0 {
		// A 200 with an empty body is not success. The same trap
		// /memql/query and Ollama's /api/pull set: the status says the
		// request was accepted, and the absence of bytes is the failure.
		return SpeakResult{}, fmt.Errorf("speak: /audio/speech answered 200 with no audio")
	}
	media := resp.Header.Get("Content-Type")
	if media == "" {
		media = "audio/mpeg"
	}
	return SpeakResult{Audio: audio, MediaType: media}, nil
}

// -----------------------------------------------------------------------------
// Image generation
// -----------------------------------------------------------------------------

// GenerateImage on a declared runtime, through the OpenAI-shaped
// /images/generations route.
//
// It exists for the same reason Vision exists on the native client: an
// operator can declare `image_gen: true` on a declared runtime, and
// without this the machine would advertise a modality clientFor's own
// client could not serve. The refusal it would get instead is honest
// but useless -- the operator declared the thing, and the machine
// agreed, and then nothing served it.
//
// The response is base64 in JSON (`data[].b64_json`) rather than raw
// bytes, which is where it differs from /audio/speech next door.
func (c *openAIClient) GenerateImage(ctx context.Context, req ImageRequest) (ImageResult, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return ImageResult{}, fmt.Errorf("image: no prompt was supplied")
	}
	body := map[string]any{
		"model":           req.Model,
		"prompt":          req.Prompt,
		"response_format": "b64_json",
	}
	if req.Count > 0 {
		body["n"] = req.Count
	}
	// The route takes ONE size string rather than two numbers, and it
	// is sent only when BOTH dimensions were asked for: half a size is
	// not a size, and "1024x0" is a request no server can honour.
	if req.Width > 0 && req.Height > 0 {
		body["size"] = fmt.Sprintf("%dx%d", req.Width, req.Height)
	}

	resp, err := c.post(ctx, "/images/generations", body)
	if err != nil {
		return ImageResult{}, err
	}
	defer resp.Body.Close()

	var out struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ImageResult{}, fmt.Errorf("image: the runtime's response could not be read: %w", err)
	}
	if len(out.Data) == 0 {
		return ImageResult{}, fmt.Errorf("image: the runtime returned no image")
	}

	// NO USAGE. This route reports none, and the package's rule is that
	// usage is reported and never inferred -- so Known stays false and
	// the engine records billing "unknown", which is the truth.
	var res ImageResult
	for _, d := range out.Data {
		data, err := decodeBase64(d.B64JSON)
		if err != nil {
			return ImageResult{}, fmt.Errorf("image: the runtime returned an image that could not be decoded: %w", err)
		}
		res.Images = append(res.Images, ImagePart{MediaType: "image/png", Data: data})
	}
	return res, nil
}

// -----------------------------------------------------------------------------
// Image generation -- Ollama's own route
// -----------------------------------------------------------------------------

// GenerateImage runs an image-generation model.
//
// Ollama answers /api/generate with base64 images in an `images` array
// for a model whose capabilities include `image`. The native route
// rather than an OpenAI-shaped one, for the same reason inference.Pull
// uses /api/pull: this is the surface the runtime actually implements,
// and the compatibility layer does not carry image generation at all.
func (c *ollamaClient) GenerateImage(ctx context.Context, req ImageRequest) (ImageResult, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return ImageResult{}, fmt.Errorf("image: no prompt was supplied")
	}
	request := map[string]any{
		"model":  req.Model,
		"prompt": req.Prompt,
		"stream": false,
	}
	// The knobs are sent only when ASKED FOR. An unset dimension left
	// out entirely gets the model's native resolution; sending a zero
	// would be a request for a zero-pixel image, which some runtimes
	// honour.
	if opts := imageOptions(req); len(opts) > 0 {
		request["options"] = opts
	}
	resp, err := c.post(ctx, "/api/generate", request)
	if err != nil {
		return ImageResult{}, err
	}
	defer resp.Body.Close()

	var body struct {
		Images          []string `json:"images"`
		PromptEvalCount int64    `json:"prompt_eval_count"`
		EvalCount       int64    `json:"eval_count"`
		Model           string   `json:"model"`
		Error           string   `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ImageResult{}, fmt.Errorf("image: the runtime's response could not be read: %w", err)
	}
	// A 200 CARRYING AN ERROR is the shape Ollama uses, so the status is
	// checked and then the body is checked again.
	if e := strings.TrimSpace(body.Error); e != "" {
		return ImageResult{}, fmt.Errorf("image: %s", e)
	}
	if len(body.Images) == 0 {
		return ImageResult{}, fmt.Errorf("image: the runtime returned no image")
	}

	out := ImageResult{Usage: Usage{
		InputTokens:  body.PromptEvalCount,
		OutputTokens: body.EvalCount,
		Known:        body.PromptEvalCount > 0 || body.EvalCount > 0,
		Model:        body.Model,
	}}
	for _, encoded := range body.Images {
		data, err := decodeBase64(encoded)
		if err != nil {
			return ImageResult{}, fmt.Errorf("image: the runtime returned an image that could not be decoded: %w", err)
		}
		// Ollama states no media type for these; PNG is what its image
		// models emit. Named as an assumption rather than read from
		// somewhere it is not written.
		out.Images = append(out.Images, ImagePart{MediaType: "image/png", Data: data})
	}
	return out, nil
}

// imageOptions renders only the knobs the caller set.
func imageOptions(req ImageRequest) map[string]any {
	out := map[string]any{}
	if req.Width > 0 {
		out["width"] = req.Width
	}
	if req.Height > 0 {
		out["height"] = req.Height
	}
	if req.Count > 0 {
		out["n"] = req.Count
	}
	if f := strings.TrimSpace(req.Format); f != "" {
		out["format"] = f
	}
	return out
}
