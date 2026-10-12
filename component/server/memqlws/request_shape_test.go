package memqlws

// What one request may cost the bridge before it is refused.
//
// The bridge decoded every request (protojson for JSON text, proto for binary)
// before anything looked at it, and decoding allocates far more than a request's
// bytes: 140 to 310 bytes for each value in it. A 32 MiB list of zeros took 2.4 GB
// to decode before it was refused for being too large for the server, so one
// request could take a node down. A request is now measured by its shape first,
// from its bytes, and one that holds too many values, or nests them too deeply, is
// refused by itself, as one over the frame limit is.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/grpclimits"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"nhooyr.io/websocket"
)

// zerosRequest is an executeQuery whose variables hold a list of zeros (at least
// one), with the ids where the browser SDK writes them: the request id first, the
// message id last. It holds zeros+7 values: the message, the payload, the two ids,
// the query, the variables and the list, and the zeros.
func zerosRequest(messageID, requestID string, zeros int) []byte {
	request, _ := zerosReader(messageID, requestID, zeros)
	text, _ := io.ReadAll(request)
	return text
}

// zerosReader reads as zerosRequest without the request being held, and says how
// long it is. The largest is 32 MiB, and a test of what refusing it costs should
// not have to hold it (under the race detector, holding it took most of a GB).
func zerosReader(messageID, requestID string, zeros int) (io.Reader, int) {
	head := `{"executeQuery":{"requestId":"` + requestID + `","query":"q","variables":{"n":[0`
	tail := `]}},"messageId":"` + messageID + `"}`
	rest := &repeatReader{text: strings.Repeat(",0", 4096), left: 2 * (zeros - 1)}
	return io.MultiReader(strings.NewReader(head), rest, strings.NewReader(tail)), len(head) + rest.left + len(tail)
}

// repeatReader reads left bytes of text over and over.
type repeatReader struct {
	text string
	left int
	off  int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	p = p[:min(len(p), r.left)]
	for n := 0; n < len(p); {
		c := copy(p[n:], r.text[r.off:])
		n += c
		r.off = (r.off + c) % len(r.text)
	}
	r.left -= len(p)
	return len(p), nil
}

// nestedRequest is an executeQuery whose variables hold lists nested inside each
// other, deepest levels deep in all: the message, the payload and the variables
// are three of them.
func nestedRequest(messageID, requestID string, deepest int) []byte {
	lists := deepest - 3
	return []byte(`{"executeQuery":{"requestId":"` + requestID + `","query":"q","variables":{"n":` +
		strings.Repeat("[", lists) + strings.Repeat("]", lists) + `}},"messageId":"` + messageID + `"}`)
}

// requireRefused reads the next frame and says it is the refusal of the request
// sent with these ids, and returns its detail.
func requireRefused(t *testing.T, ctx context.Context, conn *websocket.Conn, messageID, requestID string) map[string]any {
	t.Helper()
	frame := readFrame(t, ctx, conn)
	refusal, ok := frame["queryError"].(map[string]any)
	require.True(t, ok, "expected the request to be refused, got %v", frame)
	require.Equal(t, requestID, refusal["requestId"], "the refusal must name the request it refuses")
	require.Equal(t, messageID, frame["correlateTo"], "the refusal must carry the id the sender waits for")
	detail := refusal["error"].(map[string]any)
	require.Equal(t, "ResourceExhausted", detail["code"])
	return detail
}

// requireAnswered reads the next frame and says it is the answer to requestID.
func requireAnswered(t *testing.T, ctx context.Context, conn *websocket.Conn, requestID string) {
	t.Helper()
	frame := readFrame(t, ctx, conn)
	result, ok := frame["queryResult"].(map[string]any)
	require.True(t, ok, "expected an answer to %s, got %v", requestID, frame)
	require.Equal(t, requestID, result["requestId"])
}

// requireTheSessionCarriesOn sends a small request and says it is answered.
func requireTheSessionCarriesOn(t *testing.T, ctx context.Context, conn *websocket.Conn) {
	t.Helper()
	requireAnswered(t, ctx, conn, sendQuery(t, ctx, conn, "small"))
}

