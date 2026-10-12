package memqlws

// request_shape.go -- what a request holds, measured from its bytes before it is
// decoded.
//
// The bridge decodes a request from a browser (protojson for JSON text, proto for
// binary) before anything else can look at it, and decoding allocates far more
// than the request's bytes: 100 to 310 bytes for each value it holds. A 32 MiB list
// of zeros took 2.7 GB to decode, and was refused only after that, for being too
// large for the server; one request could take a node down. What decoding costs is
// set by how many values a request holds, so that is measured first, from the
// bytes, in a pass that allocates nothing, together with how deeply the values
// nest; a request over either bound is refused by itself, as one over the frame
// limit is. JSON is measured as it arrives, so that one over a bound is not even
// held whole (readRequest); protocol buffers are measured once they have arrived,
// before they are decoded.
//
// The bounds are far above what clients send. The browser SDK writes every call
// with its arguments in it as the query text, which is one JSON string: a template
// save, the largest request there is, holds five values however large its
// document. An AI chat holds three or four for each message of its conversation.
// A hundred thousand values cost at most about 31 MB to decode (an empty object,
// the dearest value, is 310 bytes).
//
// Depth is bounded for a reason of its own. protojson counts one level for each list
// or object nested in variables and refuses past ten thousand; the server decodes
// the same request as protocol buffers, where each such level is two messages (a
// Value and its list or Struct), and refuses past five thousand. A request between
// the two passed the bridge and ended the session at the server's Recv. A thousand
// levels is at most about two thousand messages, which every decoder takes.

