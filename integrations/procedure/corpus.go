package procedure

import (
	"context"
	"encoding/json"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	proc "github.com/znasllc-io/memql/component/procedure"
	"github.com/znasllc-io/memql/component/work"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/num"
)

// corpus.go -- rows to values, and the only half of this epic that reads a
// row. Everything below it is a decision over values in component/procedure.

// Level names which of D24's two corpus levels to mine. The pipeline below is
// identical for both, which is why the level is a PARAMETER rather than two
// functions: a second copy is a copy that drifts, and the record's claim that
// "the one pipeline lifts repeated automation sequences into higher
// automations by the same compression rule" is only true if it is literally
// the same pipeline.
type Level int

const (
	// LevelAction mines the actions inside recorded sessions.
	LevelAction Level = 1
	// LevelAutomation mines automation invocations -- subrun steps -- so a
	// repeated sequence of automations becomes a higher automation.
	LevelAutomation Level = 2
)

// appActionTypes are the step types a procedure is made of at the action level
// (epic memql#5396, design D2 and D12).
//
// app_answer IS NOT ONE, although a session records it as a step. It is the
// session's structured answer -- the one part of a session that IS
// intelligence (component/work reads it as reasoning) -- and it carries no
// tool_result observation, because nothing was executed. Mined, it was the
// last step of every recording, so it ended every procedure: a step no replay
// can perform, whose recordings agreed on nothing a comparison could hold, so
// every replay of every procedure would have failed on it and none could ever
// be promoted. A procedure is the ACTIONS; the answer is what the app said
// about them.
var appActionTypes = map[string]bool{
	"exec":     true,
	"fs_write": true,
	"fs_read":  true,
	"fetch":    true,
	"mcp":      true,
}

// pureReadTypes duplicates component/procedure's set for ONE purpose: deciding
// which steps need a Consumed verdict. The module drops an unconsumed pure
// read; only this package can compute Consumed, because it needs the whole
// run.
var pureReadTypes = map[string]bool{"fs_read": true, "fetch": true, "mcp": true}

// replayTriggerPrefix marks a run a learned procedure's REPLAY opened
// (`procedure:<mode>`). A replay run is never a recording: learning from one
// would teach a procedure its own output, and shadowing one would compare a
// procedure with itself.
const replayTriggerPrefix = "procedure:"

// isReplayRun reports whether a run row is a procedure's replay.
func isReplayRun(run map[string]any) bool {
	return strings.HasPrefix(strings.TrimSpace(str(run, "triggeredBy")), replayTriggerPrefix)
}

// corpusKey names one mining job.
type corpusKey struct {
	OwnerUserId   string
	GoalSignature string
	Level         Level
}

// recording is one run of the corpus: its steps lowered for the pipeline, and
// everything the lift reads about it that the pipeline does not.
type recording struct {
	RunId string
	// Run is the run row (shape workRunFull).
	Run map[string]any
	// Steps are the level's steps, in recorded order, arguments included.
	Steps []proc.Step
	// Evidence is what each step REPORTED, by step key -- the observables a
	// replay is compared on, and the paths its footprint is measured from.
	Evidence map[string]stepEvidence
	// Fingerprint is the session's environment at its start, from the first
	// step that carries it; Workspace is that fingerprint's working directory.
	Fingerprint map[string]any
	Workspace   string
	// Verdicts are a person's step-level feedback, by step key.
	Verdicts map[string][]stepVerdict
}

// stepEvidence is what one recorded action reported.
type stepEvidence struct {
	StepType    string
	Observation work.StepObservation
	// Paths are every file path the action named -- in its arguments and in
	// its contents -- for the footprint. A path inside the workspace is
	// already written relative to it (relativize.go).
	Paths []string
	// Workspace is the directory the action's paths were read against: the
	// recording's fingerprint cwd, or -- for a recording that carried no
	// fingerprint -- the action's own. The footprint is measured against the
	// SAME directory the arguments were relativized against, or a path the
	// rewrite made relative would be judged against a different root.
	Workspace string
}

// stepVerdict is one feedback row on one step version.
type stepVerdict struct {
	Version   int
	Verdict   work.Verdict
	CreatedAt time.Time
	Order     int
}

// variables is the recording run's goal input.
func (r recording) variables() map[string]any { return obj(r.Run, "variables") }

