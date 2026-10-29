package app

// pipeline_review_model_failure_test.go -- what the independent review's model
// call reports when it fails.
//
// The driver prints a pipeline workflow's error whole on the run's check run,
// which is public on GitHub for a public repository, and masks secret values
// only. A model call's error is raw runtime text: a door that served from the
// owner's machine begins with the name that machine reported (its hostname,
// "Joses-MacBook-Pro", unless the owner replaced it), a vendor's error quotes
// what the vendor sent back, a local runtime's can carry a path or a request id.
// So the review reports a failed model call by its stable code and the router's
// words, keeps the original in the chain for the callers that decide on its
// type, and logs the original for the owner.
//
// The "a failed model call is reported by its code" subtest of
// TestPipelineIndependentReviewRestoresCurrentSourceOwner (the db-gated sibling
// in pipeline_review_owner_db_test.go) drives the same through the real engine.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelinerun"
	"github.com/znasllc-io/memql/component/router"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

const (
	reportedHostname = "Joses-MacBook-Pro"
	reportedPath     = "/Users/jose/.ollama/models"
	reportedRequest  = "req_01HXABCDEF"
)

// machineRefusal is a typed refusal whose text leads with the machine's name,
// the shape a fleet door's error takes (Candidate.Label() first).
type machineRefusal struct{}

func (*machineRefusal) Error() string {
	return reportedHostname + ": the model is not installed (" + reportedPath + ")"
}
func (*machineRefusal) Code() string { return memql.RefusalCodeNoLocalModel }

func reviewFailureInput() pipelinerun.IndependentReviewInput {
	return pipelinerun.IndependentReviewInput{
		Kind:   "ordinary",
		Source: pipelinerun.ReviewSource{PipelineID: "pipe-1", Repository: "acme/repo"},
	}
}

func TestAFailedReviewModelCallIsReportedByItsCodeNotItsText(t *testing.T) {
	a := &App{}
	ctx := context.Background()
	doors := []work.DoorReport{{Name: "fleet:strongest", Door: "fleet", Reason: reportedHostname + ": refused before start " + reportedPath}}

	t.Run("a typed refusal whose text starts with the machine's reported name", func(t *testing.T) {
		in := &machineRefusal{}
		got := a.pipelineModelFailure(ctx, reviewFailureInput(), in)
		require.Equal(t, router.FailureReason(in), got.Error())
		require.Contains(t, got.Error(), memql.RefusalCodeNoLocalModel)
		requireNothingReported(t, got)
		var typed *machineRefusal
		require.ErrorAs(t, got, &typed, "callers that decide on the refusal's type still find it")
		require.ErrorIs(t, got, in)
	})

	t.Run("the router's own refusal, whose doors quote the machine", func(t *testing.T) {
		in := &router.InferenceUnavailable{Code: work.RefusalEveryDoorShut, Doors: doors}
		got := a.pipelineModelFailure(ctx, reviewFailureInput(), in)
		require.Equal(t, "failed when called: every_door_shut: no door to a model is open for this call", got.Error())
		requireNothingReported(t, got)
		var typed *router.InferenceUnavailable
		require.ErrorAs(t, got, &typed)
		// A refusal's doors stay readable: both readers keep working, the typed
		// one through the chain and the message one through the code.
		code, found, ok := work.DoorsFrom(got)
		require.True(t, ok)
		require.Equal(t, work.RefusalEveryDoorShut, code)
		require.Equal(t, doors, found, "the structured door report survives the wrapping")
		code, ok = work.InferenceRefusalCode(got.Error())
		require.True(t, ok)
		require.Equal(t, work.RefusalEveryDoorShut, code, "a failure that travels as a string still names its code")
	})

	t.Run("a vendor's error quoting a request id and a local path", func(t *testing.T) {
		in := fmt.Errorf("%s: 500 internal server error (request %s, cache %s)", reportedHostname, reportedRequest, reportedPath)
		got := a.pipelineModelFailure(ctx, reviewFailureInput(), in)
		require.Equal(t, "failed when called: upstream: the provider failed", got.Error())
		requireNothingReported(t, got)
		require.ErrorIs(t, got, in)
	})

	t.Run("a deadline under a door's label", func(t *testing.T) {
		got := a.pipelineModelFailure(ctx, reviewFailureInput(), fmt.Errorf("%s: %w", reportedHostname, context.DeadlineExceeded))
		require.Equal(t, "failed when called: timeout: timed out", got.Error())
		requireNothingReported(t, got)
		require.ErrorIs(t, got, context.DeadlineExceeded, "a cancel or a deadline is still recognised as one")
	})

	t.Run("a run ceiling stays visible to errors.As", func(t *testing.T) {
		in := &memql.RunCeilingError{RunId: "run-7", StepKey: "review", Breach: memql.RunCeilingBreach{Ceiling: "tokens", Limit: "1000", Actual: "1200", Reason: "the token budget is spent"}}
		got := a.pipelineModelFailure(ctx, reviewFailureInput(), in)
		var ceiling *memql.RunCeilingError
		require.ErrorAs(t, got, &ceiling, "the automation journal parks on this type")
		require.Same(t, in, ceiling)
		// A string-only matcher must not mistake a run's own ceiling for the
		// process cost ceiling the router refuses with.
		_, matched := work.InferenceRefusalCode(got.Error())
		require.False(t, matched, got.Error())
	})

	t.Run("no error is no failure", func(t *testing.T) {
		require.NoError(t, a.pipelineModelFailure(ctx, reviewFailureInput(), nil))
	})
}

func requireNothingReported(t *testing.T, err error) {
	t.Helper()
	for _, raw := range []string{reportedHostname, reportedPath, reportedRequest} {
		require.NotContains(t, err.Error(), raw, "a public check run would print this")
	}
}

// The owner can still read what the model call said: the original error goes to
// this node's log, tagged with the pipeline and repository (and with the work run
// as its subject when the context carries one, as this test's does), and the
// line says what the pipeline reported instead.
func TestTheOriginalReviewModelErrorIsLoggedForTheOwner(t *testing.T) {
	var buf bytes.Buffer
	a := &App{Logger: slog.New(slog.NewJSONHandler(&buf, nil))}
	ctx := common.ContextWithRun(context.Background(), common.RunContext{RunId: "v1:work:run:run-7", OwnerUserId: "owner-1"})
	in := &machineRefusal{}

	got := a.pipelineModelFailure(ctx, reviewFailureInput(), in)

	var lines []map[string]any
	for _, raw := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var line map[string]any
		require.NoError(t, json.Unmarshal(raw, &line), string(raw))
		lines = append(lines, line)
	}
	require.Len(t, lines, 1, "one line per failed call")
	line := lines[0]
	require.Equal(t, "WARN", line["level"])
	require.Equal(t, in.Error(), line["error"], "the owner reads the original text here")
	require.Equal(t, got.Error(), line["reported"], "and what the check run says instead")
	require.Equal(t, map[string]any{"id": "run-7", "concept": "v1:work:run"}, line["subject"], "the run is the subject, as a bare id")
	require.Equal(t, "pipe-1", line["pipeline"])
	require.Equal(t, "acme/repo", line["repository"])
	require.False(t, strings.Contains(got.Error(), reportedHostname))
}

// A node with no logger (a test, a build that wired none) still words the
// error: the wording is the part that must never be skipped.
func TestAReviewModelErrorIsWordedEvenWithNoLogger(t *testing.T) {
	got := (&App{}).pipelineModelFailure(context.Background(), reviewFailureInput(), errors.New(reportedHostname+": failed"))
	require.NotContains(t, got.Error(), reportedHostname)
}