import (
	"errors"
	"io"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const (
	// maxRequestValues is the most values one request may hold: in JSON, its
	// strings, numbers, booleans, nulls, objects and arrays (a key is not a value);
	// in protocol buffers, the message, every field of it and of every message
	// inside it, and every number in a packed list.
	maxRequestValues = 100_000
	// maxRequestDepth is how deeply one request's values may nest: in JSON, objects
	// and arrays open at once; in protocol buffers, messages inside messages.
	maxRequestDepth = 1_000
)

// requestShape is what a request holds: how many values, and how deeply they nest.
type requestShape struct {
	values  int64
	deepest int
}

// over says whether the shape is over a bound, and which. Depth is looked at first:
// the measure of protocol buffers does not look deeper than the depth bound, so a
// count it reports for a request within that bound is the whole count.
func (s requestShape) over() (overLimit, bool) {
	switch {
	case s.deepest > maxRequestDepth:
		return overLimit{unit: inLevels, size: int64(s.deepest), limit: maxRequestDepth}, true
	case s.values > maxRequestValues:
		return overLimit{unit: inValues, size: s.values, limit: maxRequestValues}, true
	}
	return overLimit{}, false
}

// jsonShape measures JSON text as it goes by, in pieces of any size, keeping a few
// counters and allocating nothing however long the text is. Only the structure
// outside strings is read. A string is skipped to its closing quote, the first
// quote no backslash escapes, and is a key if a colon follows it and a value if
// anything else does. A number, true, false or null is a value where it begins.
//
// It does not check that the text is JSON: protojson does that, and stops at the
// first thing that is not. What protojson decodes before it stops is what this has
// counted, but for the one string it may have decoded as a value just before
// stopping at a colon that made it a key here.
type jsonShape struct {
	requestShape
	depth int // objects and arrays open now

	inString    bool // inside a string
	escaped     bool // inside a string, after a backslash
	inScalar    bool // inside a number, true, false or null
	afterString bool // a string has closed: a key if a colon comes next
}

func (j *jsonShape) scan(p []byte) {
	for _, c := range p {
		if j.inString {
			switch {
			case j.escaped:
				j.escaped = false
			case c == '\\':
				j.escaped = true
			case c == '"':
				j.inString = false
				j.afterString = true
			}
			continue
		}
		if j.afterString {
			if isJSONSpace(c) {
				continue
			}
			j.afterString = false
			if c == ':' {
				continue // the string was a key
			}
			j.values++
		}
		switch c {
		case '"':
			j.inScalar = false
			j.inString = true
		case '{', '[':
			j.inScalar = false
			j.values++
			j.depth++
			if j.depth > j.deepest {
				j.deepest = j.depth
			}
		case '}', ']':
			j.inScalar = false
			if j.depth > 0 {
				j.depth--
			}
		case ',', ':', ' ', '\t', '\n', '\r':
			j.inScalar = false
		default:
			if !j.inScalar {
				j.inScalar = true
				j.values++
			}
		}
	}
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// finish is the end of the text: a string that closed last was a value. It says
// whether the text is over a bound. Calling it again changes nothing, and a nil
// jsonShape (a message that is not JSON) is over none.
func (j *jsonShape) finish() bool {
	if j == nil {
		return false
	}
	if j.afterString {
		j.afterString = false
		j.values++
	}
	_, over := j.over()
	return over
}

// errOverItsShape stops the reading of a request that has gone over a bound of its
// shape, so that what is kept of it stops there (see readRequest).
var errOverItsShape = errors.New("memqlws: the request is over a bound of its shape")

// shapeReader measures a JSON request as it is read.
type shapeReader struct {
	r     io.Reader
	shape *jsonShape
	// stopAtBound makes the read that takes the request over a bound say so with
	// errOverItsShape, once. While the rest of a refused request is read and
	// discarded it is off, so that all of it is read.
	stopAtBound bool
}

func (s *shapeReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.shape.scan(p[:n])
	if s.stopAtBound && err == nil {
		if _, over := s.shape.over(); over {
			s.stopAtBound = false
			return n, errOverItsShape
		}
	}
	return n, err
}

// clientMessage is the descriptor a request in protocol buffers is read by.
var clientMessage = (&memqlv1.MemqlClientMessage{}).ProtoReflect().Descriptor()

// measureProto measures a request in protocol buffers the way jsonShape measures
// JSON: the message is a value, and so is every field of it and of every message
// inside it, and every number in a packed list; every message inside another is a
// level deeper. It reads the encoding by the message's descriptor and allocates
// nothing. It goes no deeper than one level past the depth bound, which bounds its
// own recursion. Where it cannot read an encoding it stops reading that message;
// proto.Unmarshal reads the encoding the same way and fails there, having decoded
// no more than this counted.
func measureProto(b []byte) requestShape {
	shape := requestShape{values: 1}
	shape.walk(b, clientMessage, 1)
	return shape
}

func (s *requestShape) walk(b []byte, md protoreflect.MessageDescriptor, depth int) {
	if depth > s.deepest {
		s.deepest = depth
	}
	if depth > maxRequestDepth {
		return
	}
	fields := md.Fields()
	for len(b) > 0 {
		number, kind, n := protowire.ConsumeTag(b)
		if n < 0 {
			return
		}
		b = b[n:]
		s.values++
		fd := fields.ByNumber(number)
		if kind != protowire.BytesType || fd == nil {
			n = protowire.ConsumeFieldValue(number, kind, b)
			if n < 0 {
				return
			}
			b = b[n:]
			continue
		}
		value, n := protowire.ConsumeBytes(b)
		if n < 0 {
			return
		}
		b = b[n:]
		switch {
		case fd.IsMap():
			s.walk(value, fd.Message(), depth) // an entry is a key and a value, not a level
		case fd.Message() != nil:
			s.walk(value, fd.Message(), depth+1)
		case fd.IsList():
			s.values += packedNumbers(fd.Kind(), value)
		}
	}
}

// packedNumbers is how many numbers a packed list of the given kind holds.
func packedNumbers(kind protoreflect.Kind, b []byte) int64 {
	switch kind {
	case protoreflect.StringKind, protoreflect.BytesKind:
		return 0 // never packed: each string is a field of its own
	case protoreflect.Fixed32Kind, protoreflect.Sfixed32Kind, protoreflect.FloatKind:
		return int64(len(b) / 4)
	case protoreflect.Fixed64Kind, protoreflect.Sfixed64Kind, protoreflect.DoubleKind:
		return int64(len(b) / 8)
	}
	var n int64
	for _, c := range b {
		if c < 0x80 { // the last byte of a varint
			n++
		}
	}
	return n
}
