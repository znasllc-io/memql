//go:build agent

package worker

import (
	"context"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	engine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/common"
	"testing"
)

func TestFleetMediaAndToolResultsSurviveReplicaHop(t *testing.T) {
	for _, kind := range []string{engine.FleetKindTranscribe, engine.FleetKindSpeak, engine.FleetKindVision, engine.FleetKindImage, engine.FleetKindChat} {
		t.Run(kind, func(t *testing.T) {
			h := newModelHop(t, func(_ context.Context, req workerservice.ModelCallRequest, _ func(workerservice.ModelCallDelta)) workerservice.ModelCallOutcome {
				switch kind {
				case engine.FleetKindTranscribe:
					if string(req.Audio.GetData()) != "audio" || req.Audio.GetSampleRateHz() != 24000 {
						t.Error("lost transcription input")
					}
				case engine.FleetKindSpeak:
					if req.Speech.GetVoice() != "male" || req.Messages[0].Content != "Hello" {
						t.Error("lost speech input")
					}
				case engine.FleetKindVision:
					if len(req.Messages[0].Images) != 1 || string(req.Messages[0].Images[0].Data) != "image" {
						t.Error("lost vision input")
					}
				case engine.FleetKindImage:
					if req.Image.GetWidth() != 512 {
						t.Error("lost image settings")
					}
				}
				return workerservice.ModelCallOutcome{FinishReason: workerservice.ModelFinishStop, Content: "Hello", Audio: &memqlv1.ModelCallAudio{Data: []byte("spoken"), MediaType: "audio/wav", SampleRateHz: 24000}, Segments: []*memqlv1.ModelCallTranscriptSegment{{Text: "Hello", EndSeconds: 1}}, Images: []*memqlv1.ModelCallImage{{Data: []byte("generated"), MediaType: "image/png"}}, ToolCalls: []workerservice.ModelCallToolCall{{Id: "call", Name: "askDiscover", ArgumentsJSON: `{"search":"fleet"}`}}}
			}, func(c *Candidate, w *workerservice.Worker) {
				value := (ModelAttributes{ContextWindow: 8192, Tools: true, Vision: true, AudioIn: true, AudioOut: true, ImageGen: true}).String()
				c.Labels[workerservice.ModelLabel(hopModel)] = value
				w.Labels[workerservice.ModelLabel(hopModel)] = value
			})
			f := newFleetInference(t, h.store)
			f.selfNodeId, f.forward = nodeA, h.link.router
			req := engine.FleetCallRequest{ActingUserId: h.owner, ModelId: hopModel, Kind: kind, Messages: []common.ChatMessage{{Role: "user", Content: "Hello"}}, Audio: &engine.FleetAudio{Data: []byte("audio"), MediaType: "audio/wav", SampleRateHz: 24000}, Speech: &engine.FleetSpeechRequest{Voice: "male", Format: "wav"}, Images: []engine.FleetImage{{Data: []byte("image"), MediaType: "image/png"}}, Image: &engine.FleetImageRequest{Width: 512, Height: 512, Count: 1}}
			result, err := f.Call(authorityCtx(t, h.owner), req)
			if err != nil {
				t.Fatal(err)
			}
			if result.Audio == nil || string(result.Audio.Data) != "spoken" || len(result.Segments) != 1 || len(result.Images) != 1 || len(result.ToolCalls) != 1 || result.ToolCalls[0].Name != "askDiscover" {
				t.Fatalf("lost terminal payload: %+v", result)
			}
		})
	}
}