// The reviewer's request: 32 MiB of zeros, inside the frame limit. Refusing it
// allocated 2.7 GB, nearly all of it decoding. Now it is refused while it arrives,
// before it is decoded, and the whole of what refusing it costs is measured here:
// every allocation in the process while it is written, read and refused, the test's
// own client included.
func TestRefusingA32MiBListOfZerosAllocatesLittle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	svc := &stubMemqlService{}
	conn := dialBridge(t, ctx, startBridge(t, Options{}, svc))
	// What the session sets up once is not part of what a request costs.
	requireTheSessionCarriesOn(t, ctx, conn)

	messageID, requestID := uuid.NewString(), uuid.NewString()
	_, envelope := zerosReader(messageID, requestID, 1)
	zeros := (grpclimits.MaxMessageBytes-envelope)/2 + 1
	request, size := zerosReader(messageID, requestID, zeros)
	require.LessOrEqual(t, size, grpclimits.MaxMessageBytes, "within the frame limit: only its shape is over")
	require.Greater(t, size, grpclimits.MaxMessageBytes-2, "as large as the frame limit allows")

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	writer, err := conn.Writer(ctx, websocket.MessageText)
	require.NoError(t, err)
	written, err := io.Copy(writer, request)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	require.EqualValues(t, size, written)
	detail := requireRefused(t, ctx, conn, messageID, requestID)
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("refusing a request of %d bytes holding %d values allocated %d bytes (%.3f of its size)",
		size, zeros+7, allocated, float64(allocated)/float64(size))
	require.Less(t, allocated, uint64(64<<20), "refusing one request allocated %d bytes", allocated)

	values := zeros + 7
	require.Equal(t, fmt.Sprintf("This request is too large to send: it holds %s values and one request can hold at most 100,000. Send less at once and try again.",
		groupedDigits(int64(values))), detail["message"])
	require.Equal(t, map[string]any{"code": "REQUEST_TOO_LARGE", "values": strconv.Itoa(values), "limitValues": "100000"}, detail["metadata"])
	require.Equal(t, []string{"small"}, receivedQueries(svc), "the server must never see a request the bridge refused")
	requireTheSessionCarriesOn(t, ctx, conn)
}

// The value bound, at its edge: a request holding exactly as many values as one
// may is taken, and one holding a value more is refused by itself.
func TestARequestHoldingTooManyValuesIsRefusedByItself(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc := &stubMemqlService{}
	conn := dialBridge(t, ctx, startBridge(t, Options{}, svc))

	messageID, requestID := uuid.NewString(), uuid.NewString()
	require.NoError(t, conn.Write(ctx, websocket.MessageText, zerosRequest(messageID, requestID, maxRequestValues-7)))
	requireAnswered(t, ctx, conn, requestID)

	messageID, requestID = uuid.NewString(), uuid.NewString()
	require.NoError(t, conn.Write(ctx, websocket.MessageText, zerosRequest(messageID, requestID, maxRequestValues-6)))
	detail := requireRefused(t, ctx, conn, messageID, requestID)
	require.Equal(t, "This request is too large to send: it holds 100,001 values and one request can hold at most 100,000. Send less at once and try again.",
		detail["message"])
	require.Equal(t, map[string]any{"code": "REQUEST_TOO_LARGE", "values": "100001", "limitValues": "100000"}, detail["metadata"])
	require.Equal(t, []string{"q"}, receivedQueries(svc), "the server must never see a request the bridge refused")
	requireTheSessionCarriesOn(t, ctx, conn)
}

