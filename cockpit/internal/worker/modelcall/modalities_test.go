package modelcall

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
)

// One in-process test per kind against a fake runtime, which is issue
// #395's acceptance criterion. No GPU, no Ollama, no Kokoro: every
// request body is captured and asserted, so what this cockpit SENDS is
// pinned rather than assumed.

// captured is the request a fake runtime received.
type captured struct {
	path string
	body map[string]any
}

func fakeOpenAI(t *testing.T, seen *[]captured, sse ...string) *openAIClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		*seen = append(*seen, captured{path: r.URL.Path, body: body})
		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range sse {
			_, _ = w.Write([]byte("data: " + line + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	return &openAIClient{baseURL: srv.URL, http: srv.Client()}
}

// speechClient is an ordinary OpenAI-compatible client pointed at the
// stub. That is the whole shape of a speech runtime here: `audioout`
// can only be true through a DECLARED runtime, clientFor reaches one as
// an openAIClient, and /audio/speech is OpenAI-shaped -- so there is no
// separate speech client to build or to keep in step.
func speechClient(srv *httptest.Server) *openAIClient {
	return &openAIClient{baseURL: srv.URL, http: srv.Client()}
}

func deadSpeechClient() *openAIClient {
	return &openAIClient{baseURL: "http://127.0.0.1:1", http: http.DefaultClient}
}

func chunk(content string) string {
	return `{"model":"m","choices":[{"delta":{"content":"` + content + `"}}]}`
}

// -----------------------------------------------------------------------------
// Vision
// -----------------------------------------------------------------------------

// A vision call is a CHAT call whose last user turn carries a content
// array. The image travels as a data URL, and the text part goes first.
func TestVisionSendsImagePartsOnTheLastUserTurn(t *testing.T) {
	var seen []captured
	c := fakeOpenAI(t, &seen, chunk("a cat"))

	res, err := c.Vision(context.Background(), VisionRequest{
		Model: "seeing:9b",
		Messages: []Message{
			{Role: "system", Content: "Be brief."},
			{Role: "user", Content: "What is this?"},
		},
		Images: []ImagePart{{MediaType: "image/png", Data: []byte("PNGDATA")}},
	}, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if res.FinishReason != FinishStop {
		t.Fatalf("finish = %q", res.FinishReason)
	}
	if len(seen) != 1 || seen[0].path != "/chat/completions" {
		t.Fatalf("a vision call must use the chat route: %+v", seen)
	}

	msgs := seen[0].body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages", len(msgs))
	}
	// The system turn keeps a plain string; only the image-carrying
	// turn becomes an array.
	if _, ok := msgs[0].(map[string]any)["content"].(string); !ok {
		t.Fatalf("the system turn became an array: %v", msgs[0])
	}

	parts, ok := msgs[1].(map[string]any)["content"].([]any)
	if !ok {
		t.Fatalf("the user turn did not become a content array: %v", msgs[1])
	}
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want text then image", len(parts))
	}
	first := parts[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "What is this?" {
		t.Fatalf("the text part must come first and keep the prompt: %v", first)
	}
	second := parts[1].(map[string]any)
	url := second["image_url"].(map[string]any)["url"].(string)
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("PNGDATA"))
	if url != want {
		t.Fatalf("image url =\n  %q\nwant\n  %q", url, want)
	}
}

// The images ride the LAST user turn, not the first: attaching them to
// the first asks the model about an image several turns of context away
// from the question.
func TestVisionAttachesToTheLastUserTurn(t *testing.T) {
	got := attachImages([]Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "ok"},
		{Role: "user", Content: "second"},
	}, []ImagePart{{Data: []byte("x")}})

	if len(got[0].Images) != 0 {
		t.Error("images were attached to the first user turn")
	}
	if len(got[2].Images) != 1 {
		t.Error("images were not attached to the last user turn")
	}
}

// A conversation with NO user turn gets one, rather than dropping the
// image: a vision call that quietly became a text call answers
// confidently about nothing.
func TestVisionAppendsAUserTurnWhenThereIsNone(t *testing.T) {
	got := attachImages([]Message{{Role: "system", Content: "Be brief."}},
		[]ImagePart{{Data: []byte("x")}})
	if len(got) != 2 || got[1].Role != "user" || len(got[1].Images) != 1 {
		t.Fatalf("no user turn was appended for the image: %+v", got)
	}
}

// A vision call with no image is refused rather than served as a chat
// call. Serving it would answer confidently about an image nobody sent.
func TestVisionRefusesWithNoImage(t *testing.T) {
	var seen []captured
	c := fakeOpenAI(t, &seen)
	if _, err := c.Vision(context.Background(), VisionRequest{Model: "m"}, func(string) error { return nil }); err == nil {
		t.Fatal("want a refusal for a vision call with no image")
	}
	if len(seen) != 0 {
		t.Fatal("the runtime was called for a vision request with no image")
	}
}

