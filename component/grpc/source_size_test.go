package memql

// source_size_test.go -- the stream refuses DSL source over the bound before
// any handler lexes it.
//
// Every test here runs through handleMessage, the one place the check lives,
// with the payload built from the descriptor -- so a Sense or bundle request
// added later is covered by the same tables rather than by a list someone has
// to remember to extend.

import (
	"fmt"
	"maps"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/sense"
)

const (
	sourceBoundRequestId = "req-source"
	sourceBoundMessageId = "m-source"
)

// payloadsCarryingDSLSource are the client payloads DSL source rides in, with
// the bound each one earns: every Sense request carries the ONE FILE an author
// has open, and every authoring payload carries a BUNDLE of constructs, which
// is sized against the largest whole domain rather than the largest file.
var payloadsCarryingDSLSource = map[string]int{
	"authoring_session_define_bundle": langparser.MaxBundleBytes,
	"authoring_validate_bundle":       langparser.MaxBundleBytes,
	"durable_demote_bundle":           langparser.MaxBundleBytes,
	"durable_promote_bundle":          langparser.MaxBundleBytes,
	"stage_bundle":                    langparser.MaxBundleBytes,
	"sense_complete":                  langparser.MaxSourceBytes,
	"sense_definition":                langparser.MaxSourceBytes,
	"sense_diagnose":                  langparser.MaxSourceBytes,
	"sense_hover":                     langparser.MaxSourceBytes,
	"sense_signature_help":            langparser.MaxSourceBytes,
	"sense_tokenize":                  langparser.MaxSourceBytes,
}

// sourcePayloadNames is the pinned set, sorted.
func sourcePayloadNames() []string { return slices.Sorted(maps.Keys(payloadsCarryingDSLSource)) }

// sourceTooLargeMessage is the refusal of a source of n bytes over limit,
// written out here rather than built from the error type, so the wording is
// pinned by this file and not by the file that produces it.
func sourceTooLargeMessage(n, limit int) string {
	label := fmt.Sprintf("%d KiB", limit>>10)
	if limit >= 1<<20 {
		label = fmt.Sprintf("%d MiB", limit>>20)
	}
	return fmt.Sprintf("source is %d bytes, over the %s limit: send one file at a time, or split it into smaller files [source_too_large]", n, label)
}

// sourceFieldOf is the payload's DSL source field, or nil.
func sourceFieldOf(body protoreflect.MessageDescriptor) protoreflect.FieldDescriptor {
	for _, carrier := range dslSourceFields {
		if field := body.Fields().ByName(carrier.name); field != nil && field.Kind() == protoreflect.StringKind && !field.IsList() {
			return field
		}
	}
	return nil
}

// dslSourcePayloads is every client payload with a DSL source field, by oneof
// field name.
func dslSourcePayloads() map[string]protoreflect.FieldDescriptor {
	found := map[string]protoreflect.FieldDescriptor{}
	fields := clientPayloadOneof.Fields()
	for i := range fields.Len() {
		payload := fields.Get(i)
		if payload.Kind() == protoreflect.MessageKind && sourceFieldOf(payload.Message()) != nil {
			found[string(payload.Name())] = payload
		}
	}
	return found
}

// envelopeWithSource is an envelope carrying the named payload, with its
// request_id set and its DSL source field holding source.
func envelopeWithSource(t *testing.T, name, source string) *memqlv1.MemqlClientMessage {
	t.Helper()
	payload, ok := dslSourcePayloads()[name]
	require.True(t, ok, "%s carries no DSL source field", name)
	envelope := &memqlv1.MemqlClientMessage{MessageId: sourceBoundMessageId}
	reflected := envelope.ProtoReflect()
	value := reflected.NewField(payload)
	body := value.Message()
	body.Set(body.Descriptor().Fields().ByName("request_id"), protoreflect.ValueOfString(sourceBoundRequestId))
	body.Set(sourceFieldOf(body.Descriptor()), protoreflect.ValueOfString(source))
	reflected.Set(payload, value)
	return envelope
}

// newSourceBoundSession is an owner's stream with Sense configured, so every
// payload in the table would reach the work its handler does.
func newSourceBoundSession(t *testing.T) (*streamSession, *captureStream) {
	t.Helper()
	s, cs := newAuthoringSession(t, auth.RoleOwner, "owner-1")
	s.service.sense = sense.New(nil)
	return s, cs
}

// requireSourceRefusal requires the one message the stream sent to be the
// refusal, correlated to the request that carried the source.
func requireSourceRefusal(t *testing.T, cs *captureStream, want string) {
	t.Helper()
	cs.mu.Lock()
	sent := slices.Clone(cs.sent)
	cs.mu.Unlock()
	require.Len(t, sent, 1, "the refusal must be the only reply")
	refusal := sent[0].GetQueryError()
	require.NotNil(t, refusal, "reply must be a QueryError, got %T", sent[0].GetPayload())
	assert.Equal(t, sourceBoundRequestId, refusal.GetRequestId())
	assert.Equal(t, sourceBoundMessageId, sent[0].GetCorrelateTo())
	assert.Equal(t, "InvalidArgument", refusal.GetError().GetCode())
	assert.Equal(t, want, refusal.GetError().GetMessage())
}

