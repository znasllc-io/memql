package memql

// Tests for the DSL spec export gRPC handler (issue memql#2125 / A4).
// handleDslSpec returns the portable JSON form of the DSL authoring surface
// (component/language/dslspec). The round-trip test proves the handler returns
// valid JSON that unmarshals back into a dslspec.Spec carrying the same version
// + the full construct count Build() produces -- i.e. the wire form is a
// faithful, lossless export of the in-process spec.

import (
	"encoding/json"
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/language/dslspec"
)

// newSpecSession builds a minimal streamSession; handleDslSpec needs no access
// context or DB -- the spec is assembled from in-process grammar tables.
func newSpecSession(t *testing.T) (*streamSession, *captureStream) {
	t.Helper()
	cs := newCaptureStream(t)
	svc := &service{
		logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
	return &streamSession{service: svc, stream: cs, logger: svc.logger}, cs
}

func dispatchSpec(s *streamSession) error {
	env := &memqlv1.MemqlClientMessage{
		MessageId: "m1",
		Payload: &memqlv1.MemqlClientMessage_DslSpec{
			DslSpec: &memqlv1.DslSpecMsg{RequestId: "r1"},
		},
	}
	return s.handleDslSpec(env, env.GetDslSpec())
}

// The handler returns valid JSON that round-trips back into a dslspec.Spec with
// the same version + construct count the in-process Build() produces, and echoes
// the version at the envelope level for cheap consumer checks.
func TestDslSpec_RoundTrip(t *testing.T) {
	s, cs := newSpecSession(t)
	require.NoError(t, dispatchSpec(s))

	res := cs.lastSent().GetDslSpecResult()
	require.NotNil(t, res, "reply must be a DslSpecResult")
	assert.Equal(t, "r1", res.GetRequestId())

	// Envelope version mirrors the embedded schema version (cheap check path).
	assert.Equal(t, dslspec.SpecVersion, res.GetVersion(), "envelope version must mirror SpecVersion")

	// The JSON must unmarshal back into a dslspec.Spec...
	var got dslspec.Spec
	require.NoError(t, json.Unmarshal([]byte(res.GetSpecJson()), &got), "spec_json must be valid dslspec.Spec JSON")

	// ...and be a faithful, lossless export of what Build() produces in-process.
	want := dslspec.Build()
	assert.Equal(t, want.Version, got.Version, "round-tripped spec version must match Build()")
	assert.Equal(t, len(want.Constructs), len(got.Constructs), "construct count must survive the round-trip")
	assert.NotEmpty(t, got.Constructs, "the exported spec must carry constructs")
	assert.Equal(t, len(want.Annotations), len(got.Annotations), "annotation count must survive the round-trip")
	assert.Equal(t, len(want.Keywords), len(got.Keywords), "keyword count must survive the round-trip")
	assert.Equal(t, len(want.Operators), len(got.Operators), "operator count must survive the round-trip")
	assert.Equal(t, len(want.FieldTypes), len(got.FieldTypes), "field-type count must survive the round-trip")
	assert.Equal(t, len(want.NextRules), len(got.NextRules), "next-rule count must survive the round-trip")

	// The embedded spec.version matches the envelope version too.
	assert.Equal(t, res.GetVersion(), got.Version, "embedded version must equal the envelope version")
}

// A nil body is rejected with an InvalidArgument query error rather than a
// panic or an empty reply.
func TestDslSpec_NilBody(t *testing.T) {
	s, cs := newSpecSession(t)
	require.NoError(t, s.handleDslSpec(&memqlv1.MemqlClientMessage{MessageId: "m1"}, nil))

	qe := cs.lastSent().GetQueryError()
	require.NotNil(t, qe, "a nil body must produce a query error")
	assert.Equal(t, "InvalidArgument", qe.GetError().GetCode())
}