// A part with no media type still produces a usable data URL. An empty
// type is rejected by the URL parser before any decoder sees it.
func TestImagePartWithNoMediaTypeStillEncodes(t *testing.T) {
	got := ImagePart{Data: []byte("x")}.dataURL()
	if !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Fatalf("dataURL = %q", got)
	}
}

// -----------------------------------------------------------------------------
// Transcription
// -----------------------------------------------------------------------------

func TestTranscribeSendsInputAudio(t *testing.T) {
	var seen []captured
	c := fakeOpenAI(t, &seen, chunk("hello "), chunk("world"))

	res, err := c.Transcribe(context.Background(), TranscribeRequest{
		Model: "gemma4:e4b", Audio: []byte("RIFFWAVE"), Format: "wav",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "hello world" {
		t.Fatalf("text = %q, want the concatenated stream", res.Text)
	}

	msgs := seen[0].body["messages"].([]any)
	parts := msgs[0].(map[string]any)["content"].([]any)
	audio := parts[0].(map[string]any)
	if audio["type"] != "input_audio" {
		t.Fatalf("part type = %v, want input_audio", audio["type"])
	}
	inner := audio["input_audio"].(map[string]any)
	if inner["format"] != "wav" {
		t.Fatalf("format = %v, want the STATED container", inner["format"])
	}
	if inner["data"] != base64.StdEncoding.EncodeToString([]byte("RIFFWAVE")) {
		t.Fatalf("audio data was not base64 of the bytes sent")
	}
}

// The format is STATED, never sniffed: a wrong container guess produces
// a transcript of noise where a stated one produces an error a caller
// can read.
func TestTranscribeRefusesWithNoStatedFormat(t *testing.T) {
	var seen []captured
	c := fakeOpenAI(t, &seen)
	if _, err := c.Transcribe(context.Background(), TranscribeRequest{Model: "m", Audio: []byte("x")}); err == nil {
		t.Fatal("want a refusal when the audio format was not stated")
	}
	if len(seen) != 0 {
		t.Fatal("the runtime was called with an unstated audio format")
	}
}

func TestTranscribeRefusesWithNoAudio(t *testing.T) {
	var seen []captured
	c := fakeOpenAI(t, &seen)
	if _, err := c.Transcribe(context.Background(), TranscribeRequest{Model: "m", Format: "wav"}); err == nil {
		t.Fatal("want a refusal for a transcription with no audio")
	}
}

// An optional prompt rides alongside the audio as a text part.
func TestTranscribeCarriesAnOptionalPrompt(t *testing.T) {
	var seen []captured
	c := fakeOpenAI(t, &seen, chunk("ok"))
	if _, err := c.Transcribe(context.Background(), TranscribeRequest{
		Model: "m", Audio: []byte("x"), Format: "wav", Prompt: "product names: MemQL",
	}); err != nil {
		t.Fatal(err)
	}
	parts := seen[0].body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want the audio and the prompt", len(parts))
	}
	if parts[1].(map[string]any)["text"] != "product names: MemQL" {
		t.Fatalf("the prompt was not carried: %v", parts[1])
	}
}

// -----------------------------------------------------------------------------
// Speech
// -----------------------------------------------------------------------------

func TestSpeakReachesTheSpeechRuntime(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte("RIFFWAVEDATA"))
	}))
	t.Cleanup(srv.Close)

	res, err := speechClient(srv).Speak(context.Background(), SpeakRequest{
		Model: "kokoro-82m", Text: "hello", Voice: "af_bella", Format: "wav",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/audio/speech" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody["input"] != "hello" || gotBody["voice"] != "af_bella" || gotBody["response_format"] != "wav" {
		t.Fatalf("body = %v", gotBody)
	}
	if string(res.Audio) != "RIFFWAVEDATA" {
		t.Fatalf("audio = %q", res.Audio)
	}
	// The media type comes from the RESPONSE HEADER, not from the
	// format asked for: several runtimes silently serve wav for an mp3
	// request, and a result that stated the request would be wrong.
	if res.MediaType != "audio/wav" {
		t.Fatalf("media type = %q, want the server's own header", res.MediaType)
	}
}