// awaitSent waits for the stream's first reply: a Sense handler answers from a
// goroutine of its own.
func awaitSent(t *testing.T, cs *captureStream) *memqlv1.MemqlServerMessage {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if msg := cs.lastSent(); msg != nil {
			return msg
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no reply within 30s")
	return nil
}

// The set the field-name rule covers, pinned in both directions: a payload
// gains or loses the bound only by a change to this list. A DSL-carrying
// payload whose field is named something else is NOT covered, which is why the
// rule is written down in source_size.go.
func TestSourceBoundCoversEverySourceCarryingPayload(t *testing.T) {
	found := dslSourcePayloads()
	assert.Equal(t, sourcePayloadNames(), slices.Sorted(maps.Keys(found)))
	for name, payload := range found {
		id := payload.Message().Fields().ByName("request_id")
		assert.True(t, id != nil && id.Kind() == protoreflect.StringKind,
			"%s has no request_id, so its refusal could reach no waiting caller", name)
	}
}

func TestSenseAndBundleSourceOverTheBoundIsRefusedBeforeItsHandler(t *testing.T) {
	for _, name := range sourcePayloadNames() {
		limit := payloadsCarryingDSLSource[name]
		t.Run(name, func(t *testing.T) {
			s, cs := newSourceBoundSession(t)
			over := strings.Repeat("a", limit+1)
			require.NoError(t, s.handleMessage(envelopeWithSource(t, name, over)))
			requireSourceRefusal(t, cs, sourceTooLargeMessage(limit+1, limit))
		})
	}
}

// The two bounds are not one. A bundle between them -- over the FILE bound,
// under the bundle bound -- reaches its handler, and the same source on a
// Sense payload is refused. Collapsing them would leave the bundle path 1.18x
// clear of the largest whole domain in the tree.
func TestABundleBetweenTheBoundsReachesItsHandler(t *testing.T) {
	between := strings.Repeat("a", langparser.MaxSourceBytes+1)
	for _, name := range sourcePayloadNames() {
		limit := payloadsCarryingDSLSource[name]
		requestId, err := oversizedDSLSource(envelopeWithSource(t, name, between))
		if limit == langparser.MaxBundleBytes {
			assert.NoError(t, err, "%s carries a bundle and must accept this", name)
			assert.Empty(t, requestId, name)
			continue
		}
		assert.Error(t, err, "%s carries one file and must refuse this", name)
	}
}

// The refusal reads a length and nothing else. The source is `1+` repeated to
// 20 MiB -- about twenty million tokens, which the lexer holds as a slice of
// some gigabytes -- so a refusal that lexed it could not stay inside the
// allocation bound, and one that did not answers in microseconds.
func TestSenseAndBundleRefusalOfA20MiBSourceNeverLexesIt(t *testing.T) {
	const (
		huge          = 20 << 20
		maxRefusal    = time.Second
		maxRefusalMem = 1 << 20
	)
	source := strings.Repeat("1+", huge/2)
	for _, name := range sourcePayloadNames() {
		limit := payloadsCarryingDSLSource[name]
		t.Run(name, func(t *testing.T) {
			s, cs := newSourceBoundSession(t)
			envelope := envelopeWithSource(t, name, source)

			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			start := time.Now()
			require.NoError(t, s.handleMessage(envelope))
			elapsed := time.Since(start)
			runtime.ReadMemStats(&after)

			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("refused in %v, allocating %d bytes", elapsed, allocated)
			requireSourceRefusal(t, cs, sourceTooLargeMessage(huge, limit))
			assert.Less(t, elapsed, maxRefusal, "the refusal took %v", elapsed)
			assert.Less(t, allocated, uint64(maxRefusalMem), "the refusal allocated %d bytes", allocated)
		})
	}
}

func TestSenseAndBundleSourceAtTheBoundPassesTheGate(t *testing.T) {
	for _, name := range sourcePayloadNames() {
		at := strings.Repeat("a", payloadsCarryingDSLSource[name])
		requestId, err := oversizedDSLSource(envelopeWithSource(t, name, at))
		assert.NoError(t, err, name)
		assert.Empty(t, requestId, name)
	}
}

// A source of exactly the bound is diagnosed, and the diagnostic sits on its
// last line: the parser read all of it. Without this, a bound set to zero
// would pass every test above.
func TestSenseDiagnoseOfASourceAtTheBoundReachesTheParser(t *testing.T) {
	source := sourceOfLength(t, langparser.MaxSourceBytes, "concept unfinished {")
	lastLine := int32(strings.Count(source, "\n") + 1)

	s, cs := newSourceBoundSession(t)
	require.NoError(t, s.handleMessage(envelopeWithSource(t, "sense_diagnose", source)))

	reply := awaitSent(t, cs)
	result := reply.GetSenseDiagnoseResult()
	require.NotNil(t, result, "reply must be a SenseDiagnoseResult, got %T", reply.GetPayload())
	assert.Equal(t, sourceBoundRequestId, result.GetRequestId())
	require.Len(t, result.GetDiagnostics(), 1)
	diagnostic := result.GetDiagnostics()[0]
	assert.Equal(t, "parse-error", diagnostic.GetCode())
	assert.Equal(t, lastLine, diagnostic.GetRange().GetStart().GetLine())
}

// sourceOfLength is exactly n bytes of comment lines, ending in tail.
func sourceOfLength(t *testing.T, n int, tail string) string {
	t.Helper()
	line := "// " + strings.Repeat("x", 76) + "\n"
	var b strings.Builder
	b.Grow(n)
	for b.Len()+len(line)+len("//\n")+len(tail) <= n {
		b.WriteString(line)
	}
	b.WriteString("//" + strings.Repeat("x", n-b.Len()-len(tail)-len("//\n")) + "\n")
	b.WriteString(tail)
	require.Len(t, b.String(), n)
	return b.String()
}
