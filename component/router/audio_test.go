package router

import (
	"context"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"testing"
)

type audioContextFleet struct{ contextFleet }

func (f *audioContextFleet) Call(_ context.Context, req memql.FleetCallRequest) (memql.FleetCallResult, error) {
	f.requests = append(f.requests, req)
	if req.ContextTokens != 0 {
		return memql.FleetCallResult{}, &memql.FleetUnavailable{ModelId: req.ModelId}
	}
	return memql.FleetCallResult{Content: "Hello", Audio: &memql.FleetAudio{Data: []byte{1, 2}, MediaType: "audio/wav"}}, nil
}
func TestAudioNeedsNoChatContextAtSelectionOrDispatch(t *testing.T) {
	for _, modality := range []airoute.Modality{airoute.ModalityTranscribe, airoute.ModalitySpeech} {
		t.Run(string(modality), func(t *testing.T) {
			model := sizedModel("audio", 82000000, 0)
			model.AudioIn = true
			model.AudioOut = true
			fleet := &audioContextFleet{contextFleet{models: []memql.FleetModel{model}}}
			providers := memql.NewProviderRegistryForTest()
			providers.SetFleetInference(fleet)
			r := New(providers, memql.NewPolicyRegistryForTest(map[string][]string{"p": {memql.FleetFastest}}), testRules(t, defaultRule("p")), nil, nil)
			resolved, err := r.ResolveFor(context.Background(), ResolveRequest{UserId: "alice", Level: airoute.LevelFast, Modality: modality, Needs: airoute.Needs{MinContextTokens: 8192}})
			if err != nil {
				t.Fatal(err)
			}
			if modality == airoute.ModalitySpeech {
				_, err = resolved.Client.(memql.SpeechAIProvider).Speak(context.Background(), memql.FleetSpeechRequest{Text: "Hello", Voice: "female", Format: "wav"})
			} else {
				_, err = resolved.Client.(memql.TranscriptionAIProvider).Transcribe(context.Background(), memql.FleetAudio{Data: []byte{1, 2}})
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(fleet.requests) != 1 {
				t.Fatalf("dispatches=%d", len(fleet.requests))
			}
		})
	}
}