// Depth is bounded too, and not only for what it costs. protojson counts one level
// for each list or object nested in variables and takes about ten thousand; the
// server decodes the same request as protocol buffers, where each of those levels
// is two messages (a Value and its list or Struct), and takes five thousand. A
// request between the two passed the bridge and ended the session at the server's
// Recv. One more than a thousand levels deep is refused by itself.
func TestARequestNestedTooDeeplyIsRefusedByItself(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc := &stubMemqlService{}
	conn := dialBridge(t, ctx, startBridge(t, Options{}, svc))

	messageID, requestID := uuid.NewString(), uuid.NewString()
	require.NoError(t, conn.Write(ctx, websocket.MessageText, nestedRequest(messageID, requestID, maxRequestDepth)))
	requireAnswered(t, ctx, conn, requestID)

	for _, deepest := range []int{maxRequestDepth + 1, 6000} {
		messageID, requestID = uuid.NewString(), uuid.NewString()
		require.NoError(t, conn.Write(ctx, websocket.MessageText, nestedRequest(messageID, requestID, deepest)))
		detail := requireRefused(t, ctx, conn, messageID, requestID)
		require.Equal(t, "This request is nested too deeply to send: one request can go at most 1,000 levels deep. Send it with less nesting and try again.",
			detail["message"])
		require.Equal(t, map[string]any{"code": "REQUEST_TOO_DEEP", "limitDepth": "1000"}, detail["metadata"])
		requireTheSessionCarriesOn(t, ctx, conn)
	}
	require.Equal(t, []string{"q", "small", "small"}, receivedQueries(svc), "the server must never see a request the bridge refused")
}

// A request sent as protocol buffers decodes the same way, about a hundred bytes
// for each value, and is measured by its shape before it is decoded too.
func TestABinaryRequestIsMeasuredByItsShapeToo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc := &stubMemqlService{}
	conn := dialBridge(t, ctx, startBridge(t, Options{}, svc))
	send := func(msg *memqlv1.MemqlClientMessage) {
		t.Helper()
		raw, err := proto.Marshal(msg)
		require.NoError(t, err)
		require.NoError(t, conn.Write(ctx, websocket.MessageBinary, raw))
	}
	withVariable := func(id string, v *structpb.Value) *memqlv1.MemqlClientMessage {
		msg := executeQueryMessage(id, id, "q")
		msg.GetExecuteQuery().Variables = &structpb.Struct{Fields: map[string]*structpb.Value{"n": v}}
		return msg
	}

	// Within its shape: taken.
	id := uuid.NewString()
	send(executeQueryMessage(id, id, "q"))
	requireAnswered(t, ctx, conn, id)

	// A list of empty values, two bytes each: the message, the payload, the two ids,
	// the query, the variables, the entry for "n", its key and its value, and the list
	// are ten values before the list's own.
	empties := make([]*structpb.Value, maxRequestValues)
	for i := range empties {
		empties[i] = &structpb.Value{}
	}
	id = uuid.NewString()
	send(withVariable(id, structpb.NewListValue(&structpb.ListValue{Values: empties})))
	detail := requireRefused(t, ctx, conn, id, id)
	require.Equal(t, map[string]any{"code": "REQUEST_TOO_LARGE", "values": strconv.Itoa(maxRequestValues + 10), "limitValues": "100000"}, detail["metadata"])
	requireTheSessionCarriesOn(t, ctx, conn)

	// Lists inside lists: every message inside another is a level.
	nested := structpb.NewListValue(&structpb.ListValue{})
	for i := 0; i < maxRequestDepth; i++ {
		nested = structpb.NewListValue(&structpb.ListValue{Values: []*structpb.Value{nested}})
	}
	id = uuid.NewString()
	send(withVariable(id, nested))
	detail = requireRefused(t, ctx, conn, id, id)
	require.Equal(t, map[string]any{"code": "REQUEST_TOO_DEEP", "limitDepth": "1000"}, detail["metadata"])
	requireTheSessionCarriesOn(t, ctx, conn)
	require.Equal(t, []string{"q", "small", "small"}, receivedQueries(svc), "the server must never see a request the bridge refused")
}

