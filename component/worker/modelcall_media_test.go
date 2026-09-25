package worker

import (
	"context"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestModelMediaReachesTheWorkerStreamAndReturns(t *testing.T) {
	session, stream := newToolStreamTestSession()
	defer session.cancel()
	audio := &memqlv1.ModelCallAudio{Data: []byte{1, 2, 3, 4}, MediaType: "audio/wav", SampleRateHz: 24000}
	speech := &memqlv1.ModelCallSpeech{Voice: "male", Format: "wav", Speed: 1.1, SpeedSet: true}
	image := &memqlv1.ModelCallImage{Data: []byte{5, 6}, MediaType: "image/png"}
	handle, err := session.openModelCall(context.Background(), ModelCallRequest{RequestId: "media", Model: "local", Kind: "transcribe", Audio: audio, Speech: speech, Messages: []ModelCallMessage{{Role: "user", Content: "hello", Images: []*memqlv1.ModelCallImage{image}}}})
	if err != nil {
		t.Fatal(err)
	}
	start := stream.sent[0].GetModelCallStart()
	if !proto.Equal(start.Audio, audio) || !proto.Equal(start.Speech, speech) || !proto.Equal(start.Messages[0].Images[0], image) {
		t.Fatal("media lost before worker stream")
	}
	segments := []*memqlv1.ModelCallTranscriptSegment{{StartSeconds: 0.1, EndSeconds: 1, Text: "hello"}}
	session.handleModelCallEnd(&memqlv1.ModelCallEnd{RequestId: "media", FinishReason: ModelFinishStop, Content: "hello", Audio: audio, Images: []*memqlv1.ModelCallImage{image}, Segments: segments})
	out, err := handle.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(out.Audio, audio) || len(out.Segments) != 1 || len(out.Images) != 1 {
		t.Fatal("media lost after worker stream")
	}
}