// loadCorpus reads one owner's recorded runs for one goal signature, oldest
// first, as recordings.
//
// Six rules, each of which fails silently if it is missed.
//
// THE READ RUNS UNDER THE OWNER'S ACTOR, never a cluster-owner variant. The
// composite tier would happily serve every owner's runs to a sweep, and the
// template mined from two people's recordings is correct about neither.
//
// THE READ IS BY SIGNATURE, pushed down (workRunsForOwnerGoalSignature), and a
// blank signature reads nothing: grouping runs of different goals mines across
// unrelated work, and the procedure that comes out is correct about nothing.
//
// OLDEST FIRST. The query answers newest first, and the corpus is reversed:
// Generalize reads the template's literal hints off its FIRST instance, so a
// corpus that put each new recording first would re-spell an unchanged
// procedure on every recording -- and a re-spelled procedure is a new version.
//
// A REPLAY RUN IS NOT A RECORDING (triggeredBy `procedure:`), and a disliked
// recording contributes nothing (D23): exclusion happens here, in the loader,
// so that nothing downstream can forget it.
//
// A RECORDING THAT CANNOT BE READ BACK IS SKIPPED WHOLE: an action with no
// tool_result observation, or whose arguments were truncated, is a step whose
// call cannot be reproduced, and a template generalized over it would replay a
// call nobody made.
//
// Consumed IS COMPUTED HERE. Whether a later step referenced an earlier
// result is a property of the whole run, which the module cannot see.
func (i *Integration) loadCorpus(ctx context.Context, k corpusKey) ([]recording, error) {
	owner := strings.TrimSpace(k.OwnerUserId)
	sig := strings.TrimSpace(k.GoalSignature)
	if owner == "" || sig == "" {
		return nil, nil
	}
	actorCtx := ownerActor(ctx, owner)

	runs, err := i.store.query(actorCtx, "query "+call("workRunsForOwnerGoalSignature", map[string]any{"goalSignature": sig}))
	if err != nil {
		return nil, err
	}
	sortOldestFirst(runs)

	var out []recording
	for _, run := range runs {
		runId := str(run, "id")
		if runId == "" {
			continue
		}
		// The query already filters on all three. Checked again because a
		// row that is not what the filter promised would be mined into a
		// procedure somebody may later run with no model.
		if str(run, "goalSignature") != sig || str(run, "status") != "succeeded" || isReplayRun(run) {
			continue
		}
		rec, ok, err := i.loadRecording(actorCtx, run, k.Level)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, rec)
		}
	}
	return out, nil
}

// loadRecording reads one run's steps and evidence. ok is false for a run
// that is not usable as a recording at this level.
func (i *Integration) loadRecording(ctx context.Context, run map[string]any, level Level) (recording, bool, error) {
	runId := str(run, "id")
	rec := recording{RunId: runId, Run: run, Evidence: map[string]stepEvidence{}}

	observations, err := i.store.query(ctx, "query "+call("workObservationsForOwnerRun", map[string]any{"runId": runId}))
	if err != nil {
		return rec, false, err
	}
	feedback := readFeedback(observations)
	if feedback.runDisliked {
		return rec, false, nil
	}
	rec.Verdicts = feedback.steps

	rows, err := i.store.query(ctx, "query "+call("workStepsForOwnerRun", map[string]any{"runId": runId}))
	if err != nil {
		return rec, false, err
	}
	sort.SliceStable(rows, func(a, b int) bool { return intOf(rows[a], "seq") < intOf(rows[b], "seq") })

	for _, r := range rows {
		if fp := obj(r, "fingerprint"); len(fp) > 0 && rec.Fingerprint == nil {
			rec.Fingerprint = fp
			rec.Workspace = strings.TrimSpace(str(fp, "cwd"))
		}
	}

	results := toolResultsByStep(observations)
	steps := make([]proc.Step, 0, len(rows))
	for _, r := range rows {
		stepType := str(r, "stepType")
		if !levelAccepts(level, stepType) {
			continue
		}
		key := str(r, "key")
		input := obj(r, "input")
		if level == LevelAction {
			// THE ARGUMENTS LIVE ON THE OBSERVATION (gap G3): the session
			// writer never wrote step.input, so an action's call is the
			// tool_result's `args`, whole up to its ceiling.
			o, found := results[key]
			if !found {
				i.log().Debug("procedure: a recorded action has no tool_result; the recording is skipped",
					"runId", runId, "stepKey", key)
				return rec, false, nil
			}
			args, reproducible := observationArgs(o)
			if !reproducible {
				i.log().Debug("procedure: a recorded action's arguments were truncated; the recording is skipped",
					"runId", runId, "stepKey", key)
				return rec, false, nil
			}
			// THE WORKSPACE IS WRITTEN AS THE WORKSPACE before anything is
			// canonicalized (relativize.go): the fingerprint's cwd, or --
			// for a recording that carried none -- the action's own.
			ws := actionWorkspace(rec.Workspace, o)
			input = relativizeArgs(args, ws)
			rec.Evidence[key] = i.stepEvidenceOf(ctx, stepType, o, input, ws)
		}
		result := obj(r, "result")
		steps = append(steps, proc.Step{
			RunId:        runId,
			Key:          key,
			Seq:          intOf(r, "seq"),
			StepType:     stepType,
			Call:         proc.Call{Construct: str(obj(r, "call"), "construct"), Name: str(obj(r, "call"), "name")},
			Input:        input,
			ResultDigest: str(r, "resultFingerprint"),
			EffectDigest: footprintDigest(obj(r, "actualFootprint")),
			ResultValue:  resultValue(result),

			GoalSignature: str(run, "goalSignature"),
		})
	}
	if len(steps) == 0 {
		// A run with nothing at this level is not a recording OF this level:
		// the goal's own run holds statements, and its session child holds
		// the actions.
		return rec, false, nil
	}
	markConsumed(steps)
	rec.Steps = steps
	return rec, true, nil
}

