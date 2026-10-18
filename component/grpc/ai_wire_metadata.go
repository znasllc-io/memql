package memql

import (
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	engine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// Stream metadata and query snapshots must name the same public identities.
// Keep canonical values on the mesh and in the originating journal; normalize
// a copy only when this session sends to the client, including after a relay.
func (s *streamSession) clientAIMetadata(msg *memqlv1.MemqlServerMessage) *memqlv1.MemqlServerMessage {
	if _, internal := s.stream.(*forwardedStream); internal {
		return msg
	}
	metadata := msg.GetAiChunk().GetMetadata()
	if metadata == nil {
		return msg
	}
	copy := metadata.AsMap()
	engine.BareifyEventPayload(copy)
	bare, err := structpb.NewStruct(copy)
	if err != nil {
		return msg // AsMap contains only protobuf JSON values.
	}
	out := proto.Clone(msg).(*memqlv1.MemqlServerMessage)
	out.GetAiChunk().Chunk = &memqlv1.AiStreamChunk_Metadata{Metadata: bare}
	return out
}