// An unset voice and format are OMITTED rather than defaulted here. A
// voice id is a property of the model that is installed, and a name
// invented by this code is a call that fails.
func TestSpeakOmitsAnUnsetVoiceAndFormat(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte("audio"))
	}))
	t.Cleanup(srv.Close)

	if _, err := speechClient(srv).Speak(context.Background(),
		SpeakRequest{Model: "kokoro-82m", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := gotBody["voice"]; ok {
		t.Error("an unset voice was sent")
	}
	if _, ok := gotBody["response_format"]; ok {
		t.Error("an unset format was sent")
	}
}

// A 200 WITH NO BYTES IS NOT SUCCESS -- the same trap /memql/query and
// Ollama's /api/pull set. A caller that trusted the status would hand
// back an empty audio file as a finished generation.
func TestSpeakRefusesAnEmptyTwoHundred(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	_, err := speechClient(srv).Speak(context.Background(),
		SpeakRequest{Model: "m", Text: "hi"})
	if err == nil {
		t.Fatal("want a refusal for a 200 carrying no audio")
	}
	if !strings.Contains(err.Error(), "no audio") {
		t.Fatalf("err = %v", err)
	}
}

func TestSpeakRefusesWithNoText(t *testing.T) {
	if _, err := deadSpeechClient().Speak(context.Background(),
		SpeakRequest{Model: "m"}); err == nil {
		t.Fatal("want a refusal for a speak call with no text")
	}
}

// -----------------------------------------------------------------------------
// Image generation
// -----------------------------------------------------------------------------

func TestImageReachesOllamaGenerate(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"model":"x/z-image-turbo","images":["` +
			base64.StdEncoding.EncodeToString([]byte("PNGBYTES")) +
			`"],"prompt_eval_count":12,"eval_count":0}`))
	}))
	t.Cleanup(srv.Close)

	res, err := (&ollamaClient{baseURL: srv.URL, http: srv.Client()}).GenerateImage(
		context.Background(), ImageRequest{Model: "x/z-image-turbo", Prompt: "a red cube"})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/generate" {
		t.Fatalf("path = %q, want the NATIVE route", gotPath)
	}
	if gotBody["prompt"] != "a red cube" || gotBody["stream"] != false {
		t.Fatalf("body = %v", gotBody)
	}
	if len(res.Images) != 1 || string(res.Images[0].Data) != "PNGBYTES" {
		t.Fatalf("images = %+v", res.Images)
	}
	if !res.Usage.Known || res.Usage.InputTokens != 12 {
		t.Fatalf("usage = %+v, want what the runtime reported", res.Usage)
	}
}

// A 200 CARRYING AN ERROR is the shape Ollama uses, so the body is
// checked after the status.
func TestImageReadsAnErrorInsideATwoHundred(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"error":"model x/z-image-turbo not found"}`))
	}))
	t.Cleanup(srv.Close)

	_, err := (&ollamaClient{baseURL: srv.URL, http: srv.Client()}).GenerateImage(
		context.Background(), ImageRequest{Model: "x/z-image-turbo", Prompt: "a cube"})
	if err == nil {
		t.Fatal("want a refusal for an error inside a 200")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want the runtime's own words", err)
	}
}

// A 200 with no images is a failure too: the other direction hands back
// an empty result as a finished generation.
func TestImageRefusesWhenNoImageCameBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"model":"m","images":[]}`))
	}))
	t.Cleanup(srv.Close)

	if _, err := (&ollamaClient{baseURL: srv.URL, http: srv.Client()}).GenerateImage(
		context.Background(), ImageRequest{Model: "m", Prompt: "x"}); err == nil {
		t.Fatal("want a refusal when no image came back")
	}
}