func levelAccepts(level Level, stepType string) bool {
	switch level {
	case LevelAutomation:
		return stepType == "automation"
	default:
		return appActionTypes[stepType]
	}
}

// toolResultsByStep indexes a run's tool_result observations by step key. A
// step with two keeps the LAST, which is the version a retry left.
func toolResultsByStep(observations []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, o := range observations {
		if str(o, "kind") != "tool_result" {
			continue
		}
		if key := str(o, "stepKey"); key != "" {
			out[key] = o
		}
	}
	return out
}

// observationArgs reads an action's arguments off its tool_result.
//
// `data.args` is the arguments as a JSON STRING (integrations/work encodes
// them), whole up to the observation ceiling. Past it the string is cut
// mid-document and `argsTruncated` says so: that call is NOT reproducible from
// the row, and reproducible reports false. A value that is not a JSON object
// is kept as {"_raw": value} -- what the app sent, in the one shape a template
// can hold -- rather than dropped.
func observationArgs(o map[string]any) (map[string]any, bool) {
	data := obj(o, "data")
	if truncated, _ := data["argsTruncated"].(bool); truncated {
		return nil, false
	}
	switch raw := data["args"].(type) {
	case nil:
		return map[string]any{}, true
	case map[string]any:
		return raw, true
	case string:
		if strings.TrimSpace(raw) == "" {
			return map[string]any{}, true
		}
		var decoded any
		if err := json.Unmarshal([]byte(raw), &decoded); err == nil {
			if m, ok := decoded.(map[string]any); ok {
				return m, true
			}
		}
		return map[string]any{"_raw": raw}, true
	default:
		return map[string]any{"_raw": raw}, true
	}
}

// stepEvidenceOf builds what one action reported, in the executor-independent
// terms a replay is compared on (component/work.StepObservation).
//
// Each tool carries only what a REPLAY of it can report, because an
// expectation holding an observable the replaying executor never reports is a
// step that can never match -- an absent measurement is never a match:
//
//	exec          error flag, exit code, result type
//	fetch, mcp    error flag, result type
//	fs_write      error flag, the written file's content digest
//	fs_read       error flag, the read file's content digest
//
// A command's file effects are deliberately not expected even when the app
// reported contents: a dispatcher runs a command and sees its exit code and
// its output, not the files it touched.
func (i *Integration) stepEvidenceOf(ctx context.Context, stepType string, o, args map[string]any, workspace string) stepEvidence {
	data := obj(o, "data")
	ev := stepEvidence{StepType: stepType, Workspace: workspace}
	if b, ok := data["isError"].(bool); ok {
		ev.Observation.IsError = &b
	}
	argPaths := pathsInArgs(args)
	ev.Paths = append(ev.Paths, argPaths...)
	switch stepType {
	case "exec":
		ev.Observation.ExitCode = optInt(data, "exitCode")
		ev.Observation.ResultType = str(data, "resultType")
	case "fetch", "mcp":
		ev.Observation.ResultType = str(data, "resultType")
	case "fs_write", "fs_read":
		op := "write"
		if stepType == "fs_read" {
			op = "read"
		}
		contents, paths := i.contentDigests(ctx, op, data, argPaths, workspace)
		ev.Observation.Contents = contents
		ev.Paths = append(ev.Paths, paths...)
	}
	return ev
}

