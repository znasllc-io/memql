package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	memqlgrpc "github.com/znasllc-io/memql/component/grpc"
	"github.com/znasllc-io/memql/component/node"
)

// Streaming transcription is MemQL OS's dictation, and it depends on two
// facts agreeing across two packages that cannot import each other.
//
// The BFF does not transcribe. It proxies the AiTranscribeStreamStart /
// Chunk / End trigger to one node type and consumes the transcript frames
// back (component/grpc/ai_transcribe_stream.go). That target is chosen in
// component/grpc; the node that has to HAVE an stt.StreamingProvider wired is
// chosen here in app/. Neither package can see the other's half.
//
// The failure when they disagree is silent and blames the browser: the mic
// opens, no transcript ever arrives, and nothing in the logs names a cause.
// It is the exact shape the epic that re-homed this from the voice node to the
// agent node had to avoid (memql#4988) -- component/grpc's own init() catches a
// change to the TARGET, and these two catch the other half, a node that stops
// wiring the provider.
//
// Source-level rather than behavioural because the wiring lives behind
// `//go:build agent`, and a test behind that tag runs in no default lane.

func agentTransportSource(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("transport_agent.go"))
	if err != nil {
		t.Fatalf("read transport_agent.go: %v", err)
	}
	return string(body)
}

// TestTheTranscriptionTargetNodeWiresAnSTTProvider pins the half that lives
// here: whichever node type component/grpc proxies transcription to must be
// the one whose transport installs the provider.
func TestTheTranscriptionTargetNodeWiresAnSTTProvider(t *testing.T) {
	target := memqlgrpc.TranscriptionNodeType()
	if target != node.NodeTypeAgent {
		t.Fatalf("streaming transcription now targets %q, but only the agent node's "+
			"transport wires an stt.StreamingProvider. Either wire one on %q's transport "+
			"or move the target back -- a target with no provider answers "+
			"\"streaming transcription is not configured\" to every dictation attempt.",
			target, target)
	}
	src := agentTransportSource(t)
	if !strings.Contains(src, "SetSTTProvider(") {
		t.Fatal("app/transport_agent.go no longer calls SetSTTProvider. The agent node is " +
			"where component/grpc proxies streaming transcription, so removing this makes " +
			"MemQL OS's Ask hold-to-talk fail with an empty transcript and no logged cause.")
	}
	if !strings.Contains(src, "a.selectSTTProvider()") && !strings.Contains(agentIntegrationsSource(t), "a.selectSTTProvider()") {
		t.Fatal("nothing on the agent build calls selectSTTProvider, so a.sttProvider is nil " +
			"and the SetSTTProvider call above installs nothing.")
	}
}

func agentIntegrationsSource(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("integrations_agent.go"))
	if err != nil {
		t.Fatalf("read integrations_agent.go: %v", err)
	}
	return string(body)
}