// Usage is REPORTED, never inferred: a runtime that counted nothing
// leaves Known false, which the engine records as billing "unknown".
func TestImageUsageStaysUnknownWhenTheRuntimeCountedNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"images":["` + base64.StdEncoding.EncodeToString([]byte("x")) + `"]}`))
	}))
	t.Cleanup(srv.Close)

	res, err := (&ollamaClient{baseURL: srv.URL, http: srv.Client()}).GenerateImage(
		context.Background(), ImageRequest{Model: "m", Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage.Known {
		t.Fatalf("usage was claimed known with no counts: %+v", res.Usage)
	}
}

// -----------------------------------------------------------------------------
// Encoding
// -----------------------------------------------------------------------------

// Padded and unpadded both decode. Some runtimes strip trailing '='
// when they concatenate, and refusing that would fail a decode over
// punctuation.
func TestDecodeBase64ToleratesAMissingPad(t *testing.T) {
	raw := []byte("abcde")
	padded := base64.StdEncoding.EncodeToString(raw)
	unpadded := base64.RawStdEncoding.EncodeToString(raw)
	for _, in := range []string{padded, unpadded} {
		got, err := decodeBase64(in)
		if err != nil {
			t.Fatalf("decodeBase64(%q): %v", in, err)
		}
		if string(got) != string(raw) {
			t.Fatalf("decodeBase64(%q) = %q", in, got)
		}
	}
	if _, err := decodeBase64("not base64 at all!!"); err == nil {
		t.Fatal("want an error for input that is not base64")
	}
}

// -----------------------------------------------------------------------------
// The dispatch gates
// -----------------------------------------------------------------------------

// startModality builds a start envelope for a modality kind. It carries
// no payload, because there is no field on ModelCallStart to put one in
// -- which is exactly the state the refusal below is about.
func startModality(model, kind string) *memqlv1.ModelCallStart {
	return &memqlv1.ModelCallStart{
		RequestId: "r1", Model: model, Kind: kind,
		Limits: &memqlv1.ModelCallLimits{TimeoutSeconds: 10, IdleTimeoutSeconds: 5, KeepaliveSeconds: 1},
	}
}

func endFor(t *testing.T, inv stubInventory, s *memqlv1.ModelCallStart) *memqlv1.ModelCallEnd {
	t.Helper()
	rec := newRecorder()
	m := NewManager(Options{Inventory: inv})
	m.Start(context.Background(), rec, s)
	return rec.wait(t)
}

// A modality the model never advertised is refused with a code that
// names the fix -- the same gate structured output and tools already
// get, and for the same reason: the router only sends a kind to a
// machine that advertised it, so a silent downgrade here would defeat
// the gating that put the call here.
func TestModalityKindRefusedWhenNotAdvertised(t *testing.T) {
	for _, tc := range []struct{ kind, word string }{
		{KindVision, "vision"},
		{KindTranscribe, "transcription"},
		{KindSpeak, "speech"},
		{KindImage, "image generation"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			inv := inventoryWith(ollamaModel("http://127.0.0.1:1", "m:9b", models.Attributes{}))
			end := endFor(t, inv, startModality("m:9b", tc.kind))

			if end.GetErrorCode() != CodeModalityUnsupported {
				t.Fatalf("error_code = %q, want %q", end.GetErrorCode(), CodeModalityUnsupported)
			}
			if !strings.Contains(end.GetError(), tc.word) {
				t.Fatalf("the refusal must name the modality: %q", end.GetError())
			}
		})
	}
}

// Advertising a modality does not permit an empty required input.
func TestAdvertisedModalityRefusesOnTheAbsentPayload(t *testing.T) {
	for _, tc := range []struct {
		kind  string
		attrs models.Attributes
	}{
		{KindVision, models.Attributes{Vision: true}},
		{KindTranscribe, models.Attributes{AudioIn: true}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			inv := inventoryWith(ollamaModel("http://127.0.0.1:1", "m:9b", tc.attrs))
			end := endFor(t, inv, startModality("m:9b", tc.kind))

			if end.GetErrorCode() != CodePayloadUnavailable {
				t.Fatalf("error_code = %q, want %q -- got error %q",
					end.GetErrorCode(), CodePayloadUnavailable, end.GetError())
			}
			if !strings.Contains(end.GetError(), "carried no ") {
				t.Fatalf("the refusal must say WHY there is no payload: %q", end.GetError())
			}
		})
	}
}

// The two refusals are DIFFERENT CODES, and that is load-bearing: an
// unknown-to-this-machine modality is a stale advertisement or a policy
// change, where an absent payload is a version skew between the engine
// and this cockpit. The refusal report lists every machine considered
// and why, so two causes that send an operator to different places get
// two codes.
func TestModalityRefusalCodesAreDistinct(t *testing.T) {
	if CodeModalityUnsupported == CodePayloadUnavailable {
		t.Fatal("the two modality refusals must not share a code")
	}
	if CodeModalityUnsupported == CodeUnsupportedKind {
		t.Fatal("an unadvertised modality and an unknown kind must not share a code")
	}
}

// An unknown kind still refuses, and the refusal now names every kind
// this worker serves -- a router that sent an unknown one is a version
// skew, and the operator reading the report needs to see which side is
// behind.
func TestUnknownKindRefusalNamesEveryServedKind(t *testing.T) {
	inv := inventoryWith(ollamaModel("http://127.0.0.1:1", "m:9b", models.Attributes{}))
	end := endFor(t, inv, startModality("m:9b", "telepathy"))

	if end.GetErrorCode() != CodeUnsupportedKind {
		t.Fatalf("error_code = %q", end.GetErrorCode())
	}
	for _, kind := range ServedKinds() {
		if !strings.Contains(end.GetError(), `"`+kind+`"`) {
			t.Errorf("the refusal must name %q: %s", kind, end.GetError())
		}
	}
}

// Chat and embedding are untouched by the modality gate. The most
// likely way to break this change is to make an ordinary chat call take
// the modality path.
func TestChatAndEmbeddingAreNotModalityGated(t *testing.T) {
	for _, kind := range []string{KindChat, KindEmbedding} {
		if _, isModality := modalityKinds[kind]; isModality {
			t.Fatalf("%q was gated as a modality", kind)
		}
	}
}

// Every modality kind has exactly one entry in the table that maps it
// to a flag and a word. Three switch statements would be three places
// for a modality to be half-added; this is the one.
func TestEveryModalityKindIsInTheTable(t *testing.T) {
	want := []string{KindVision, KindTranscribe, KindSpeak, KindImage}
	if len(modalityKinds) != len(want) {
		t.Fatalf("%d entries for %d modality kinds", len(modalityKinds), len(want))
	}
	for _, kind := range want {
		entry, ok := modalityKinds[kind]
		if !ok {
			t.Fatalf("%q has no entry", kind)
		}
		if entry.Word == "" || entry.Advertised == nil {
			t.Fatalf("%q has an incomplete entry: %+v", kind, entry)
		}
	}
	// And ServedKinds lists all six, which is what the unknown-kind
	// refusal prints.
	if len(ServedKinds()) != len(want)+2 {
		t.Fatalf("ServedKinds = %v, want chat, embedding and the four modalities", ServedKinds())
	}
}

// -----------------------------------------------------------------------------
// The settled proto shape (memql#5137, agreed 2026-09-07)
// -----------------------------------------------------------------------------

// A media type this side does not recognise yields NO container, which
// Transcribe then refuses by name. The fail-closed direction: passing
// an unknown container through would have the runtime decode the bytes
// as something they are not, and the result is a confident transcript
// of noise rather than a failure anybody notices.
func TestAudioFormatFor(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"audio/wav", "wav"},
		{"audio/x-wav", "wav"},
		{"audio/wave", "wav"},
		{"AUDIO/WAV", "wav"},
		{"  audio/wav  ", "wav"},
		{"audio/mpeg", "mp3"},
		{"audio/mp3", "mp3"},
		{"audio/ogg", "opus"},
		{"audio/opus", "opus"},
		{"audio/flac", "flac"},
		{"audio/m4a", "m4a"},
		{"audio/webm", "webm"},
		// Unrecognised, empty, and a plausible-looking near-miss.
		{"audio/aiff", ""},
		{"", ""},
		{"wav", ""},
		{"video/mp4", ""},
	} {
		if got := audioFormatFor(tc.in); got != tc.want {
			t.Errorf("audioFormatFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The speak knobs travel only when SET. Speed carries an explicit
// SpeedSet for the reason Params.Temperature does: zero is a meaningful
// value in the type and is not the same request as "no preference" --
// and a speed of 0 sent as a preference is a call that generates
// nothing.
func TestSpeakSendsSpeedOnlyWhenSet(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  SpeakRequest
		want bool
	}{
		{"unset", SpeakRequest{Model: "m", Text: "hi"}, false},
		{"set to zero", SpeakRequest{Model: "m", Text: "hi", SpeedSet: true}, true},
		{"set", SpeakRequest{Model: "m", Text: "hi", Speed: 1.25, SpeedSet: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&body)
				_, _ = w.Write([]byte("audio"))
			}))
			t.Cleanup(srv.Close)

			if _, err := speechClient(srv).Speak(context.Background(), tc.req); err != nil {
				t.Fatal(err)
			}
			_, present := body["speed"]
			if present != tc.want {
				t.Fatalf("speed present = %v, want %v (body %v)", present, tc.want, body)
			}
		})
	}
}

// The image knobs the same way: an unset dimension is LEFT OUT so the
// model's native resolution applies. Sending a zero would be a request
// for a zero-pixel image, which some runtimes honour.
func TestImageSendsOnlyTheKnobsThatWereSet(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"images":["` + base64.StdEncoding.EncodeToString([]byte("x")) + `"]}`))
	}))
	t.Cleanup(srv.Close)
	c := &ollamaClient{baseURL: srv.URL, http: srv.Client()}

	if _, err := c.GenerateImage(context.Background(), ImageRequest{Model: "m", Prompt: "a cube"}); err != nil {
		t.Fatal(err)
	}
	if _, present := body["options"]; present {
		t.Fatalf("options were sent for a request that set none: %v", body)
	}

	body = nil
	if _, err := c.GenerateImage(context.Background(), ImageRequest{
		Model: "m", Prompt: "a cube", Width: 1024, Format: "png",
	}); err != nil {
		t.Fatal(err)
	}
	opts := body["options"].(map[string]any)
	if opts["width"] != float64(1024) || opts["format"] != "png" {
		t.Fatalf("options = %v", opts)
	}
	if _, present := opts["height"]; present {
		t.Fatalf("an unset height was sent: %v", opts)
	}
}

