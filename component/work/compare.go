package work

// compare.go -- how a replayed step is judged (epic memql#5408, task
// memql#5410; design record
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md,
// D15 "compared, exactly for deterministic actions and by inferred type where
// the successful recordings themselves varied", D16).
//
// ONLY EXECUTOR-INDEPENDENT OBSERVABLES. A recording's result digest is the
// cockpit's -- the exact text, or the canonical JSON, of the app's own tool
// result -- and a workbench replay of the same command never produces it. So a
// step is compared on what any executor can report about it: the error flag,
// the exit code, the inferred JSON type of its result, and the content digests
// of the files it read or wrote. A comparison that included the cockpit's
// digest would fail every replay by construction.
//
// AN ABSENT MEASUREMENT IS NEVER A MATCH (the step concept, D16). Whatever the
// expectation holds, the replay must report; silence is not agreement, and
// reading it as agreement is how a broken executor certifies itself. And a
// step the recordings agreed on NOTHING about cannot be verified at all -- the
// same rule postcondition.go states for a deterministic step with no
// postcondition -- so both comparisons refuse it rather than wave it through.
//
// Two comparisons, one per question:
//
//   Compare        canary and trusted: is this replay what the RECORDINGS did?
//   CompareShadow  shadow: is this replay what the APP did, for this goal,
//                  beside it? Exact on a deterministic step; by type where the
//                  successful recordings themselves varied.
//
// "By type" relaxes the BYTES, never the outcome: the error flag and the exit
// code are held in both modes, because a failed command is never the same
// step as a clean one however much the successful recordings varied elsewhere
// -- and that runs both ways: a step every recording saw fail is held to
// failing, never waved through when a replay of it succeeds.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

// ContentDigest is one file a step read or wrote, identified by its bytes.
type ContentDigest struct {
	// Op is "read" or "write".
	Op string `json:"op"`
	// Path is relative to the workspace. Compared after path.Clean, so
	// "./a.txt" and "a.txt" are one file.
	Path string `json:"path"`
	// Digest is the SHA-256 of the file's bytes as the step left them,
	// lowercase hex. The cockpit's "sha256:<hex>" spelling reads as the same
	// digest. Empty when unknown -- and, in an expectation, when the
	// recordings varied.
	Digest string `json:"digest,omitempty"`
}

// StepObservation is what one execution of a step reported, in
// executor-independent terms. Every field is optional because every executor
// reports less than everything, and an absent field is a fact nobody
// measured, not a zero.
type StepObservation struct {
	// IsError is the executor's own error verdict.
	IsError *bool `json:"isError,omitempty"`
	// ExitCode is a command's exit status. A POINTER because absent and zero
	// are different answers: a clean command exits 0 and a file write has no
	// exit code at all.
	ExitCode *int `json:"exitCode,omitempty"`
	// ResultType is the result's inferred JSON type -- object, array,
	// string, number, boolean or null -- and "" when unknown.
	ResultType string `json:"resultType,omitempty"`
	// Contents are the files the step read or wrote.
	Contents []ContentDigest `json:"contents,omitempty"`
}

// StepExpectation is what every successful recording of one template step
// agreed on. It is stored per step in the construct's procedure payload, so it
// is a VALUE with a stable encoding: its contents are sorted, and an absent
// exit code stays absent through a round trip.
type StepExpectation struct {
	// NoError: every recording reported isError=false.
	NoError bool `json:"noError,omitempty"`
	// Error: every recording reported isError=true. A step every recording
	// saw fail is a step whose FAILURE is the contract -- a lookup answering
	// "not found" before the next step creates the thing -- and a replay in
	// which it succeeded carries on into a state no recording saw.
	Error bool `json:"error,omitempty"`
	// ExitCode: every recording reported this same exit code.
	ExitCode *int `json:"exitCode,omitempty"`
	// ResultType: every recording agreed on this result type.
	ResultType string `json:"resultType,omitempty"`
	// Contents: one entry per (op, path) seen in EVERY recording, its Digest
	// empty where the recordings' bytes varied.
	Contents []ContentDigest `json:"contents,omitempty"`
	// Exact: the recordings agreed on every observable they carried -- a
	// deterministic step, held to the same bytes rather than the same shape.
	Exact bool `json:"exact,omitempty"`
}

