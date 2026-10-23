package probe

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/modelcall"
)

// fakeRuntime stands in for a local model. Every test in this file
// drives one, so the whole suite -- every figure, every absence, every
// sentence -- runs on a CI machine with no GPU and no Ollama.
type fakeRuntime struct {
	// reply answers by schema or tool name; always wins over it.
	reply  map[string]string
	always string
	// freeText is what a schema-less turn streams. Empty means a
	// default; setting it to a sentinel that never emits is not
	// possible on purpose -- see silent, below.
	freeText string
	// silent makes the runtime answer with no streamed output at all,
	// which is what a call that failed before generating looks like.
	silent bool
	// toolCall is what the model "asks for", by tool name.
	toolCall map[string]modelcall.ToolCall
	// hangOn names a prompt substring the runtime will block on, for
	// the timeout path.
	hangOn  string
	hangFor time.Duration
	// err fails every call.
	err error
	// usage is what the runtime reports. Known=false is a runtime that
	// said nothing, which is a real and common state.
	usage modelcall.Usage
	// calls records the prompts, so a test can assert what was asked.
	calls []modelcall.ChatRequest
}

func (f *fakeRuntime) Chat(ctx context.Context, req modelcall.ChatRequest, emit modelcall.Emit) (modelcall.Result, error) {
	f.calls = append(f.calls, req)
	if f.err != nil {
		return modelcall.Result{}, f.err
	}

	prompt := ""
	for _, m := range req.Messages {
		prompt += m.Content
	}
	if f.hangOn != "" && strings.Contains(prompt, f.hangOn) {
		select {
		case <-time.After(f.hangFor):
		case <-ctx.Done():
			return modelcall.Result{}, ctx.Err()
		}
	}

	if len(req.Tools) > 0 {
		call, ok := f.toolCall[req.Tools[0].Name]
		res := modelcall.Result{FinishReason: modelcall.FinishStop, Usage: f.usage}
		if ok {
			res.ToolCalls = []modelcall.ToolCall{call}
		}
		return res, nil
	}

	body := f.always
	if body == "" && len(req.Schema) > 0 {
		for key, reply := range f.reply {
			if strings.Contains(string(req.Schema), key) {
				body = reply
				break
			}
		}
	}
	if body == "" && len(req.Schema) == 0 {
		// A free-text turn. A real runtime always streams SOMETHING
		// here, and the throughput cases time the first chunk -- a fake
		// that emitted nothing would make every first-token figure
		// absent for a reason no real machine has.
		body = f.freeText
		if body == "" {
			body = "The excerpt describes a disk filling and three runs failing."
		}
	}
	if emit != nil && body != "" && !f.silent {
		if err := emit(body); err != nil {
			return modelcall.Result{}, err
		}
	}
	return modelcall.Result{FinishReason: modelcall.FinishStop, Usage: f.usage}, nil
}

func (f *fakeRuntime) Embed(context.Context, modelcall.EmbedRequest) (modelcall.Result, error) {
	return modelcall.Result{}, errors.New("the probe suite makes no embedding call")
}

// goodAnswers holds every schema, keyed by a string that appears in the
// schema itself so the fake can route on it.
func goodAnswers() map[string]string {
	return map[string]string{
		"severity": `{"severity":"high","summary":"the disk filled","owner":"platform"}`,
		"fields":   `{"fields":[{"name":"name","value":"Ada"},{"name":"email","value":"ada@example.com"}],"complete":true}`,
		"symptoms": `{"symptoms":["gateway timeout","slow under load"],"confidence":0.8}`,
		"decision": `{"decision":"rebuild","reason":"the cache is stale and two builds failed"}`,
		"patches":  `{"patches":[{"path":"a.go","hunk":"@@ -12 +12 @@"}]}`,
	}
}

func goodToolCalls() map[string]modelcall.ToolCall {
	return map[string]modelcall.ToolCall{
		"read_file": {Name: "read_file", ArgumentsJSON: `{"path":"/etc/hostname"}`},
		"search":    {Name: "search", ArgumentsJSON: `{"query":"NewClient","limit":50}`},
		"run_query": {Name: "run_query", ArgumentsJSON: `{"statement":"MATCH (w:worker) RETURN count(w)"}`},
	}
}