// The chat route returns text and nothing about WHEN each word was
// said, so a one-shot transcription carries NO segments rather than
// dividing the duration up -- which would be a timestamp the caller
// could seek on and land nowhere near.
func TestTranscribeReportsNoSegmentsItDidNotReceive(t *testing.T) {
	var seen []captured
	c := fakeOpenAI(t, &seen, chunk("hello world"))

	res, err := c.Transcribe(context.Background(), TranscribeRequest{
		Model: "m", Audio: []byte("x"), Format: "wav",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Segments) != 0 {
		t.Fatalf("segments were invented: %+v", res.Segments)
	}
	if res.Text != "hello world" {
		t.Fatalf("text = %q", res.Text)
	}
}

// -----------------------------------------------------------------------------
// The serving path, end to end
// -----------------------------------------------------------------------------

// withPayload isolates modality-specific serving behavior; wire tests use real fields.
func withPayload(t *testing.T, p Payload) {
	t.Helper()
	prev := readPayload
	readPayload = func(*memqlv1.ModelCallStart) (Payload, bool) { return p, true }
	t.Cleanup(func() { readPayload = prev })
}

// A vision call reaches the runtime WITH ITS IMAGE, through the native
// Ollama route -- which is the one that matters, because `vision=1` is
// set by the native probe and clientFor hands those models an
// ollamaClient.
func TestVisionCallReachesTheNativeRuntimeWithItsImage(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"model":"m","message":{"content":"a cat"},"done":true,"done_reason":"stop"}` + "\n"))
	}))
	t.Cleanup(srv.Close)

	withPayload(t, Payload{Images: []ImagePart{{MediaType: "image/png", Data: []byte("PNGDATA")}}})

	rec := newRecorder()
	m := NewManager(Options{Inventory: inventoryWith(
		ollamaModel(srv.URL, "seeing:9b", models.Attributes{Vision: true, MaxConcurrent: 1}))})
	s := start("r", "seeing:9b", KindVision)
	m.Start(context.Background(), rec, s)

	end := rec.wait(t)
	if end.GetErrorCode() != "" {
		t.Fatalf("end = %+v", end)
	}
	if rec.content() != "a cat" {
		t.Fatalf("content = %q", rec.content())
	}

	msgs := body["messages"].([]any)
	images, ok := msgs[len(msgs)-1].(map[string]any)["images"].([]any)
	if !ok || len(images) != 1 {
		t.Fatalf("the image did not reach the runtime: %v", msgs)
	}
	// Ollama's NATIVE shape is a BARE base64 string, not a data URL.
	// Sending a data: URL here is accepted and decoded as bytes that
	// begin with the literal text "data:image/png;base64,", so the
	// model is shown noise and answers about it confidently.
	if images[0] != base64.StdEncoding.EncodeToString([]byte("PNGDATA")) {
		t.Fatalf("image = %v, want bare base64", images[0])
	}
}

// A transcription's TEXT rides the deltas, exactly as a generation's
// does -- so a caller that streams and a caller that does not are the
// same shape on the other side.
func TestTranscribeCallStreamsItsTranscriptAsDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("data: " + chunk("hello world") + "\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)

	withPayload(t, Payload{Audio: []byte("RIFF"), AudioMediaType: "audio/wav"})

	rec := newRecorder()
	m := NewManager(Options{Inventory: inventoryWith(models.Info{
		ID: "hear:9b", Kind: models.KindOpenAICompatible, Runtime: "declared",
		BaseURL: srv.URL, Allowed: true,
		Attributes: models.Attributes{AudioIn: true, MaxConcurrent: 1},
	})})
	m.Start(context.Background(), rec, start("r", "hear:9b", KindTranscribe))

	end := rec.wait(t)
	if end.GetErrorCode() != "" {
		t.Fatalf("end = %+v", end)
	}
	if rec.content() != "hello world" {
		t.Fatalf("content = %q, want the transcript as deltas", rec.content())
	}
}

// A media type this machine cannot name a container for is REFUSED
// rather than guessed: a wrong container has the runtime decode the
// bytes as something they are not, and the result is a confident
// transcript of noise.
func TestTranscribeCallRefusesAnUnnameableContainer(t *testing.T) {
	withPayload(t, Payload{Audio: []byte("x"), AudioMediaType: "audio/aiff"})

	rec := newRecorder()
	m := NewManager(Options{Inventory: inventoryWith(models.Info{
		ID: "hear:9b", Kind: models.KindOpenAICompatible, BaseURL: "http://127.0.0.1:1", Allowed: true,
		Attributes: models.Attributes{AudioIn: true, MaxConcurrent: 1},
	})})
	m.Start(context.Background(), rec, start("r", "hear:9b", KindTranscribe))

	end := rec.wait(t)
	if end.GetErrorCode() == "" {
		t.Fatal("an unnameable container was accepted")
	}
	if !strings.Contains(end.GetError(), "audio/aiff") {
		t.Fatalf("the refusal must name what arrived: %q", end.GetError())
	}
}

// A speak call's TEXT comes from the last user turn -- there is no
// separate input field on the wire (memql#5137) -- and the knobs come
// from the payload.
func TestSpeakCallTakesItsTextFromTheLastUserTurn(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte("RIFFWAVE"))
	}))
	t.Cleanup(srv.Close)

	withPayload(t, Payload{Speech: SpeakRequest{Voice: "af_bella", Format: "wav"}})

	rec := newRecorder()
	m := NewManager(Options{Inventory: inventoryWith(models.Info{
		ID: "kokoro-82m", Kind: models.KindOpenAICompatible, BaseURL: srv.URL, Allowed: true,
		Attributes: models.Attributes{AudioOut: true, MaxConcurrent: 1},
	})})
	s := start("r", "kokoro-82m", KindSpeak)
	s.Messages = []*memqlv1.ModelCallMessage{
		{Role: "system", Content: "Be brief."},
		{Role: "user", Content: "say this out loud"},
	}
	m.Start(context.Background(), rec, s)

	end := rec.wait(t)
	if end.GetErrorCode() != "" {
		t.Fatalf("end = %+v", end)
	}
	if body["input"] != "say this out loud" {
		t.Fatalf("input = %v, want the last user turn", body["input"])
	}
	if body["voice"] != "af_bella" {
		t.Fatalf("the payload's knobs did not reach the runtime: %v", body)
	}
}

// And an image call the same way: prompt from the messages, dimensions
// from the payload.
func TestImageCallTakesItsPromptFromTheMessages(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"images":["` + base64.StdEncoding.EncodeToString([]byte("PNG")) + `"]}`))
	}))
	t.Cleanup(srv.Close)

	withPayload(t, Payload{Image: ImageRequest{Width: 1024, Height: 1024}})

	rec := newRecorder()
	m := NewManager(Options{Inventory: inventoryWith(
		ollamaModel(srv.URL, "x/z-image-turbo", models.Attributes{ImageGen: true, MaxConcurrent: 1}))})
	s := start("r", "x/z-image-turbo", KindImage)
	s.Messages = []*memqlv1.ModelCallMessage{{Role: "user", Content: "a red cube"}}
	m.Start(context.Background(), rec, s)

	end := rec.wait(t)
	if end.GetErrorCode() != "" {
		t.Fatalf("end = %+v", end)
	}
	if body["prompt"] != "a red cube" {
		t.Fatalf("prompt = %v, want the user turn", body["prompt"])
	}
	opts := body["options"].(map[string]any)
	if opts["width"] != float64(1024) {
		t.Fatalf("the payload's knobs did not reach the runtime: %v", opts)
	}
}