// contentDigests resolves an action's recorded contents to (op, path, digest).
//
// A stored content is a Library file the observation references by id, and
// its digest is the file row's `sha256` -- the bytes as the action left them,
// which any executor can hash again. The row keeps only a NAME, so the path is
// recovered from it (component/worker.ContentBaseName) and then preferred from
// the action's own arguments: a content-addressed file may have been filed by
// an EARLIER session under another name, and the argument is what THIS action
// said. A content that was not stored is still named in `contentOmitted`, with
// its digest when one was measured.
//
// Paths are written relative to the recording's workspace when they were
// inside it, which is what makes an expectation comparable with a replay in a
// different workspace. The second result is every path as recorded, for the
// footprint.
func (i *Integration) contentDigests(ctx context.Context, op string, data map[string]any, argPaths []string, workspace string) ([]work.ContentDigest, []string) {
	var (
		out   []work.ContentDigest
		paths []string
	)
	sessionId, actionId := str(data, "sessionId"), str(data, "appActionId")
	for _, fileId := range stringList(data["contentRefs"]) {
		name, digest := i.libraryFile(ctx, fileId)
		p := ""
		if base, ok := workerservice.ContentBaseName(name, sessionId, actionId); ok {
			p = base
		}
		p = preferArgPath(p, argPaths)
		if p == "" {
			continue
		}
		paths = append(paths, p)
		out = append(out, work.ContentDigest{Op: op, Path: workspaceRelative(workspace, p), Digest: digest})
	}
	for _, entry := range stringList(data["contentOmitted"]) {
		p, digest := parseOmitted(entry)
		if p == "" {
			continue
		}
		paths = append(paths, p)
		out = append(out, work.ContentDigest{Op: op, Path: workspaceRelative(workspace, p), Digest: digest})
	}
	return out, paths
}

// libraryFile reads a recorded content's Library row under the owner's actor:
// its name and its digest, either of which may be empty.
func (i *Integration) libraryFile(ctx context.Context, fileId string) (string, string) {
	rows, err := i.store.query(ctx, "query "+call("libraryFileById", map[string]any{"fileId": fileId}))
	if err != nil || len(rows) == 0 {
		i.log().Debug("procedure: a recorded content's Library row is not readable", "fileId", fileId, "err", err)
		return "", ""
	}
	return str(rows[0], "name"), strings.ToLower(strings.TrimSpace(str(rows[0], "sha256")))
}

// preferArgPath picks the path THIS action named for a content: the argument
// path whose base name is the recovered one, or -- when the action named
// exactly one path -- that one, whatever the stored row is called.
func preferArgPath(base string, argPaths []string) string {
	for _, p := range argPaths {
		if base != "" && path.Base(p) == base {
			return p
		}
	}
	if len(argPaths) == 1 {
		return argPaths[0]
	}
	return base
}

// parseOmitted reads one `contentOmitted` entry: "<path>: <why>", with
// " (sha256 <hex>)" at the end when the digest was measured.
func parseOmitted(entry string) (string, string) {
	p, rest, found := strings.Cut(entry, ": ")
	if !found {
		return "", ""
	}
	digest := ""
	const marker = "(sha256 "
	if i := strings.LastIndex(rest, marker); i >= 0 && strings.HasSuffix(rest, ")") {
		digest = strings.ToLower(strings.TrimSpace(rest[i+len(marker) : len(rest)-1]))
	}
	return strings.TrimSpace(p), digest
}