func knownUsage() modelcall.Usage {
	return modelcall.Usage{InputTokens: 8000, OutputTokens: 200, Known: true, Model: "m"}
}

// A clock that advances a fixed amount per reading, so throughput is a
// number this test can predict exactly.
func steppingClock(step time.Duration) func() time.Time {
	base := time.Unix(1757260800, 0).UTC()
	n := 0
	return func() time.Time {
		n++
		return base.Add(time.Duration(n) * step)
	}
}

func run(t *testing.T, rt *fakeRuntime, mut func(*Request)) Report {
	t.Helper()
	req := Request{
		SuiteVersion: SuiteVersion,
		Model:        "qwen3.5:9b",
		Client:       rt,
		Now:          steppingClock(time.Second),
	}
	if mut != nil {
		mut(&req)
	}
	rep, err := Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return rep
}

// -----------------------------------------------------------------------------
// The version gate
// -----------------------------------------------------------------------------

// A version the cockpit does not know is refused in BOTH directions.
// Figures are filed by (machine, model, suiteVersion), so running a
// different set of cases under a number the cluster asked for would
// file measurements of one thing as measurements of another.
func TestRunRefusesAnUnknownSuiteVersion(t *testing.T) {
	for _, asked := range []int{SuiteVersion + 1, SuiteVersion - 1, 0, 99} {
		_, err := Run(context.Background(), Request{SuiteVersion: asked, Client: &fakeRuntime{}})
		if err == nil {
			t.Fatalf("suite version %d was accepted; only %d is known", asked, SuiteVersion)
		}
		if !errors.Is(err, ErrUnknownSuite) {
			t.Fatalf("suite version %d: err = %v, want ErrUnknownSuite", asked, err)
		}
	}
}

// The refusal names BOTH numbers and the fix. A person reading
// "unsupported version" with neither has nothing to compare and nothing
// to do.
func TestTheVersionRefusalNamesBothNumbersAndTheFix(t *testing.T) {
	_, err := Run(context.Background(), Request{SuiteVersion: 2, Client: &fakeRuntime{}})
	want := "this machine knows probe suite version 1; the cluster asked for version 2. " +
		"Update the cockpit on this machine and run the probe again."
	if !strings.HasSuffix(err.Error(), want) {
		t.Fatalf("refusal =\n  %q\nwant it to end with\n  %q", err.Error(), want)
	}
}

// The gate runs BEFORE anything is asked of the runtime. A refused
// version that had already spent a 32K generation would be a refusal
// that cost the machine more than the run it declined.
func TestTheVersionGateRunsBeforeTheRuntimeIsTouched(t *testing.T) {
	rt := &fakeRuntime{}
	_, _ = Run(context.Background(), Request{SuiteVersion: 2, Client: rt})
	if len(rt.calls) != 0 {
		t.Fatalf("the runtime was called %d times behind a refused version", len(rt.calls))
	}
}

// -----------------------------------------------------------------------------
// Structured output
// -----------------------------------------------------------------------------

func TestStructuredValidityKnownGood(t *testing.T) {
	rep := run(t, &fakeRuntime{reply: goodAnswers(), usage: knownUsage()}, nil)
	f := rep.Figure(FigureStructuredValidity)
	if !f.Measured() || f.Value != 1.0 {
		t.Fatalf("structured_validity = %+v, want 1.0 measured", f)
	}
	if f.Detail != "5 of 5 schemas held" {
		t.Fatalf("Detail = %q", f.Detail)
	}
}

// Prose where a schema was asked for MEASURES as zero. It does not go
// absent: the model answered, and the answer was wrong -- which is the
// exact failure this figure exists to catch, and an absence would hide
// it as "could not run".
func TestStructuredValidityCountsProseAsAFailure(t *testing.T) {
	rep := run(t, &fakeRuntime{always: "Sure! Here is the triage result: it looks severe.", usage: knownUsage()}, nil)
	f := rep.Figure(FigureStructuredValidity)
	if !f.Measured() {
		t.Fatalf("prose must MEASURE as 0, not go absent: %+v", f)
	}
	if f.Value != 0.0 {
		t.Fatalf("Value = %v, want 0", f.Value)
	}
}