// A RUNTIME THAT CANNOT SERVE THE MODALITY IT WAS ADVERTISED FOR is
// named, not crashed into. A call reaches runModality only when the
// machine advertised the flag, so this means the advertisement and the
// runtime disagree -- and the operator needs the model and the modality
// to find out which.
func TestAModalityTheRuntimeCannotServeNamesTheDisagreement(t *testing.T) {
	withPayload(t, Payload{Speech: SpeakRequest{}})

	rec := newRecorder()
	// A NATIVE Ollama model claiming audioout: Ollama serves no speech,
	// so ollamaClient implements no Speaker.
	m := NewManager(Options{Inventory: inventoryWith(
		ollamaModel("http://127.0.0.1:1", "confused:9b", models.Attributes{AudioOut: true, MaxConcurrent: 1}))})
	m.Start(context.Background(), rec, start("r", "confused:9b", KindSpeak))

	end := rec.wait(t)
	if end.GetErrorCode() == "" {
		t.Fatal("a runtime that cannot speak served a speak call")
	}
	for _, want := range []string{"confused:9b", "speech", "ollama"} {
		if !strings.Contains(end.GetError(), want) {
			t.Errorf("the refusal must name %q: %s", want, end.GetError())
		}
	}
}

// EVERY FLAG A PROBE OR A DECLARATION CAN SET MUST HAVE A SERVING PATH
// BEHIND IT. This is the inversion that matters most in this package:
// a machine that advertises what it cannot serve takes a call and fails
// it on somebody else's prompt.
//
// The table is the whole matrix of (how the flag becomes true) x (which
// client clientFor then hands the call to), and each row asserts the
// capability interface is actually implemented.
func TestEveryAdvertisableModalityHasAServingPath(t *testing.T) {
	native := &ollamaClient{baseURL: "http://127.0.0.1:1", http: http.DefaultClient}
	declared := &openAIClient{baseURL: "http://127.0.0.1:1", http: http.DefaultClient}

	for _, tc := range []struct {
		flag, source string
		client       any
		implements   func(any) bool
	}{
		// vision and imagegen can be set by the NATIVE probe (Ollama's
		// /api/show reports both capabilities), so the native client
		// must serve both.
		{"vision", "the native Ollama probe", native, func(c any) bool { _, ok := c.(VisionClient); return ok }},
		{"imagegen", "the native Ollama probe", native, func(c any) bool { _, ok := c.(ImageGenerator); return ok }},

		// All four can be set by an operator's DECLARATION, which
		// clientFor reaches as an openAIClient.
		{"vision", "a declared runtime", declared, func(c any) bool { _, ok := c.(VisionClient); return ok }},
		{"audioin", "a declared runtime", declared, func(c any) bool { _, ok := c.(Transcriber); return ok }},
		{"audioout", "a declared runtime", declared, func(c any) bool { _, ok := c.(Speaker); return ok }},
		{"imagegen", "a declared runtime", declared, func(c any) bool { _, ok := c.(ImageGenerator); return ok }},
	} {
		t.Run(tc.flag+" via "+tc.source, func(t *testing.T) {
			if !tc.implements(tc.client) {
				t.Fatalf("%s can be advertised through %s and the client it routes to cannot serve it",
					tc.flag, tc.source)
			}
		})
	}
}

