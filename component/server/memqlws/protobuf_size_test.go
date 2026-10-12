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
//
// The tests put the bridge in front of a server whose cap is 64 KiB, with the
// bridge measuring against the same: the shape is the same as at 32 MiB, and the
// requests are kilobytes instead of tens of megabytes (the 32 MiB version took
// 3.8 GB of memory under the race detector).

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"nhooyr.io/websocket"
)

// testServerCap is the server's message cap in the tests that try the measure.
const testServerCap = 64 << 10

func TestARequestOverTheServersCapAsProtocolBuffersIsRefusedByItself(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	svc := &stubMemqlService{}
	conn := dialBridge(t, ctx, startBridgeWith(t, Options{}, svc, bridgeSetup{serverCap: testServerCap}))

	// 8000 zeros: 16 KB of JSON, under the cap and the frame limit, and about 88 KB
	// as protocol buffers, over the cap.
	const numbers = 8000
	messageID, requestID := uuid.NewString(), uuid.NewString()
	text := `{"executeQuery":{"requestId":"` + requestID + `","query":"q","variables":{"n":[` +
		strings.Repeat("0,", numbers-1) + `0]}},"messageId":"` + messageID + `"}`
	require.Less(t, len(text), testServerCap, "the JSON must be under the cap for this to be the case under test")
	encoded := encodedSizeOf(t, text)
	require.Greater(t, encoded, testServerCap, "the encoding must be over the cap for this to be the case under test")
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(text)))

	frame := readFrame(t, ctx, conn)
	refusal, ok := frame["queryError"].(map[string]any)
	require.True(t, ok, "expected a refusal for a request the server would not take, got %v", frame)
	require.Equal(t, requestID, refusal["requestId"])
	require.Equal(t, messageID, frame["correlateTo"])
	detail := refusal["error"].(map[string]any)
	require.Equal(t, "ResourceExhausted", detail["code"])
	require.Regexp(t, regexp.MustCompile(`^This request is too large to send: it is \d+ KiB and one request can carry at most 64 KiB\. Send less at once and try again\.$`),
		detail["message"])
	metadata := detail["metadata"].(map[string]any)
	require.Equal(t, "REQUEST_TOO_LARGE", metadata["code"])
	require.Equal(t, strconv.Itoa(encoded), metadata["sizeBytes"], "the size is the one the server counts")
	require.Equal(t, strconv.Itoa(testServerCap), metadata["limitBytes"])
	require.Empty(t, receivedQueries(svc), "the server must never see a request the bridge refused")

	// The session carries on.
	next := sendQuery(t, ctx, conn, "small")
	frame = readFrame(t, ctx, conn)
	result, ok := frame["queryResult"].(map[string]any)
	require.True(t, ok, "the session did not survive: %v", frame)
	require.Equal(t, next, result["requestId"])
}

// The bridge gives an executeQuery that came without a request id one of its own,
// the message id, before it forwards it, so the server receives the request with
// that id in it: a tag, a length and the id more than was sent, 38 bytes for a
// uuid. A request is measured with the id in it. Measured as it was sent, one
// within those bytes of the cap passed the bridge and ended the session at the
// server's Recv.
func TestARequestIsMeasuredWithTheIdTheBridgeGivesIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	svc := &stubMemqlService{}
	conn := dialBridge(t, ctx, startBridgeWith(t, Options{}, svc, bridgeSetup{serverCap: testServerCap}))

	// Exactly at the cap as it was sent, with no request id: the id takes it over.
	messageID := uuid.NewString()
	query := queryForEncodedSize(t, messageID, "", testServerCap)
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(executeQueryWithoutRequestId(messageID, query))))
	frame := readFrame(t, ctx, conn)
	refusal, ok := frame["queryError"].(map[string]any)
	require.True(t, ok, "a request the bridge's id took over the cap was not refused: %v", frame)
	require.Equal(t, messageID, frame["correlateTo"])
	require.Equal(t, messageID, refusal["requestId"], "a request with no request id is named by its message id")
	metadata := refusal["error"].(map[string]any)["metadata"].(map[string]any)
	require.Equal(t, "REQUEST_TOO_LARGE", metadata["code"])
	withId := proto.Size(executeQueryMessage(messageID, messageID, query))
	require.Equal(t, testServerCap+2+len(messageID), withId)
	require.Equal(t, strconv.Itoa(withId), metadata["sizeBytes"], "the size is the request's with the id in it")
	require.Empty(t, receivedQueries(svc), "the server must never see a request the bridge refused")

	// Exactly at the cap with the id in it: taken whole, and answered by that id.
	messageID = uuid.NewString()
	query = queryForEncodedSize(t, messageID, messageID, testServerCap)
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(executeQueryWithoutRequestId(messageID, query))))
	frame = readFrame(t, ctx, conn)
	result, ok := frame["queryResult"].(map[string]any)
	require.True(t, ok, "a request exactly at the cap with its id was not taken: %v", frame)
	require.Equal(t, messageID, result["requestId"])
	require.Equal(t, []string{query}, receivedQueries(svc), "the server must receive the request whole")
}

// executeQueryWithoutRequestId is an executeQuery as a client writes it that
// leaves the request id to the bridge.
func executeQueryWithoutRequestId(messageID, query string) string {
	return `{"executeQuery":{"query":"` + query + `"},"messageId":"` + messageID + `"}`
}

func executeQueryMessage(messageID, requestID, query string) *memqlv1.MemqlClientMessage {
	return &memqlv1.MemqlClientMessage{
		MessageId: messageID,
		Payload: &memqlv1.MemqlClientMessage_ExecuteQuery{
			ExecuteQuery: &memqlv1.ExecuteQueryMsg{RequestId: requestID, Query: query},
		},
	}
}

// queryForEncodedSize is a query of q's that makes an executeQuery with these ids
// exactly size bytes as protocol buffers.
func queryForEncodedSize(t *testing.T, messageID, requestID string, size int) string {
	t.Helper()
	query := ""
	// A length prefix can grow by a byte as the query does, so the size is approached
	// and then corrected.
	for range 4 {
		got := proto.Size(executeQueryMessage(messageID, requestID, query))
		switch {
		case got == size:
			return query
		case got < size:
			query += strings.Repeat("q", size-got)
		default:
			query = query[:len(query)-(got-size)]
		}
	}
	t.Fatalf("no query of q's makes the request %d bytes", size)
	return ""
}

// encodedSizeOf is the size of a request's protocol buffers encoding, which is
// what the server measures.
func encodedSizeOf(t *testing.T, text string) int {
	t.Helper()
	msg := &memqlv1.MemqlClientMessage{}
	require.NoError(t, protojson.Unmarshal([]byte(text), msg))
	return proto.Size(msg)
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