// The RIGHT KEYS WITH THE WRONG TYPES is the case a `required`-only
// check would score as valid, and it is the reason this package does
// not use one.
func TestStructuredValidityRejectsRightKeysWrongTypes(t *testing.T) {
	rep := run(t, &fakeRuntime{reply: map[string]string{
		// severity is not in the enum; confidence is a string; complete
		// is a string; patches is an object rather than an array.
		"severity": `{"severity":"catastrophic","summary":"x","owner":"y"}`,
		"fields":   `{"fields":[{"name":"a","value":"b"}],"complete":"yes"}`,
		"symptoms": `{"symptoms":["a"],"confidence":"0.8"}`,
		"decision": `{"decision":"rebuild","reason":42}`,
		"patches":  `{"patches":{"path":"a.go","hunk":"@@"}}`,
	}, usage: knownUsage()}, nil)
	f := rep.Figure(FigureStructuredValidity)
	if f.Value != 0.0 {
		t.Fatalf("Value = %v, want 0 -- every one of these has the required keys and the wrong types", f.Value)
	}
}

// A code fence is a rendering habit, not a schema violation. Leniency
// in ONE direction: the fence is stripped, and prose around the JSON is
// never recovered.
func TestStructuredValidityToleratesACodeFence(t *testing.T) {
	answers := map[string]string{}
	for k, v := range goodAnswers() {
		answers[k] = "```json\n" + v + "\n```"
	}
	rep := run(t, &fakeRuntime{reply: answers, usage: knownUsage()}, nil)
	if got := rep.Figure(FigureStructuredValidity).Value; got != 1.0 {
		t.Fatalf("Value = %v, want 1.0 -- a fence is not a schema failure", got)
	}
}

// A RUNTIME error is not the model failing a schema. The whole case
// goes absent, because a validity rate computed over a partial run is a
// number nobody can read.
func TestStructuredValidityGoesAbsentOnARuntimeError(t *testing.T) {
	rep := run(t, &fakeRuntime{err: errors.New("connection refused")}, nil)
	f := rep.Figure(FigureStructuredValidity)
	if f.Measured() {
		t.Fatalf("a runtime error was scored as a model failure: %+v", f)
	}
	if !strings.Contains(f.AbsentReason, "connection refused") {
		t.Fatalf("AbsentReason = %q, want the runtime's own words", f.AbsentReason)
	}
}

// The names of the schemas that failed. "0.60" says the model is
// unreliable; "triage, healing" says which prompts to stop routing
// here.
func TestStructuredValidityNamesTheSchemasThatMissed(t *testing.T) {
	answers := goodAnswers()
	answers["severity"] = `not json at all`
	rep := run(t, &fakeRuntime{reply: answers, usage: knownUsage()}, nil)
	f := rep.Figure(FigureStructuredValidity)
	if !strings.Contains(f.Detail, "missed: triage") {
		t.Fatalf("Detail = %q, want the failing schema named", f.Detail)
	}
}

// -----------------------------------------------------------------------------
// Tool calls
// -----------------------------------------------------------------------------

func TestToolCorrectnessKnownGood(t *testing.T) {
	rep := run(t, &fakeRuntime{toolCall: goodToolCalls(), usage: knownUsage()}, nil)
	f := rep.Figure(FigureToolCorrectness)
	if !f.Measured() || f.Value != 1.0 {
		t.Fatalf("tool_correctness = %+v, want 1.0", f)
	}
}

// Arguments that do not parse are a MISS. A truncated arguments string
// is the most consequential tool-calling failure there is, and a model
// that produces them should rank below one that does not.
func TestToolCorrectnessCountsUnparseableArgumentsAsAMiss(t *testing.T) {
	calls := goodToolCalls()
	calls["search"] = modelcall.ToolCall{Name: "search", ArgumentsJSON: `{"query":"NewCl`}
	rep := run(t, &fakeRuntime{toolCall: calls, usage: knownUsage()}, nil)
	f := rep.Figure(FigureToolCorrectness)
	if f.Value >= 1.0 {
		t.Fatalf("Value = %v, want a miss for truncated arguments", f.Value)
	}
	if !strings.Contains(f.Detail, "missed: search") {
		t.Fatalf("Detail = %q, want the failing definition named", f.Detail)
	}
}