// A declared image runtime reaches /images/generations, whose response
// is base64 in JSON rather than the raw bytes /audio/speech returns.
func TestImageCallOnADeclaredRuntime(t *testing.T) {
	var path string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"` +
			base64.StdEncoding.EncodeToString([]byte("PNG")) + `"}]}`))
	}))
	t.Cleanup(srv.Close)

	res, err := (&openAIClient{baseURL: srv.URL, http: srv.Client()}).GenerateImage(
		context.Background(), ImageRequest{Model: "flux", Prompt: "a red cube", Width: 1024, Height: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/images/generations" {
		t.Fatalf("path = %q", path)
	}
	if body["size"] != "1024x1024" {
		t.Fatalf("size = %v", body["size"])
	}
	if len(res.Images) != 1 || string(res.Images[0].Data) != "PNG" {
		t.Fatalf("images = %+v", res.Images)
	}
	// Usage is REPORTED, never inferred: this route reports none.
	if res.Usage.Known {
		t.Fatalf("usage was claimed known on a route that reports none: %+v", res.Usage)
	}
}

// HALF A SIZE IS NOT A SIZE. "1024x0" is a request no server can
// honour, so a request naming only one dimension sends none.
func TestImageCallOmitsAHalfSize(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"` +
			base64.StdEncoding.EncodeToString([]byte("x")) + `"}]}`))
	}))
	t.Cleanup(srv.Close)

	if _, err := (&openAIClient{baseURL: srv.URL, http: srv.Client()}).GenerateImage(
		context.Background(), ImageRequest{Model: "m", Prompt: "x", Width: 1024}); err != nil {
		t.Fatal(err)
	}
	if _, present := body["size"]; present {
		t.Fatalf("a half size was sent: %v", body)
	}
}