// holdsNothing reports whether the expectation demands nothing at all. An
// agreed failure is a demand like any other: a step whose one agreed
// observable is that it failed is verified by failing.
func (e StepExpectation) holdsNothing() bool {
	return !e.NoError && !e.Error && e.ExitCode == nil && e.ResultType == "" && len(e.Contents) == 0
}

// The two references a reason names. Each observable names the one it was
// actually held to: where the app said nothing and the recordings stood in,
// "the app" would be a claim about something the app never reported.
const (
	everyRecording = "every recording"
	theApp         = "the app"
)

// unverifiableStep is the refusal for a step the recordings agreed on nothing
// about.
const unverifiableStep = "the recordings agreed on nothing this step reports, so no replay of it can be verified"

// ExpectationFrom folds the recordings of one template step into what they
// agreed on. No recordings is no expectation, and never an exact one.
func ExpectationFrom(recordings []StepObservation) StepExpectation {
	var exp StepExpectation
	n := len(recordings)
	if n == 0 {
		return exp
	}
	// An observable is AGREED when every recording carried it with one value,
	// and CONSISTENT when it is agreed or no recording carried it at all.
	// Exact is every observable consistent.
	exact := true

	carried, errored := 0, 0
	for _, r := range recordings {
		if r.IsError != nil {
			carried++
			if *r.IsError {
				errored++
			}
		}
	}
	// Agreed one way or the other: clean everywhere is NoError, failed
	// everywhere is Error, and a split or a silence is neither.
	exp.NoError = carried == n && errored == 0
	exp.Error = carried == n && errored == n
	if carried != 0 && (carried != n || (errored != 0 && errored != n)) {
		exact = false
	}

	var code *int
	carried, agreed := 0, true
	for _, r := range recordings {
		if r.ExitCode == nil {
			continue
		}
		carried++
		if code == nil {
			v := *r.ExitCode
			code = &v
		} else if *r.ExitCode != *code {
			agreed = false
		}
	}
	if carried == n && agreed {
		exp.ExitCode = code
	}
	if carried != 0 && (carried != n || !agreed) {
		exact = false
	}

	typ := ""
	carried, agreed = 0, true
	for _, r := range recordings {
		if r.ResultType == "" {
			continue
		}
		carried++
		if typ == "" {
			typ = r.ResultType
		} else if r.ResultType != typ {
			agreed = false
		}
	}
	if carried == n && agreed {
		exp.ResultType = typ
	}
	if carried != 0 && (carried != n || !agreed) {
		exact = false
	}

	indexes := make([]map[contentKey]string, n)
	for i, r := range recordings {
		indexes[i] = contentIndex(r.Contents)
	}
	for i := 1; i < n; i++ {
		for k := range indexes[i] {
			if _, ok := indexes[0][k]; !ok {
				// A file only some recordings touched is not the step's
				// contract, and a step whose effects vary is not exact.
				exact = false
			}
		}
	}
	for _, k := range sortedContentKeys(indexes[0]) {
		digest, inEvery, sameBytes := indexes[0][k], true, true
		for i := 1; i < n; i++ {
			d, ok := indexes[i][k]
			if !ok {
				inEvery = false
				break
			}
			if d != digest {
				sameBytes = false
			}
		}
		if !inEvery {
			exact = false
			continue
		}
		if !sameBytes {
			// The file is the step's contract; its bytes are not.
			digest = ""
			exact = false
		}
		exp.Contents = append(exp.Contents, ContentDigest{Op: k.op, Path: k.path, Digest: digest})
	}

	exp.Exact = exact
	return exp
}