// A model that calls the WRONG tool, or none, misses.
func TestToolCorrectnessCountsAMissingCall(t *testing.T) {
	rep := run(t, &fakeRuntime{toolCall: map[string]modelcall.ToolCall{}, usage: knownUsage()}, nil)
	if got := rep.Figure(FigureToolCorrectness).Value; got != 0.0 {
		t.Fatalf("Value = %v, want 0 when the model called nothing", got)
	}
}

// Required keys missing from otherwise-valid JSON is a miss: the tool
// would be dispatched without the argument it needs.
func TestToolCorrectnessRequiresTheDeclaredKeys(t *testing.T) {
	calls := goodToolCalls()
	calls["read_file"] = modelcall.ToolCall{Name: "read_file", ArgumentsJSON: `{"file":"/etc/hostname"}`}
	rep := run(t, &fakeRuntime{toolCall: calls, usage: knownUsage()}, nil)
	if !strings.Contains(rep.Figure(FigureToolCorrectness).Detail, "missed: read_file") {
		t.Fatalf("a call missing its required key was scored correct")
	}
}

// -----------------------------------------------------------------------------
// Throughput
// -----------------------------------------------------------------------------

// THE EXACT RATE, not merely a positive one.
//
// The clock steps one second per reading and the case reads it three
// times -- start, first token, end -- so elapsed is exactly two seconds
// and 200 tokens is exactly 100 tok/s. Asserting `> 0` instead would
// stay green if the division became a multiplication, or the raw token
// count, or a constant.
func TestThroughputIsTokensOverElapsed(t *testing.T) {
	rep := run(t, &fakeRuntime{
		reply: goodAnswers(), toolCall: goodToolCalls(),
		usage: modelcall.Usage{OutputTokens: 200, Known: true},
	}, nil)

	f := rep.Figure(FigureThroughput8K)
	if !f.Measured() {
		t.Fatalf("throughput_8k absent: %s", f.AbsentReason)
	}
	if f.Unit != "tokens/sec" {
		t.Fatalf("Unit = %q, want tokens/sec", f.Unit)
	}
	if f.Value != 100 {
		t.Fatalf("Value = %v, want 200 tokens over 2 s = 100", f.Value)
	}
	if f.Detail != "200 tokens in 2s" {
		t.Fatalf("Detail = %q", f.Detail)
	}

	// And the 32K case measures the SAME model over the same stepping
	// clock, so its figure is the same arithmetic on the same numbers.
	// A rate that varied between the two cases here would mean one of
	// them is reading a different clock.
	if g := rep.Figure(FigureThroughput32K); g.Value != f.Value {
		t.Fatalf("8K = %v and 32K = %v on one clock", f.Value, g.Value)
	}
}

// TIME TO FIRST TOKEN IS THE EXACT GAP, and it is measured from the
// case's start rather than from the process's. One second on the
// stepping clock: start is read first, the first delta second.
func TestTimeToFirstTokenIsTheGapFromTheCaseStart(t *testing.T) {
	rep := run(t, &fakeRuntime{
		reply: goodAnswers(), toolCall: goodToolCalls(), usage: knownUsage(),
	}, nil)

	f := rep.Figure(FigureTTFT8K)
	if !f.Measured() {
		t.Fatalf("ttft_8k absent: %s", f.AbsentReason)
	}
	if f.Value != 1 {
		t.Fatalf("Value = %v, want exactly one clock step", f.Value)
	}
	if f.Unit != "sec" {
		t.Fatalf("Unit = %q, want sec", f.Unit)
	}
}