// A SCHEMA IS HONOURED OR THE CALL FAILS, and a vision call is a chat
// call -- so the schema has to reach the runtime. Dropping it silently
// answers prose to a call the router only sent here because this
// machine advertised structured output, and the parse failure surfaces
// three layers away naming nothing.
func TestVisionCallCarriesItsSchemaToTheRuntime(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"model":"m","message":{"content":"{}"},"done":true,"done_reason":"stop"}` + "\n"))
	}))
	t.Cleanup(srv.Close)

	withPayload(t, Payload{Images: []ImagePart{{MediaType: "image/png", Data: []byte("P")}}})

	rec := newRecorder()
	m := NewManager(Options{Inventory: inventoryWith(ollamaModel(srv.URL, "seeing:9b",
		models.Attributes{Vision: true, StructuredOutput: true, MaxConcurrent: 1}))})
	s := start("r", "seeing:9b", KindVision)
	s.ResponseFormatSchema = []byte(`{"type":"object","required":["caption"]}`)
	m.Start(context.Background(), rec, s)

	if end := rec.wait(t); end.GetErrorCode() != "" {
		t.Fatalf("end = %+v", end)
	}
	if _, present := body["format"]; !present {
		t.Fatalf("the schema never reached the runtime: request keys %v", keysOf(body))
	}
}

// THE OTHER THREE KINDS RETURN NO TEXT for a schema to constrain -- a
// transcript, audio bytes, image bytes -- so a schema arriving on one
// is REFUSED rather than dropped. Dropping it lets a caller believe it
// asked for something.
func TestASchemaOnANonChatModalityIsRefused(t *testing.T) {
	for _, kind := range []string{KindTranscribe, KindSpeak, KindImage} {
		t.Run(kind, func(t *testing.T) {
			rec := newRecorder()
			m := NewManager(Options{Inventory: inventoryWith(models.Info{
				ID: "m", Kind: models.KindOpenAICompatible, BaseURL: "http://127.0.0.1:1", Allowed: true,
				Attributes: models.Attributes{
					AudioIn: true, AudioOut: true, ImageGen: true,
					StructuredOutput: true, MaxConcurrent: 1,
				},
			})})
			s := startModality("m", kind)
			s.ResponseFormatSchema = []byte(`{"type":"object"}`)
			m.Start(context.Background(), rec, s)

			end := rec.wait(t)
			if end.GetErrorCode() != CodeSchemaUnsupported {
				t.Fatalf("error_code = %q, want %q (%q)", end.GetErrorCode(), CodeSchemaUnsupported, end.GetError())
			}
			if !strings.Contains(end.GetError(), "no text for a response schema") {
				t.Fatalf("the refusal must say WHY: %q", end.GetError())
			}
		})
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
