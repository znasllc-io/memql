package memqlws

// A request is measured twice on its way to the server, and in different units.
//
// The bridge reads a request as JSON text and limits that (the frame limit). The
// server limits the protocol buffers encoding of the same request, and the two
// are not the same size. A list of small numbers is two bytes of JSON each ("0,")
// and eleven of protocol buffers (a Value holding a double is a tag, a length and
// eight bytes, inside a list entry that has its own tag and length), so a
// variables Struct of about six MiB of JSON is over the server's 32 MiB. The
// server does not refuse such a message: grpc-go fails the stream's Recv, the
// server's stream loop returns on it, and the whole session ends with every other
// request on it. So the bridge measures what the server will measure, before it
// forwards a request, and refuses an oversized one by itself.

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/grpclimits"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"nhooyr.io/websocket"
)

func TestARequestOverTheServersCapAsProtocolBuffersIsRefusedByItself(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	svc := &stubMemqlService{}
	conn := dialBridge(t, ctx, startBridge(t, Options{}, svc))

	// 3.3 million zeros: 6.6 MB of JSON, under the 32 MiB frame limit, and about 36
	// MB as protocol buffers, over the server's 32 MiB.
	const numbers = 3_300_000
	messageID, requestID := uuid.NewString(), uuid.NewString()
	text := `{"executeQuery":{"requestId":"` + requestID + `","query":"q","variables":{"n":[` +
		strings.Repeat("0,", numbers-1) + `0]}},"messageId":"` + messageID + `"}`
	require.Less(t, len(text), grpclimits.MaxMessageBytes, "the JSON must be under the frame limit for this to be the case under test")
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(text)))

	frame := readFrame(t, ctx, conn)
	refusal, ok := frame["queryError"].(map[string]any)
	require.True(t, ok, "expected a refusal for a request the server would not take, got keys of %v", len(frame))
	require.Equal(t, requestID, refusal["requestId"])
	require.Equal(t, messageID, frame["correlateTo"])
	detail := refusal["error"].(map[string]any)
	require.Equal(t, "ResourceExhausted", detail["code"])
	require.Regexp(t, regexp.MustCompile(`^This request is too large to send: it is \d+\.\d MiB and one request can carry at most 32 MiB\. Send less at once and try again\.$`),
		detail["message"])
	metadata := detail["metadata"].(map[string]any)
	require.Equal(t, "REQUEST_TOO_LARGE", metadata["code"])
	require.Equal(t, "33554432", metadata["limitBytes"])
	require.Empty(t, receivedQueries(svc), "the server must never see a request the bridge refused")

	// The session carries on.
	next := sendQuery(t, ctx, conn, "small")
	frame = readFrame(t, ctx, conn)
	result, ok := frame["queryResult"].(map[string]any)
	require.True(t, ok, "the session did not survive: %v", frame)
	require.Equal(t, next, result["requestId"])
}

// The two sizes differ by a large factor for a list of small numbers, and the
// bridge compares the one the server compares: the encoding, at its cap exactly (the
// server refuses a message strictly over it).
func TestWithinServerCapIsAboutTheEncodingTheServerMeasures(t *testing.T) {
	numbers := make([]any, 1000)
	for i := range numbers {
		numbers[i] = 0
	}
	variables, err := structpb.NewStruct(map[string]any{"n": numbers})
	require.NoError(t, err)
	msg := &memqlv1.MemqlClientMessage{
		MessageId: "m",
		Payload: &memqlv1.MemqlClientMessage_ExecuteQuery{
			ExecuteQuery: &memqlv1.ExecuteQueryMsg{RequestId: "r", Query: "q", Variables: variables},
		},
	}
	// The JSON text a client writes, compact: encoding/json is deterministic where
	// protojson's whitespace is not.
	text, err := json.Marshal(map[string]any{"executeQuery": map[string]any{"requestId": "r", "query": "q", "variables": map[string]any{"n": numbers}}, "messageId": "m"})
	require.NoError(t, err)
	encoded := proto.Size(msg)
	require.Greater(t, encoded, 4*len(text), "a list of small numbers is several times larger encoded than as JSON text (%d against %d)", encoded, len(text))

	size, ok := withinServerCap(msg, encoded)
	require.True(t, ok, "a message exactly at the cap is taken")
	require.Equal(t, encoded, size)
	_, ok = withinServerCap(msg, encoded-1)
	require.False(t, ok, "a message one byte over the cap is not")
}