// pathKeys are the argument names whose string value is a file path, for the
// footprint. Named rather than sniffed: an argument that merely LOOKS like a
// path -- the old_string of an edit that begins with "//" -- is content, and
// reading it as a path would send a portable procedure to somebody's machine.
var pathKeys = map[string]bool{
	"path": true, "file": true, "file_path": true, "filePath": true, "filepath": true,
	"filename": true, "targetPath": true, "target_path": true, "directory": true,
	"dir": true, "cwd": true, "notebook_path": true,
}

// pathsInArgs collects every path-keyed string in an action's arguments,
// nested ones included, sorted.
func pathsInArgs(args map[string]any) []string {
	var out []string
	var walk func(map[string]any)
	walk = func(m map[string]any) {
		for k, v := range m {
			switch t := v.(type) {
			case string:
				if pathKeys[k] && strings.TrimSpace(t) != "" {
					out = append(out, strings.TrimSpace(t))
				}
			case map[string]any:
				walk(t)
			case []any:
				for _, e := range t {
					if sub, ok := e.(map[string]any); ok {
						walk(sub)
					}
				}
			}
		}
	}
	walk(args)
	sort.Strings(out)
	return out
}

// workspaceRelative writes an absolute path inside the workspace relative to
// it; anything else is returned cleaned and as it was.
func workspaceRelative(workspace, p string) string {
	p = strings.TrimSpace(p)
	ws := strings.TrimSpace(workspace)
	if p == "" || !path.IsAbs(p) || !path.IsAbs(ws) {
		return cleanPath(p)
	}
	ws = path.Clean(ws)
	abs := path.Clean(p)
	if ws != "/" && strings.HasPrefix(abs, ws+"/") {
		return strings.TrimPrefix(abs, ws+"/")
	}
	return abs
}

func cleanPath(p string) string {
	if p == "" {
		return ""
	}
	return path.Clean(p)
}

// feedback is what a run's feedback observations say (D21, D23).
type feedback struct {
	// runDisliked: a dislike naming the RUN. The recording leaves the corpus.
	runDisliked bool
	// steps: verdicts naming a STEP, by step key -- the candidate gate's
	// evidence, which holds a procedure at candidate until a disliked step
	// has a liked or neutral version.
	steps map[string][]stepVerdict
}

// readFeedback sorts a run's feedback rows into the run-level verdict and the
// step-level ones. A row names a step through data.target.stepKey (and the
// version through data.target.version); a row naming no step names the run,
// which is also every feedback row epic C wrote.
func readFeedback(observations []map[string]any) feedback {
	fb := feedback{steps: map[string][]stepVerdict{}}
	for idx, o := range observations {
		if str(o, "kind") != "feedback" {
			continue
		}
		data := obj(o, "data")
		verdict := work.ParseVerdict(str(data, "verdict"))
		target := obj(data, "target")
		key := str(target, "stepKey")
		if key == "" {
			if verdict == work.VerdictDislike {
				fb.runDisliked = true
			}
			continue
		}
		created, _ := time.Parse(time.RFC3339Nano, str(o, "createdAt"))
		fb.steps[key] = append(fb.steps[key], stepVerdict{
			Version:   intOf(target, "version"),
			Verdict:   verdict,
			CreatedAt: created,
			Order:     idx,
		})
	}
	return fb
}

// stepVersions folds one step's feedback into component/work's shape: one
// verdict per version, oldest version first, each the NEWEST verdict given on
// that version -- a verdict is never overwritten (D21), so the newest row is
// the person's current judgment of that version.
func stepVersions(rows []stepVerdict) work.StepVersions {
	if len(rows) == 0 {
		return work.StepVersions{}
	}
	newest := map[int]stepVerdict{}
	for _, r := range rows {
		prev, seen := newest[r.Version]
		if !seen || r.CreatedAt.After(prev.CreatedAt) || (r.CreatedAt.Equal(prev.CreatedAt) && r.Order > prev.Order) {
			newest[r.Version] = r
		}
	}
	versions := make([]int, 0, len(newest))
	for v := range newest {
		versions = append(versions, v)
	}
	sort.Ints(versions)
	out := work.StepVersions{Verdicts: make([]work.Verdict, 0, len(versions))}
	for _, v := range versions {
		out.Verdicts = append(out.Verdicts, newest[v].Verdict)
	}
	return out
}