// USAGE IS REPORTED, NEVER INFERRED. A runtime that reported no token
// count yields an ABSENT throughput rather than a rate computed from
// character counts -- a guessed numerator would rank machines on a
// tokeniser this code does not have.
func TestThroughputGoesAbsentWhenTheRuntimeReportedNoTokens(t *testing.T) {
	rep := run(t, &fakeRuntime{
		reply: goodAnswers(), toolCall: goodToolCalls(),
		usage: modelcall.Usage{Known: false},
	}, nil)
	f := rep.Figure(FigureThroughput8K)
	if f.Measured() {
		t.Fatalf("a throughput was invented from an unreported token count: %+v", f)
	}
	if !strings.Contains(f.AbsentReason, "reported no token count") {
		t.Fatalf("AbsentReason = %q", f.AbsentReason)
	}
}

// The 32K prompt really is four times the 8K one. Both cases share the
// filler's tokens-per-character constant, so the RATIO holds even
// though the constant is approximate -- and the ratio is what the
// comparison between the two figures depends on.
func TestTheThroughputPromptsAreInTheStatedRatio(t *testing.T) {
	small, large := len(fillerPrompt(8_000)), len(fillerPrompt(32_000))
	ratio := float64(large) / float64(small)
	if ratio < 3.9 || ratio > 4.1 {
		t.Fatalf("32K/8K prompt ratio = %.2f, want about 4", ratio)
	}
}

// The filler is deterministic: a prompt that varied between runs would
// put noise straight into the figure the fleet ranks on.
func TestTheThroughputPromptIsDeterministic(t *testing.T) {
	if fillerPrompt(8_000) != fillerPrompt(8_000) {
		t.Fatal("the filler prompt changed between two calls")
	}
}

// -----------------------------------------------------------------------------
// The supervisor
// -----------------------------------------------------------------------------

// A runaway case ends and reports an ABSENT figure naming the timeout.
// The run survives it: one wedged case is not a reason to lose the
// figures that measured cleanly, which is the case most worth
// reporting.
func TestCaseTimeoutReportsAnAbsentFigureAndTheRunContinues(t *testing.T) {
	rep := run(t, &fakeRuntime{
		reply: goodAnswers(), toolCall: goodToolCalls(), usage: knownUsage(),
		hangOn: "Summarise the following log excerpt", hangFor: 2 * time.Second,
	}, func(r *Request) { r.CaseTimeout = 20 * time.Millisecond })

	f := rep.Figure(FigureThroughput8K)
	if f.Measured() {
		t.Fatalf("a case that timed out reported a value: %+v", f)
	}
	want := "the 8K throughput case did not answer within 20ms and was ended."
	if f.AbsentReason != want {
		t.Fatalf("AbsentReason =\n  %q\nwant\n  %q", f.AbsentReason, want)
	}
	if !rep.Figure(FigureStructuredValidity).Measured() {
		t.Fatal("the cases that did answer were lost to the one that hung")
	}
}

// An interrupted probe reports the remaining cases as NOT RUN, which is
// a different sentence from a timeout: nobody's model failed anything.
func TestACancelledProbeSaysTheCasesWereNotRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := Run(ctx, Request{
		SuiteVersion: SuiteVersion, Model: "m",
		Client: &fakeRuntime{reply: goodAnswers(), usage: knownUsage()},
		Now:    steppingClock(time.Second),
	})
	if err != nil {
		t.Fatalf("a cancelled probe must report what it has, not fail: %v", err)
	}
	f := rep.Figure(FigureToolCorrectness)
	if f.Measured() {
		t.Fatal("a case ran after cancellation")
	}
	if !strings.Contains(f.AbsentReason, "was cancelled") {
		t.Fatalf("AbsentReason = %q, want the cancellation named", f.AbsentReason)
	}
}

