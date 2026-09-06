package proving

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
)

// benchActor is the synthetic principal every bench row is written under.
//
// The concepts declare @rowAuthz(clusterOwner) and AccessContext.IsClusterOwner()
// is exactly Role == RoleOwner, so weakening this role does not produce an
// error -- it produces a run that reports itself complete while every row it
// meant to write is silently refused. That failure mode is why this is a
// named constant with a paragraph rather than a literal at the call site.
const benchActor = "cluster:proving-suite"

// benchContext installs the actor and the internal origin stamp.
//
// BOTH HALVES ARE REQUIRED AND THEY FAIL DIFFERENTLY. Without the cluster-owner
// role the row-authz write guard refuses the row; without the internal-origin
// stamp the function validator refuses the @serverOnly mutation with ONE WARN
// and nothing above it hears. Either way the suite reports a clean run and the
// published record stays empty, which is the exact shape of bug that survives
// a release.
//
// component/proving holds an entry in call_origin_conformance_test.go's
// allowlist for this stamp. It is not a widening: the only mutations it is for
// are createBenchRun and createBenchSample, both @serverOnly, and both writing
// rows the README's claims gate reads.
func benchContext(ctx context.Context) context.Context {
	claims := map[string]any{"sub": benchActor, "role": "system"}
	ctx = auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: benchActor, Claims: claims})
	ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: benchActor,
		Role:   auth.RoleOwner,
		// The rank rules do not govern the cluster acting as itself.
		Unranked: true,
		// The rows are the deployment's, not this label's.
		Synthetic: true,
	})
	return auth.ContextWithInternalOrigin(ctx)
}

// WriteRows publishes a suite result as v1:bench:run plus one v1:bench:sample
// per figure.
//
// A ROW WRITE NEVER FAILS THE RUN. The journal takes the same position for the
// same reason: the measurement is the product of the run, and losing the
// published copy of it must not turn a green suite red. Every refusal is
// returned so the caller can report it, and the caller reports it as a WARNING
// beside a successful envelope rather than as a failure.
func (r *Runner) WriteRows(ctx context.Context, result SuiteResult) (runId string, problems []string) {
	ctx = benchContext(ctx)

	runId = "v1:bench:run:" + id.NewShortId()
	verdict := "pass"
	if len(result.Blocking()) > 0 {
		verdict = "blocked"
	}

	args := map[string]any{
		"benchRunId":        runId,
		"tier":              string(r.Prov.Tier),
		"commit":            r.Prov.Commit,
		"corpusFingerprint": result.Scorecard.CorpusFingerprint,
		"scenarioCount":     scenarioCount(result),
		"verdict":           verdict,
		"runner":            r.Prov.Runner,
		"startedAt":         time.Now().UTC().Format(time.RFC3339),
		"finishedAt":        time.Now().UTC().Format(time.RFC3339),
	}
	if len(r.Prov.ModelIds) > 0 {
		args["modelIds"] = r.Prov.ModelIds
	}
	// costUsd is OMITTED rather than written as zero on a CI run. A CI run
	// reaches no provider, and "it cost nothing" and "no cost was measured"
	// are different claims -- about money as much as anything else.
	if r.Prov.CostUSD != nil {
		args["costUsd"] = *r.Prov.CostUSD
	}
	if b := result.Blocking(); len(b) > 0 {
		args["blockingFailures"] = b
	}

	if err := r.execute(ctx, "createBenchRun", args); err != nil {
		return runId, []string{fmt.Sprintf("the bench run row was refused: %v", err)}
	}

	for _, e := range result.Scorecard.Entries {
		sampleArgs := map[string]any{
			"benchSampleId": "v1:bench:sample:" + id.NewShortId(),
			"benchRunId":    runId,
			"family":        string(e.Family),
			"scenarioId":    e.Scenario,
			"arm":           string(e.Arm),
			"metric":        string(e.Figure.Metric),
			"unit":          string(e.Figure.Unit),
			"tier":          string(e.Figure.Prov.Tier),
			"commit":        e.Figure.Prov.Commit,
			"measuredOn":    e.Figure.Prov.Date,
		}
		if e.Figure.IsMeasured() {
			st := e.Figure.Stat
			sampleArgs["n"] = st.N
			sampleArgs["median"] = st.Median
			sampleArgs["p10"] = st.P10
			sampleArgs["p90"] = st.P90
			sampleArgs["minimum"] = st.Min
			sampleArgs["maximum"] = st.Max
			sampleArgs["mad"] = st.MAD
		} else {
			// The statistical fields are LEFT OUT, not written as zero. The
			// concept's own doc says why: a schema that could only say "0"
			// would collapse "measured zero" and "not measured", which is the
			// one distinction this whole suite is built to keep.
			sampleArgs["absentReason"] = string(e.Figure.Absent)
			if e.Figure.Detail != "" {
				sampleArgs["detail"] = e.Figure.Detail
			}
		}
		if err := r.execute(ctx, "createBenchSample", sampleArgs); err != nil {
			problems = append(problems, fmt.Sprintf("%s/%s/%s was refused: %v", e.Scenario, e.Arm, e.Figure.Metric, err))
		}
	}
	return runId, problems
}

func (r *Runner) execute(ctx context.Context, name string, args map[string]any) error {
	_, err := r.Engine.Execute(ctx, renderCall(name, args))
	return err
}

// renderCall composes `mutation name(k: v, ...)` with SORTED keys.
//
// Strings go through langparser.QuoteString, never Go's %q. The two escape
// grammars diverge on NUL, \a, \v and \xNN, and the MemQL lexer rejects all of
// them -- a scenario id or a failure message is free text, so a Go-quoted
// string here is a call that parses on the happy path and refuses the whole
// row the first time a benchmark reports something with a control character in
// it (memql#3035, memql#3611).
func renderCall(name string, args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := args[k]
		if v == nil {
			// A nil argument is DROPPED, never rendered `null`: a null
			// optional refuses the whole row.
			continue
		}
		parts = append(parts, k+": "+renderValue(v))
	}
	return "mutation " + name + "(" + strings.Join(parts, ", ") + ")"
}

func renderValue(v any) string {
	switch t := v.(type) {
	case string:
		return langparser.QuoteString(t)
	case int:
		return strconv.Itoa(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case []string:
		parts := make([]string, 0, len(t))
		for _, s := range t {
			parts = append(parts, langparser.QuoteString(s))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return langparser.QuoteString(fmt.Sprintf("%v", v))
}

func scenarioCount(result SuiteResult) int {
	seen := map[string]bool{}
	for _, e := range result.Scorecard.Entries {
		seen[e.Scenario] = true
	}
	return len(seen)
}
