package modelcall

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func TestTranscriptionWireReachesFileRuntime(t *testing.T) {
	for _, protocol := range []string{"openai", "whisper-cpp"} {
		t.Run(protocol, func(t *testing.T) {
			var gotPath, gotAudio string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				file, _, err := r.FormFile("file")
				if err != nil {
					t.Error(err)
					http.Error(w, "missing file", 400)
					return
				}
				defer file.Close()
				data, _ := io.ReadAll(file)
				gotAudio = string(data)
				if r.FormValue("response_format") != "json" {
					t.Error("not a JSON transcription")
				}
				_, _ = w.Write([]byte(`{"text":"Add Alex to CNAS"}`))
			}))
			defer srv.Close()
			rec := newRecorder()
			manager := NewManager(Options{Inventory: inventoryWith(models.Info{ID: "whisper", Kind: models.KindOpenAICompatible, Runtime: "declared", BaseURL: srv.URL, Allowed: true, Transcription: protocol, Attributes: models.Attributes{AudioIn: true, MaxConcurrent: 1}})})
			request := start("audio-turn", "whisper", KindTranscribe)
			request.Audio = &memqlv1.ModelCallAudio{Data: []byte("RIFF speech bytes"), MediaType: "audio/wav", SampleRateHz: 24000}
			manager.Start(context.Background(), rec, request)
			end := rec.wait(t)
			if end.ErrorCode != "" || rec.content() != "Add Alex to CNAS" || gotAudio != "RIFF speech bytes" {
				t.Fatalf("media lost: end=%v text=%q audio=%q", end, rec.content(), gotAudio)
			}
			want := "/audio/transcriptions"
			if protocol == "whisper-cpp" {
				want = "/inference"
			}
			if gotPath != want {
				t.Fatalf("path=%q", gotPath)
			}
		})
	}
}
func TestSpeechWireReturnsBinaryAndResolvesVoice(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte("RIFF output"))
	}))
	defer srv.Close()
	manager := NewManager(Options{Inventory: inventoryWith(models.Info{ID: "kokoro", Kind: models.KindOpenAICompatible, BaseURL: srv.URL, Allowed: true, Voices: map[string]string{"female": "af_heart", "male": "am_michael"}, Attributes: models.Attributes{AudioOut: true, MaxConcurrent: 1}})})
	rec := newRecorder()
	request := start("speech-turn", "kokoro", KindSpeak)
	request.Messages = []*memqlv1.ModelCallMessage{{Role: "user", Content: "Alex is now a member."}}
	request.Speech = &memqlv1.ModelCallSpeech{Voice: "male", Format: "wav"}
	manager.Start(context.Background(), rec, request)
	end := rec.wait(t)
	if end.ErrorCode != "" || string(end.GetAudio().GetData()) != "RIFF output" || end.GetAudio().GetMediaType() != "audio/wav" {
		t.Fatalf("speech lost: %v", end)
	}
	if body["voice"] != "am_michael" || body["input"] != "Alex is now a member." {
		t.Fatalf("request=%v", body)
	}
}
func TestToolCatalogueAndResultCrossWorkerWire(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte("data: {\"model\":\"local\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"askDiscover\",\"arguments\":\"{\\\"search\\\":\\\"groups\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	manager := NewManager(Options{Inventory: inventoryWith(models.Info{ID: "local", Kind: models.KindOpenAICompatible, BaseURL: srv.URL, Allowed: true, Attributes: models.Attributes{Tools: true, MaxConcurrent: 1}})})
	request := start("tools-turn", "local", KindChat)
	request.Tools = []*memqlv1.ModelCallTool{{Name: "askDiscover", ParametersJson: `{"type":"object","properties":{"search":{"type":"string"}}}`}}
	request.Messages = []*memqlv1.ModelCallMessage{{Role: "assistant", ToolCalls: []*memqlv1.ModelCallToolCall{{Id: "previous", Name: "askDiscover", ArgumentsJson: `{}`}}}, {Role: "tool", ToolCallId: "previous", Name: "askDiscover", Content: "allowed capabilities"}, {Role: "user", Content: "Find groups"}}
	rec := newRecorder()
	manager.Start(context.Background(), rec, request)
	end := rec.wait(t)
	if end.ErrorCode != "" || len(end.ToolCalls) != 1 || end.ToolCalls[0].Name != "askDiscover" || end.ToolCalls[0].ArgumentsJson != `{"search":"groups"}` {
		t.Fatalf("tool result lost: %v", end)
	}
	if len(body["tools"].([]any)) != 1 {
		t.Fatal("catalogue missing")
	}
	msgs := body["messages"].([]any)
	if msgs[1].(map[string]any)["tool_call_id"] != "previous" {
		t.Fatal("tool result correlation missing")
	}
}