// Compare judges a canary or trusted replay of one step against what every
// recording agreed on. It returns every reason the step does not match, in a
// person's words, and true only when there are none.
func Compare(exp StepExpectation, got StepObservation) (bool, []string) {
	if exp.holdsNothing() {
		return false, []string{unverifiableStep}
	}
	var why []string
	why = append(why, compareAgreedErrorFlag(exp, got.IsError)...)
	why = append(why, compareExitCode(exp.ExitCode, got.ExitCode, everyRecording)...)
	why = append(why, compareResultType(exp.ResultType, got.ResultType, everyRecording)...)
	why = append(why, compareContents(exp.Contents, got.Contents,
		func(_ contentKey, recorded string) string { return recorded }, exp.Exact, everyRecording)...)
	return len(why) == 0, why
}

// CompareShadow judges a shadow replay of one step against the app's own
// execution of it for the same goal, with the same bindings. The APP is the
// reference; the expectation decides how strictly each observable is held:
//
//   - the error flag, the exit code and the result type are held exactly in
//     both modes. Where the app's observation did not report one, the
//     recordings' agreed value stands in -- a failure every recording saw
//     included -- so the app's silence never excuses the replay, and the
//     reason names the recordings, since the app said nothing to differ from.
//   - every file the app read or wrote must be read or written by the replay.
//     Its bytes are held exactly on a deterministic step, and on any step
//     where the recordings agreed on that file's bytes; by presence where they
//     varied.
//   - on a deterministic step, a file only the replay touched is a divergence:
//     a deterministic step that does something extra is not the same step.
func CompareShadow(exp StepExpectation, app, replay StepObservation) (bool, []string) {
	if exp.holdsNothing() {
		return false, []string{unverifiableStep}
	}
	var why []string

	if app.IsError != nil {
		why = append(why, compareErrorFlag(app.IsError, replay.IsError, theApp)...)
	} else {
		why = append(why, compareAgreedErrorFlag(exp, replay.IsError)...)
	}

	if app.ExitCode != nil {
		why = append(why, compareExitCode(app.ExitCode, replay.ExitCode, theApp)...)
	} else {
		why = append(why, compareExitCode(exp.ExitCode, replay.ExitCode, everyRecording)...)
	}

	if app.ResultType != "" {
		why = append(why, compareResultType(app.ResultType, replay.ResultType, theApp)...)
	} else {
		why = append(why, compareResultType(exp.ResultType, replay.ResultType, everyRecording)...)
	}

	recorded := contentIndex(exp.Contents)
	why = append(why, compareContents(app.Contents, replay.Contents, func(k contentKey, appDigest string) string {
		agreedDigest := recorded[k]
		if !exp.Exact && agreedDigest == "" {
			return ""
		}
		if appDigest != "" {
			return appDigest
		}
		return agreedDigest
	}, exp.Exact, theApp)...)
	return len(why) == 0, why
}

// compareAgreedErrorFlag holds got to the error flag every recording agreed
// on, in whichever direction they agreed. The two checks are independent, so
// an expectation claiming both -- which ExpectationFrom never writes -- is one
// no replay can meet, rather than one read whichever way suits the replay.
func compareAgreedErrorFlag(exp StepExpectation, got *bool) []string {
	var why []string
	if exp.NoError {
		clean := false
		why = append(why, compareErrorFlag(&clean, got, everyRecording)...)
	}
	if exp.Error {
		failed := true
		why = append(why, compareErrorFlag(&failed, got, everyRecording)...)
	}
	return why
}

// compareErrorFlag holds got to want, when there is a want.
func compareErrorFlag(want, got *bool, who string) []string {
	switch {
	case want == nil:
		return nil
	case got == nil:
		return []string{"error flag not reported"}
	case *got && !*want:
		return []string{fmt.Sprintf("reported an error where %s had none", who)}
	case !*got && *want:
		return []string{fmt.Sprintf("reported no error where %s had one", who)}
	}
	return nil
}

// compareExitCode holds got to want, when there is a want.
func compareExitCode(want, got *int, who string) []string {
	switch {
	case want == nil:
		return nil
	case got == nil:
		return []string{"exit code not reported"}
	case *got != *want:
		return []string{fmt.Sprintf("exit code %d where %s exited %d", *got, who, *want)}
	}
	return nil
}

// compareResultType holds got to want, when there is a want.
func compareResultType(want, got, who string) []string {
	switch {
	case want == "":
		return nil
	case got == "":
		return []string{"result type not reported"}
	case got != want:
		return []string{fmt.Sprintf("returned %s where %s returned %s", got, who, want)}
	}
	return nil
}