// The bounds are far above what clients send. A template save is the largest
// request there is, and the SDK writes a call with its arguments in it, the
// template's whole document included, as the query text: one JSON string however
// large the document, its quotes and backslashes escaped. A request of 3 MiB of it
// is taken whole, written that way and with the document in variables as one
// string.
func TestALargeTemplateSaveIsStillTakenWhole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc := &stubMemqlService{}
	conn := dialBridge(t, ctx, startBridge(t, Options{}, svc))

	document := templateDocument(3 << 20)
	documentJSON, err := json.Marshal(document)
	require.NoError(t, err)
	call := func(content string) string {
		return `builtin campaignSaveTemplate(accountId: "acct-1", content: ` + content + `, expectedRevision: "", name: "Welcome")`
	}
	for _, c := range []struct {
		name      string
		query     string
		variables map[string]any
	}{
		{"as the SDK writes it, the call with its document as the query text", call(memqlValueText(document)), nil},
		{"with the document in variables as one string", call("$content"), map[string]any{"content": string(documentJSON)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			id := uuid.NewString()
			executeQuery := map[string]any{"requestId": id, "query": c.query}
			if c.variables != nil {
				executeQuery["variables"] = c.variables
			}
			text, err := json.Marshal(map[string]any{"executeQuery": executeQuery, "messageId": id})
			require.NoError(t, err)
			require.Greater(t, len(text), 3<<20)
			require.Contains(t, string(text), `\\\"`, "the document's own escapes are escaped again")

			require.NoError(t, conn.Write(ctx, websocket.MessageText, text))
			requireAnswered(t, ctx, conn, id)
			svc.mu.Lock()
			received := svc.received[len(svc.received)-1].GetExecuteQuery()
			svc.mu.Unlock()
			require.Equal(t, c.query, received.GetQuery(), "the server must receive the request whole")
			if c.variables != nil {
				require.Equal(t, string(documentJSON), received.GetVariables().GetFields()["content"].GetStringValue())
			}
		})
	}
}

// templateDocument is an email document of about size bytes, of the kind a
// template save carries: blocks of text with quotes, backslashes, markup and
// letters from outside ASCII.
func templateDocument(size int) map[string]any {
	text := `He said "welcome" \ and <b>meant</b> it — café ☕, 50% off & "more"` + "\n"
	var blocks []any
	for n := 0; n < size; n += 20 * len(text) {
		blocks = append(blocks, map[string]any{"type": "text", "align": "left", "text": strings.Repeat(text, 20)})
	}
	return map[string]any{
		"subject":   `Welcome, "friend"`,
		"preheader": `It's here \o/`,
		"layout":    "simple",
		"theme":     map[string]any{"accent": "#0a7", "font": "Inter"},
		"blocks":    blocks,
	}
}

