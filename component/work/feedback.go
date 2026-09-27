package work

// feedback.go -- a person's verdict on the framework's three axes, and the
// validator's pre-filter beside it (epic memql#5414; design record
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md,
// D21 and D22).
//
// THE VOCABULARY IS THE AI FLUENCY FRAMEWORK'S DISCERNMENT. A like, a dislike
// or neutral; and a dislike asks one question before it is saved -- was the
// PRODUCT wrong (what it produced), the PROCESS (how it went about it) or the
// PERFORMANCE (how it behaved)? The question is the point: a dislike that
// names no axis says only that something was wrong, and nothing downstream can
// act on that -- not a re-run's guidance, not the goal's description guidance.
//
// THE VALIDATOR IS A PRE-FILTER, NEVER A CERTIFIER (D22). It applies the same
// three axes before a person looks, and a disagreement between it and the
// person is KEPT, because it is the signal that the validator's prompt needs
// work. Nothing here lets its verdict count as a like: verdict.go's candidate
// gate reads feedback observations only, and the validator writes a decision.

import (
	"errors"
	"fmt"
	"strings"
)

// maxFeedbackReasonBytes bounds a person's reason. It rides as guidance into
// later prompts, so it is kept to a paragraph.
const maxFeedbackReasonBytes = 2000

// Feedback refusals.
var (
	// ErrFeedbackAxisRequired: a dislike that names no axis.
	ErrFeedbackAxisRequired = errors.New("feedback_axis_required: say what was wrong -- the result, the approach or the behaviour")
	// ErrFeedbackVerdictInvalid: not like, dislike or neutral.
	ErrFeedbackVerdictInvalid = errors.New("feedback_verdict_invalid: a verdict is like, dislike or neutral")
	// ErrFeedbackReasonTooLong: a reason past the bound.
	ErrFeedbackReasonTooLong = errors.New("feedback_reason_too_long: keep the reason to a paragraph")
)

// Axes are the framework's three kinds of discernment. A true axis is a
// problem found on it -- for a person's dislike and for the validator alike.
type Axes struct {
	Product     bool `json:"product"`
	Process     bool `json:"process"`
	Performance bool `json:"performance"`
}

// Any reports whether any axis is set.
func (a Axes) Any() bool { return a.Product || a.Process || a.Performance }

// Names are the set axes by name, always in product, process, performance
// order, so a sentence built from them reads the same every time.
func (a Axes) Names() []string {
	var out []string
	if a.Product {
		out = append(out, "product")
	}
	if a.Process {
		out = append(out, "process")
	}
	if a.Performance {
		out = append(out, "performance")
	}
	return out
}

// Object is the stored form: all three keys, always.
func (a Axes) Object() map[string]any {
	return map[string]any{"product": a.Product, "process": a.Process, "performance": a.Performance}
}

// ParseAxes reads stored axes. A key that is absent or not a boolean is false.
func ParseAxes(v any) Axes {
	m, _ := v.(map[string]any)
	b := func(k string) bool { x, _ := m[k].(bool); return x }
	return Axes{Product: b("product"), Process: b("process"), Performance: b("performance")}
}

// AxesFromNames builds axes from names, ignoring any it does not know.
func AxesFromNames(names []string) Axes {
	var a Axes
	for _, n := range names {
		switch strings.ToLower(strings.TrimSpace(n)) {
		case "product":
			a.Product = true
		case "process":
			a.Process = true
		case "performance":
			a.Performance = true
		}
	}
	return a
}

// ValidateFeedback refuses a verdict the record cannot keep: anything but
// like, dislike or neutral; a dislike with no axis (D21 -- the question is the
// point); a reason past a paragraph.
func ValidateFeedback(v Verdict, axes Axes, reason string) error {
	switch v {
	case VerdictLike, VerdictNeutral:
	case VerdictDislike:
		if !axes.Any() {
			return ErrFeedbackAxisRequired
		}
	default:
		return ErrFeedbackVerdictInvalid
	}
	if len(reason) > maxFeedbackReasonBytes {
		return ErrFeedbackReasonTooLong
	}
	return nil
}

// ValidatorVerdict is the answer validator's judgement of one step version.
type ValidatorVerdict struct {
	Axes   Axes   `json:"axes"`
	Reason string `json:"reason"`
}

// Flagged reports whether the validator found a problem on any axis.
func (v ValidatorVerdict) Flagged() bool { return v.Axes.Any() }

// Word is the verdict as stored: pass or flag.
func (v ValidatorVerdict) Word() string {
	if v.Flagged() {
		return "flag"
	}
	return "pass"
}

// Disagrees reports whether a person's verdict contradicts the validator's: a
// like on an answer it flagged, or a dislike on one it passed. Neutral never
// disagrees -- it says the person looked and did not object, which is
// compatible with either.
func Disagrees(person Verdict, validator ValidatorVerdict) bool {
	switch person {
	case VerdictLike:
		return validator.Flagged()
	case VerdictDislike:
		return !validator.Flagged()
	}
	return false
}

// FeedbackContent is the one sentence a feedback observation carries -- its
// embedding source, so recall can find "the time the totals were wrong".
// stepKey empty means the verdict is on the whole run.
func FeedbackContent(v Verdict, stepKey string, version int, axes Axes, reason string) string {
	var what string
	switch v {
	case VerdictLike:
		what = "Liked"
	case VerdictDislike:
		what = "Disliked"
	case VerdictNeutral:
		what = "Marked neutral"
	default:
		what = "Judged"
	}
	target := "the run"
	if stepKey != "" {
		target = stepKey
		if version > 0 {
			target = fmt.Sprintf("version %d of %s", version, stepKey)
		}
	}
	s := what + " " + target
	if names := axes.Names(); v == VerdictDislike && len(names) > 0 {
		s += " (" + strings.Join(names, ", ") + ")"
	}
	if r := strings.TrimSpace(reason); r != "" {
		s += ": " + r
	}
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}