// sortOldestFirst orders run rows by creation, oldest first, id breaking ties
// so two replicas order one corpus identically.
func sortOldestFirst(runs []map[string]any) {
	at := func(r map[string]any) time.Time {
		t, _ := time.Parse(time.RFC3339Nano, str(r, "createdAt"))
		return t
	}
	sort.SliceStable(runs, func(a, b int) bool {
		ta, tb := at(runs[a]), at(runs[b])
		if !ta.Equal(tb) {
			return ta.Before(tb)
		}
		return str(runs[a], "id") < str(runs[b], "id")
	})
}

// markConsumed sets Consumed on every step whose result a LATER step's
// arguments referenced. It is a whole-run question, which is why it cannot
// live in the pure module.
//
// The comparison is over the flattened leaf VALUES of the result against the
// flattened leaf values of every later step's input. A digest-only result --
// which is what a large result degrades to -- is compared as its digest, so a
// step whose result was too big to keep is still recognised as consumed when a
// later argument carries that digest.
func markConsumed(steps []proc.Step) {
	for i := range steps {
		if !pureReadTypes[steps[i].StepType] {
			continue
		}
		values := leafValues(steps[i].ResultValue)
		if steps[i].ResultDigest != "" {
			values = append(values, steps[i].ResultDigest)
		}
		if len(values) == 0 {
			continue
		}
		for j := i + 1; j < len(steps) && !steps[i].Consumed; j++ {
			later := leafValues(steps[j].Input)
			for _, v := range values {
				if v == "" {
					continue
				}
				for _, w := range later {
					if strings.Contains(w, v) {
						steps[i].Consumed = true
						break
					}
				}
			}
		}
	}
}

func leafValues(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		return []string{t}
	case bool:
		return []string{boolString(t)}
	case float64:
		return []string{trimFloat(t)}
	case []any:
		var out []string
		for _, e := range t {
			out = append(out, leafValues(e)...)
		}
		return out
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var out []string
		for _, k := range keys {
			out = append(out, leafValues(t[k])...)
		}
		return out
	default:
		return nil
	}
}

// resultValue unwraps the trimmed result to the part a later argument could
// have come from. The row's `result` is {status, result, error, metadata,
// contentId}; the inner `result` is the payload, and a data-flow reference
// into `status` would be an explanation nobody meant.
func resultValue(result map[string]any) any {
	if result == nil {
		return nil
	}
	if inner, ok := result["result"]; ok {
		return inner
	}
	return nil
}

// footprintDigest folds an observed footprint into a comparable string. Two
// actions with the same tool and arguments but different effects are not the
// same action, and this is what makes that visible to Symbolize.
func footprintDigest(fp map[string]any) string {
	if len(fp) == 0 {
		return ""
	}
	return strings.Join(leafValues(fp), "\x1f")
}

// intOf narrows a decoded payload number. The fields it reads are a step's
// `seq` -- its POSITION, which is an ordering -- and a feedback row's version,
// which orders versions. So an out-of-range value saturates rather than
// becoming zero: a step that claimed position 0 would sort to the front of a
// run it belongs at the end of, and the procedure mined from it would have its
// steps in the wrong order. core/num names that answer.
func intOf(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return num.ClampFloat64(v)
	case int:
		return v
	case int64:
		return num.ClampInt64(v)
	default:
		return 0
	}
}

// optInt reads an OPTIONAL whole number -- an exit code -- keeping absent
// absent. A value that is not a whole number is not an exit code at all, and
// is reported as unmeasured rather than rounded into one; an out-of-range one
// saturates, because it is compared for equality and no recorded exit code
// is that large.
func optInt(m map[string]any, key string) *int {
	var n int
	switch v := m[key].(type) {
	case float64:
		if v != float64(num.ClampFloat64(v)) {
			return nil
		}
		n = num.ClampFloat64(v)
	case int:
		n = v
	case int64:
		n = num.ClampInt64(v)
	case json.Number:
		parsed, err := strconv.ParseInt(v.String(), 10, 64)
		if err != nil {
			return nil
		}
		n = num.ClampInt64(parsed)
	default:
		return nil
	}
	return &n
}

// stringList reads a decoded list of strings, skipping anything else.
func stringList(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// trimFloat writes a decoded JSON number the way canonicalization does, so a
// value that arrived as 1 in a result and 1 in an argument compares equal.
func trimFloat(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}