// memqlValueText writes a value as the browser SDK does when it puts an argument
// into a call (renderMemQLValue in sdk/ts/src/client/memqlValue.ts): strings as
// JSON writes them, objects with their keys sorted, lists and objects with ", "
// between their entries.
func memqlValueText(v any) string {
	switch v := v.(type) {
	case nil:
		return "nil"
	case string:
		var b bytes.Buffer
		encoder := json.NewEncoder(&b)
		encoder.SetEscapeHTML(false) // as JSON.stringify
		_ = encoder.Encode(v)
		return strings.TrimSuffix(b.String(), "\n")
	case bool:
		return strconv.FormatBool(v)
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = memqlValueText(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + ": " + memqlValueText(v[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return fmt.Sprint(v)
	}
}

// shapeOf measures text whole, and one byte at a time: the two must agree, since a
// request arrives in pieces cut anywhere.
func shapeOf(t *testing.T, text string) requestShape {
	t.Helper()
	var whole, bytewise jsonShape
	whole.scan([]byte(text))
	whole.finish()
	for i := 0; i < len(text); i++ {
		bytewise.scan([]byte{text[i]})
	}
	bytewise.finish()
	require.Equal(t, whole.requestShape, bytewise.requestShape, "measured in pieces: %s", text)
	return whole.requestShape
}

func TestJSONShapeCountsValuesAndHowDeeplyTheyNest(t *testing.T) {
	for _, c := range []struct {
		text    string
		values  int64
		deepest int
	}{
		{`{}`, 1, 1},
		{`[]`, 1, 1},
		{`"x"`, 1, 0},
		{`0`, 1, 0},
		{`{"a":1}`, 2, 1},
		{`{"a":"b"}`, 2, 1},
		{`[1,2,3]`, 4, 1},
		{`[true,false,null]`, 4, 1},
		{` { "a" : [ -1.5e+10 , "x" ] , "b" : { } } `, 5, 2},
		{"{\n\t\"a\" \r\n: 1}", 2, 1},
		{`{"a":{"b":[1,{"c":"d"}]}}`, 6, 4},
		{`[[],[[]],[]]`, 5, 3},
		// Closers with nothing open do not hide what opens after them.
		{`]]]}}[[`, 2, 2},
		// Structure inside a string is text.
		{`["{[,:]}"]`, 2, 1},
		{`{"{":"}"}`, 2, 1},
	} {
		require.Equal(t, requestShape{values: c.values, deepest: c.deepest}, shapeOf(t, c.text), "%s", c.text)
	}
}

// A string ends at the first quote no backslash escapes. A measure that ended it at
// an escaped quote would count text as values; one that let a backslash escape the
// quote after an escaped backslash would read on into what follows as if it were
// text, and not count values that protojson then decodes.
func TestJSONShapeIsNotFooledByEscapes(t *testing.T) {
	for _, c := range []struct {
		text   string
		values int64
	}{
		{`["a\"b",0]`, 3},
		{`["\\",0]`, 3},
		{`["\\\"",0]`, 3},
		{`["\\\\",0]`, 3},
		{`["\"\\\"\\",0]`, 3},
		{`["\u0022",0]`, 3},
		{`{"k\"ey":[0,0]}`, 4},
		// A list in a string is text...
		{`["\"],[0,0,0,0]"]`, 2},
		// ...and a list after a string that ends in an escaped backslash is not.
		{`["\\"],[0,0,0,0]`, 7},
	} {
		require.Equal(t, c.values, shapeOf(t, c.text).values, "%s", c.text)
	}
}

// The measure of protocol buffers counts as JSON's does: the message and every
// field in it and in every message inside it, and every number in a packed list.
func TestProtoShapeCountsFieldsAndHowDeeplyMessagesNest(t *testing.T) {
	measure := func(msg *memqlv1.MemqlClientMessage) requestShape {
		t.Helper()
		raw, err := proto.Marshal(msg)
		require.NoError(t, err)
		return measureProto(raw)
	}

	// The message, its id and payload, the payload's id, query and variables, the
	// entry for "n", its key and value, and the value's list: ten values; then three
	// entries in the list, of one number each, two values apiece. Levels: the
	// message, the payload, the variables (a map entry is not a level), the value of
	// "n", its list, and the values in it.
	numbers, err := structpb.NewList([]any{1, 2, 3})
	require.NoError(t, err)
	msg := executeQueryMessage("m", "r", "q")
	msg.GetExecuteQuery().Variables = &structpb.Struct{Fields: map[string]*structpb.Value{"n": structpb.NewListValue(numbers)}}
	require.Equal(t, requestShape{values: 10 + 6, deepest: 6}, measure(msg))

	// A packed list: the message, the payload, the list and its three numbers.
	subscribe := &memqlv1.MemqlClientMessage{Payload: &memqlv1.MemqlClientMessage_Subscribe{Subscribe: &memqlv1.SubscribeMsg{
		Actions: []memqlv1.GraphNodeAction{
			memqlv1.GraphNodeAction_GRAPH_NODE_ACTION_CREATED,
			memqlv1.GraphNodeAction_GRAPH_NODE_ACTION_UPDATED,
			memqlv1.GraphNodeAction_GRAPH_NODE_ACTION_DELETED,
		},
	}}}
	require.Equal(t, requestShape{values: 6, deepest: 2}, measure(subscribe))

	// A field this engine does not know is a value too.
	unknown := protowire.AppendVarint(protowire.AppendTag(nil, 999, protowire.VarintType), 1)
	require.Equal(t, requestShape{values: 2, deepest: 1}, measureProto(unknown))

	// What is not protocol buffers stops the measure, and nothing breaks.
	require.Equal(t, requestShape{values: 1, deepest: 1}, measureProto([]byte{0xff, 0xff, 0xff}))
	require.Equal(t, requestShape{values: 2, deepest: 1}, measureProto([]byte{0x0a, 0xff}))

	// It goes one level past the depth bound and no deeper, however deep the
	// request goes, so its own recursion is bounded too.
	nested := structpb.NewListValue(&structpb.ListValue{})
	for i := 0; i < 3*maxRequestDepth; i++ {
		nested = structpb.NewListValue(&structpb.ListValue{Values: []*structpb.Value{nested}})
	}
	deep := executeQueryMessage("m", "r", "q")
	deep.GetExecuteQuery().Variables = &structpb.Struct{Fields: map[string]*structpb.Value{"n": nested}}
	shape := measure(deep)
	require.Equal(t, maxRequestDepth+1, shape.deepest)
	over, ok := shape.over()
	require.True(t, ok)
	require.Equal(t, overLimit{unit: inLevels, size: maxRequestDepth + 1, limit: maxRequestDepth}, over)
}

// readRequest measures JSON as it reads it. A request over a bound of its shape is
// read to its end and all of it measured, and only its start and end are kept;
// within its bounds it comes back whole.
func TestReadRequestKeepsOnlyTheEndsOfARequestOverItsShape(t *testing.T) {
	text := zerosRequest("m-1", "r-1", 300_000)
	var shape jsonShape
	data, over, err := readRequest(bytes.NewReader(text), int64(len(text)), &shape)
	require.NoError(t, err)
	require.Nil(t, data)
	require.NotNil(t, over)
	require.EqualValues(t, len(text), over.size)
	require.Equal(t, requestShape{values: 300_007, deepest: 4}, over.shape)
	require.Equal(t, text[:idPrefixBytes], over.prefix)
	require.Equal(t, text[len(text)-idTailBytes:], over.tail)
	require.Equal(t, overLimit{unit: inValues, size: 300_007, limit: maxRequestValues}, over.excess(int64(len(text))))
	messageID, requestID := idsOfAnOversizedRequest(websocket.MessageText, over.prefix, over.tail)
	require.Equal(t, "m-1", messageID)
	require.Equal(t, "r-1", requestID)

	// Over the byte limit as well, it is refused for its bytes.
	shape = jsonShape{}
	_, over, err = readRequest(bytes.NewReader(text), int64(len(text))-1, &shape)
	require.NoError(t, err)
	require.Equal(t, overLimit{unit: inBytes, size: int64(len(text)), limit: int64(len(text)) - 1}, over.excess(int64(len(text))-1))

	// Over the byte limit before it is over its shape, which it goes over only while
	// the rest of it is read and discarded: the rest is still read to its end, which
	// the next message on the connection needs, and measured whole.
	shape = jsonShape{}
	reader := bytes.NewReader(text)
	_, over, err = readRequest(reader, 100<<10, &shape)
	require.NoError(t, err)
	require.Zero(t, reader.Len(), "the request was not read to its end")
	require.EqualValues(t, len(text), over.size)
	require.Equal(t, requestShape{values: 300_007, deepest: 4}, over.shape)

	// Within its bounds it comes back whole, measured.
	small := zerosRequest("m-2", "r-2", 10)
	shape = jsonShape{}
	data, over, err = readRequest(bytes.NewReader(small), int64(len(small)), &shape)
	require.NoError(t, err)
	require.Nil(t, over)
	require.Equal(t, small, data)
	require.Equal(t, requestShape{values: 17, deepest: 4}, shape.requestShape)
}

// What is kept of a request over its shape stops where it went over: reading the
// reviewer's 32 MiB list of zeros holds a few hundred KiB of it, where reading it
// whole took four times its size.
func TestReadingARequestOverItsShapeHoldsLittleOfIt(t *testing.T) {
	request, size := zerosReader("m", "r", 16<<20)
	var shape jsonShape
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, over, err := readRequest(request, 64<<20, &shape)
	runtime.ReadMemStats(&after)
	require.NoError(t, err)
	require.NotNil(t, over)
	require.EqualValues(t, size, over.size)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("reading a request of %d bytes over its shape allocated %d bytes", size, allocated)
	require.Less(t, allocated, uint64(2<<20))
}