// EVERY DECLARED FIGURE IS PRODUCED, on every path. A figure name a
// case declares but never emits is a row that silently never appears --
// which reads to an operator as a probe that did not finish, and to the
// engine as a measurement that was never taken.
func TestEveryDeclaredFigureIsProduced(t *testing.T) {
	for _, tc := range []struct {
		name string
		rt   *fakeRuntime
		mut  func(*Request)
	}{
		{"everything answers", &fakeRuntime{reply: goodAnswers(), toolCall: goodToolCalls(), usage: knownUsage()}, nil},
		{"the runtime is down", &fakeRuntime{err: errors.New("connection refused")}, nil},
		{"no usage reported", &fakeRuntime{reply: goodAnswers(), toolCall: goodToolCalls()}, nil},
		{
			"a case hangs",
			&fakeRuntime{
				reply: goodAnswers(), toolCall: goodToolCalls(), usage: knownUsage(),
				hangOn: "Summarise the following log excerpt", hangFor: 2 * time.Second,
			},
			func(r *Request) { r.CaseTimeout = 20 * time.Millisecond },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep := run(t, tc.rt, tc.mut)

			var want []string
			for _, c := range suite() {
				want = append(want, c.Names...)
			}
			if len(rep.Figures) != len(want) {
				t.Fatalf("%d figures, want %d (%v)", len(rep.Figures), len(want), want)
			}
			for _, name := range want {
				f := rep.Figure(name)
				if strings.Contains(f.AbsentReason, "has no case named") {
					t.Fatalf("declared figure %q was never produced", name)
				}
				if !f.Measured() && strings.TrimSpace(f.AbsentReason) == "" {
					t.Fatalf("%s is neither measured nor explained", name)
				}
			}
		})
	}
}

// Time to first token SURVIVES a runtime that reported no token count:
// it is measured from the clock alone, so the two figures fail
// independently. Collapsing them would lose the one figure a person
// waiting actually feels.
func TestTimeToFirstTokenIsMeasuredWithoutAUsageReport(t *testing.T) {
	rep := run(t, &fakeRuntime{
		reply: goodAnswers(), toolCall: goodToolCalls(),
		usage: modelcall.Usage{Known: false},
	}, nil)
	if rep.Figure(FigureThroughput8K).Measured() {
		t.Fatal("throughput was measured without a token count")
	}
	ttft := rep.Figure(FigureTTFT8K)
	if !ttft.Measured() {
		t.Fatalf("ttft_8k absent: %s", ttft.AbsentReason)
	}
	if ttft.Unit != "sec" {
		t.Fatalf("Unit = %q, want sec", ttft.Unit)
	}
}

// A call that streamed NOTHING has no first token to time, and that is
// an absence rather than a zero -- zero would be the fastest possible
// machine, the opposite of the truth.
func TestTimeToFirstTokenIsAbsentWhenNothingStreamed(t *testing.T) {
	rep := run(t, &fakeRuntime{reply: goodAnswers(), toolCall: goodToolCalls(), usage: knownUsage(), silent: true}, nil)
	f := rep.Figure(FigureTTFT8K)
	if f.Measured() {
		t.Fatalf("a first-token time was reported for a call that emitted nothing: %+v", f)
	}
	if !strings.Contains(f.AbsentReason, "no streamed output") {
		t.Fatalf("AbsentReason = %q", f.AbsentReason)
	}
}

// A figure the suite does not produce comes back as a SENTENCE rather
// than a silent zero value, so a caller asking for one that was never
// in this version reads why.
func TestAskingForAFigureTheSuiteDoesNotProduce(t *testing.T) {
	rep := run(t, &fakeRuntime{reply: goodAnswers(), toolCall: goodToolCalls(), usage: knownUsage()}, nil)
	f := rep.Figure("video_quality")
	if f.Measured() {
		t.Fatal("a figure the suite never produced came back measured")
	}
	if !strings.Contains(f.AbsentReason, `has no case named "video_quality"`) {
		t.Fatalf("AbsentReason = %q", f.AbsentReason)
	}
}