// compareContents holds got's files to the reference set. digestFor answers
// the digest a reference file's bytes are held to, "" for presence only; strict
// also refuses a file only got touched.
func compareContents(ref, got []ContentDigest, digestFor func(k contentKey, refDigest string) string, strict bool, who string) []string {
	refIdx, gotIdx := contentIndex(ref), contentIndex(got)
	var why []string
	for _, k := range sortedContentKeys(refIdx) {
		d, ok := gotIdx[k]
		if !ok {
			why = append(why, fmt.Sprintf("missing the %s of %s that %s made", k.op, k.path, who))
			continue
		}
		want := digestFor(k, refIdx[k])
		switch {
		case want == "":
		case d == "":
			why = append(why, fmt.Sprintf("the %s of %s reported no digest", k.op, k.path))
		case d != want:
			why = append(why, fmt.Sprintf("the %s of %s has content %s where %s had %s", k.op, k.path, short(d), who, short(want)))
		}
	}
	if strict {
		for _, k := range sortedContentKeys(gotIdx) {
			if _, ok := refIdx[k]; !ok {
				why = append(why, fmt.Sprintf("an extra %s of %s that %s never made", k.op, k.path, who))
			}
		}
	}
	return why
}

// contentKey is one file one way: a read and a write of the same path are
// different effects.
type contentKey struct{ op, path string }

// contentIndex maps each (op, cleaned path) to its normalized digest. A step
// that reports one file twice keeps the last digest it MEASURED: every digest
// is over the bytes as the step left them, so a later report with no digest is
// less information, not a different file.
func contentIndex(cs []ContentDigest) map[contentKey]string {
	out := make(map[contentKey]string, len(cs))
	for _, c := range cs {
		k := contentKey{op: c.Op, path: cleanContentPath(c.Path)}
		d := normalizeDigest(c.Digest)
		if prev, seen := out[k]; seen && d == "" {
			d = prev
		}
		out[k] = d
	}
	return out
}

// sortedContentKeys orders keys by path, then op, so every list built from
// them -- a stored expectation, a list of reasons -- reads the same on every
// run.
func sortedContentKeys(idx map[contentKey]string) []contentKey {
	keys := make([]contentKey, 0, len(idx))
	for k := range idx {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].path != keys[j].path {
			return keys[i].path < keys[j].path
		}
		return keys[i].op < keys[j].op
	})
	return keys
}

// cleanContentPath is path.Clean for a path that says something; an empty one
// stays empty rather than becoming ".".
func cleanContentPath(p string) string {
	if strings.TrimSpace(p) == "" {
		return ""
	}
	return path.Clean(p)
}

// normalizeDigest reads a digest in either spelling the tree writes -- the
// Library's lowercase hex and the cockpit's "sha256:<hex>" -- as one value.
func normalizeDigest(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	return strings.TrimPrefix(d, "sha256:")
}

// InferTextType types text the way the cockpit types a text result
// (memql-cockpit internal/worker/harness/result.go, inferTextType): text that
// is exactly one JSON value is typed by what it parses as, and anything else is
// a string. A replay typed by any other rule would disagree with every
// recording of the same output.
func InferTextType(s string) string {
	v, ok := decodeOneJSONValue(s)
	if !ok {
		return "string"
	}
	return jsonTypeName(v)
}

// jsonWhitespace is the whitespace RFC 8259 allows around a value -- and ONLY
// that: strings.TrimSpace would also strip a vertical tab, typing "0\v" as a
// number although it is not JSON at all.
const jsonWhitespace = " \t\n\r"

// decodeOneJSONValue decodes exactly one JSON value, numbers kept as written,
// and refuses a value with anything behind it: "1 2" is not JSON, whatever a
// lenient reader makes of it.
func decodeOneJSONValue(s string) (any, bool) {
	trimmed := strings.Trim(s, jsonWhitespace)
	if trimmed == "" {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return v, true
}

// jsonTypeName names a decoded value's JSON type.
func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number, float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return ""
}
