package memqlws

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"nhooyr.io/websocket"
)

// oversized.go -- a request from a browser that is larger than the bridge takes.
//
// The WebSocket library closes the connection when a message is over its read
// limit, and the session ends with it: every other request on the socket, and
// every live feed riding it, goes with the one that was too large. A request that
// is too large is the sender's to fix, and it is one request. So the library is
// told a limit twice the bridge's own, the bridge reads each message itself up to
// its limit, and what is past it is counted and thrown away: the request is
// refused with a sentence, by the id it came with, and the session carries on. A
// message past twice the limit is still the end of the connection, because a
// client that sends without end is not one request that failed.

// readLimit is the limit the WebSocket library enforces: twice the bridge's own,
// so that a request over the limit can be read to its end and refused.
func readLimit(limit int64) int64 {
	if limit > math.MaxInt64/2 {
		return math.MaxInt64
	}
	return 2 * limit
}

// oversizedRequest is a request that was over the limit: the start and the end of
// it, which is all that is kept, and how large it was.
type oversizedRequest struct {
	prefix []byte
	tail   []byte
	size   int64
}

// readRequest reads one message from r. A message within limit comes back whole.
// One over it comes back as an oversizedRequest: its first idPrefixBytes, its last
// idTailBytes, and the number of bytes in all, the rest read and discarded. An
// error is a connection that failed, or one that went past the library's own limit
// and was closed.
func readRequest(r io.Reader, limit int64) ([]byte, *oversizedRequest, error) {
	var buf bytes.Buffer
	n, err := buf.ReadFrom(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if n <= limit {
		return buf.Bytes(), nil, nil
	}
	prefix := append([]byte(nil), buf.Bytes()[:min(buf.Len(), idPrefixBytes)]...)
	tail := &tailKeeper{max: idTailBytes}
	_, _ = tail.Write(buf.Bytes())
	buf = bytes.Buffer{}
	rest, err := io.Copy(tail, r)
	if err != nil {
		return nil, nil, err
	}
	return nil, &oversizedRequest{prefix: prefix, tail: tail.b, size: n + rest}, nil
}

// tailKeeper keeps the last max bytes written to it.
type tailKeeper struct {
	max int
	b   []byte
}

func (t *tailKeeper) Write(p []byte) (int, error) {
	n := len(p)
	if n >= t.max {
		t.b = append(t.b[:0], p[n-t.max:]...)
		return n, nil
	}
	if over := len(t.b) + n - t.max; over > 0 {
		copy(t.b, t.b[over:])
		t.b = t.b[:len(t.b)-over]
	}
	t.b = append(t.b, p...)
	return n, nil
}

// refuseOversizedRequest answers a request that was over the limit with an error
// for that request alone.
func (s *session) refuseOversizedRequest(msgType websocket.MessageType, over *oversizedRequest) {
	messageID, requestID := idsOfAnOversizedRequest(msgType, over.prefix, over.tail)
	s.refuseRequest(messageID, requestID, over.size, s.maxBytes)
}

// withinServerCap says whether the server will take msg. The server limits the
// protocol buffers encoding of a request, which is not the size of the JSON text
// the frame limit measured: a list of small numbers is two bytes of JSON each and
// eleven of protocol buffers. grpc-go refuses a message over its limit by failing
// the stream's Recv, and the server's stream loop returns on that, so the whole
// session would end. The request is measured here the way the server will.
func withinServerCap(msg *memqlv1.MemqlClientMessage, limit int) (size int, ok bool) {
	size = proto.Size(msg)
	return size, size <= limit
}

// refuseRequest answers a request that cannot be forwarded with an error for that
// request alone, by the ids it came with (requestID, or the message id when there
// is none), and the sentence for a request that is too large.
func (s *session) refuseRequest(messageID, requestID string, size, limit int64) {
	named := requestID
	if named == "" {
		named = messageID
	}
	s.logDebug("memql ws request refused: too large", "request_id", named, "bytes", size, "limit", limit)
	s.queueLocalErrorItem(&queuedLocalError{
		requestId: named,
		correlate: messageID,
		code:      codes.ResourceExhausted,
		message:   requestTooLargeSentence(size, limit),
		metadata: map[string]string{
			"code":       "REQUEST_TOO_LARGE",
			"sizeBytes":  strconv.FormatInt(size, 10),
			"limitBytes": strconv.FormatInt(limit, 10),
		},
	})
}

// requestTooLargeSentence is what the person whose request it was is told.
func requestTooLargeSentence(size, limit int64) string {
	return fmt.Sprintf("This request is too large to send: it is %s and one request can carry at most %s. Send less at once and try again.",
		describeBytes(size), describeBytes(limit))
}

// describeBytes writes a size for a person: whole MiB or KiB as such, anything else
// rounded UP, so that a request one byte over the limit never reads as the limit.
func describeBytes(n int64) string {
	const kib, mib = 1 << 10, 1 << 20
	switch {
	case n >= mib && n%mib == 0:
		return strconv.FormatInt(n/mib, 10) + " MiB"
	case n >= mib:
		return strconv.FormatFloat(math.Ceil(float64(n)/mib*10)/10, 'f', 1, 64) + " MiB"
	default:
		return strconv.FormatInt((n+kib-1)/kib, 10) + " KiB"
	}
}

// idsOfAnOversizedRequest finds the ids of a request in the start and the end of
// it: its message id, and the id of the request inside it, so that the refusal
// names the request it refuses. Clients differ in where they write them: the
// browser SDK writes the request id first and adds the message id last, after
// the query; a client that writes its keys in order puts both after it. Either may
// still be missing, when it is in the part that was not kept.
func idsOfAnOversizedRequest(msgType websocket.MessageType, prefix, tail []byte) (messageID, requestID string) {
	if msgType == websocket.MessageBinary {
		return idsOfProtoPrefix(prefix)
	}
	messageID, requestID = idsOfJSONPrefix(prefix)
	if messageID == "" {
		messageID = lastStringValue(tail, messageIDKey)
	}
	if requestID == "" {
		requestID = lastStringValue(tail, requestIDKey)
	}
	return messageID, requestID
}

var (
	messageIDKey = regexp.MustCompile(`"messageId"\s*:\s*"([^"\\]{1,200})"`)
	requestIDKey = regexp.MustCompile(`"requestId"\s*:\s*"([^"\\]{1,200})"`)
)

// lastStringValue is the value of the last occurrence of a key in the end of a JSON
// message, which is cut where it is cut and so cannot be read as JSON. The ids are
// plain strings (uuids), which is all it looks for.
func lastStringValue(tail []byte, key *regexp.Regexp) string {
	matches := key.FindAllSubmatch(tail, -1)
	if len(matches) == 0 {
		return ""
	}
	return string(matches[len(matches)-1][1])
}

// idsOfJSONPrefix reads the start of a JSON message, as far as it is whole:
// "messageId" in the object itself, and "requestId" in the object that is the
// message's payload.
func idsOfJSONPrefix(prefix []byte) (messageID, requestID string) {
	type container struct {
		object    bool
		expectKey bool
		key       string
	}
	var stack []container
	decoder := json.NewDecoder(bytes.NewReader(prefix))
	for {
		token, err := decoder.Token()
		if err != nil {
			// The end of what was kept, or not JSON: what was found stands.
			return messageID, requestID
		}
		switch t := token.(type) {
		case json.Delim:
			if t == '{' || t == '[' {
				stack = append(stack, container{object: t == '{', expectKey: t == '{'})
				continue
			}
			stack = stack[:len(stack)-1]
			if n := len(stack); n > 0 && stack[n-1].object {
				stack[n-1].expectKey = true
			}
		default:
			n := len(stack)
			if n == 0 || !stack[n-1].object {
				continue
			}
			top := &stack[n-1]
			if top.expectKey {
				top.key, _ = token.(string)
				top.expectKey = false
				continue
			}
			if value, isString := token.(string); isString {
				switch {
				case n == 1 && top.key == "messageId" && messageID == "":
					messageID = value
				case n == 2 && top.key == "requestId" && requestID == "":
					requestID = value
				}
			}
			top.expectKey = true
		}
	}
}

// The field numbers of MemqlClientMessage that matter here: the payload is one of
// the fields from 10 on, and provenance (100) comes after them. correlate_to and
// metadata, which come before the payload, are not messages with a request id.
const (
	payloadFieldsFrom = 10
	provenanceField   = 100
)

// idsOfProtoPrefix reads the start of a protocol buffers message the same way:
// the message id is the first field of the message, and the request id the first
// field of the payload, which is what an ExecuteQueryMsg writes first.
func idsOfProtoPrefix(prefix []byte) (messageID, requestID string) {
	for b := prefix; len(b) > 0; {
		number, kind, n := protowire.ConsumeTag(b)
		if n < 0 {
			return
		}
		b = b[n:]
		if kind != protowire.BytesType {
			n = protowire.ConsumeFieldValue(number, kind, b)
			if n < 0 {
				return
			}
			b = b[n:]
			continue
		}
		length, n := protowire.ConsumeVarint(b)
		if n < 0 {
			return
		}
		b = b[n:]
		inner := b
		complete := uint64(len(inner)) >= length
		if complete {
			inner = inner[:length]
		}
		switch {
		case number == 1 && messageID == "":
			if !complete {
				return
			}
			messageID = string(inner)
		case number >= payloadFieldsFrom && number < provenanceField && requestID == "":
			requestID = firstStringField(inner)
		}
		if !complete {
			return
		}
		b = b[length:]
	}
	return
}

// firstStringField is the value of the first field of a message when that field is
// field 1 and is a whole string, or "".
func firstStringField(message []byte) string {
	number, kind, n := protowire.ConsumeTag(message)
	if n < 0 || number != 1 || kind != protowire.BytesType {
		return ""
	}
	value, n := protowire.ConsumeBytes(message[n:])
	if n < 0 {
		return ""
	}
	return string(value)
}