// Progress is emitted BEFORE each case, not only after. Somebody
// watching a probe that hangs has to read off the screen which case
// hung, and a line printed only on completion is the line the wedged
// case never prints.
// PROGRESS IS EMITTED BEFORE THE WORK, and the test proves the ORDER
// rather than the count.
//
// It records every event -- started and finished alike -- in the order
// they arrive, and asserts that each case's start precedes any figure
// for that case. Counting start events alone would stay green with the
// emit moved to AFTER c.Run, which is precisely the change that loses
// the property: somebody watching a probe that hangs has to read off
// the screen which case hung, and a line printed only on completion is
// the line the wedged case never prints.
func TestProgressAnnouncesEachCaseBeforeItRuns(t *testing.T) {
	type step struct {
		name    string
		started bool
	}
	var seen []step
	run(t, &fakeRuntime{reply: goodAnswers(), toolCall: goodToolCalls(), usage: knownUsage()},
		func(r *Request) {
			r.Progress = func(e Event) { seen = append(seen, step{e.Case, e.Started}) }
		})

	startedAt := map[string]int{}
	for i, s := range seen {
		if s.started {
			if _, dup := startedAt[s.name]; dup {
				t.Fatalf("%s announced twice: %v", s.name, seen)
			}
			startedAt[s.name] = i
			continue
		}
		at, ok := startedAt[s.name]
		if !ok {
			t.Fatalf("a figure for %s arrived before its case was announced: %v", s.name, seen)
		}
		if at > i {
			t.Fatalf("%s was announced after its figure", s.name)
		}
	}
	if len(startedAt) != len(suite()) {
		t.Fatalf("%d cases announced, want %d: %v", len(startedAt), len(suite()), seen)
	}
	if seen[0].name != FigureStructuredValidity || !seen[0].started {
		t.Fatalf("the first event was %+v, want the first case starting", seen[0])
	}
}

// A run with no client is refused rather than reporting a suite's worth
// of absences that all say the same thing.
func TestRunRefusesWithNoClient(t *testing.T) {
	if _, err := Run(context.Background(), Request{SuiteVersion: SuiteVersion}); err == nil {
		t.Fatal("want a refusal with no runtime client")
	}
}

// A CANCELLATION MUST NOT ERASE A MEASUREMENT THAT ALREADY HAPPENED.
// The window is one clock read wide -- the parent can be cancelled
// after c.Run has returned complete figures -- and overwriting a real
// value with "was not run" is exactly the figure-and-absence confusion
// this package exists to prevent, committed by the code that documents
// it.
func TestCancellationKeepsAFigureTheCaseAlreadyMeasured(t *testing.T) {
	measured := []Figure{
		{Name: FigureThroughput8K, Value: 41.2, Unit: "tokens/sec", Detail: "100 tokens in 2.4s"},
		{Name: FigureTTFT8K, AbsentReason: "no streamed output."},
	}
	absent := []Figure{
		{Name: FigureThroughput8K, AbsentReason: "was not run."},
		{Name: FigureTTFT8K, AbsentReason: "was not run."},
	}

	got := keepMeasured(measured, absent)
	if len(got) != 2 {
		t.Fatalf("got %d figures", len(got))
	}
	if !got[0].Measured() || got[0].Value != 41.2 {
		t.Fatalf("a real measurement was erased by the cancellation: %+v", got[0])
	}
	// And a figure that was ALREADY absent takes the cancellation's
	// reason, which is the more useful of the two.
	if got[1].Measured() || got[1].AbsentReason != "was not run." {
		t.Fatalf("an already-absent figure did not take the cancellation reason: %+v", got[1])
	}
}

// End to end: a probe cancelled partway keeps everything it measured
// and reports only the rest as not run.
func TestACancelledProbeKeepsWhatItAlreadyMeasured(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel once the structured case has answered, so the tool case is
	// the first to see a cancelled parent.
	rt := &fakeRuntime{reply: goodAnswers(), toolCall: goodToolCalls(), usage: knownUsage()}
	rep, err := Run(ctx, Request{
		SuiteVersion: SuiteVersion, Model: "m", Client: rt,
		Now: steppingClock(time.Second),
		Progress: func(e Event) {
			if e.Started && e.Case == FigureToolCorrectness {
				cancel()
			}
		},
	})
	if err != nil {
		t.Fatalf("a cancelled probe must report what it has: %v", err)
	}

	if f := rep.Figure(FigureStructuredValidity); !f.Measured() {
		t.Fatalf("the case that completed before the cancellation was erased: %+v", f)
	}
	if f := rep.Figure(FigureThroughput32K); f.Measured() {
		t.Fatalf("a case that never ran reported a value: %+v", f)
	}
}
