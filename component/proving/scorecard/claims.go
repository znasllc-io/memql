package scorecard

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/proving/figure"
)

// A CLAIM is a number in published prose that rests on a scorecard figure.
//
// Design P4: the gate is over MARKED claims. A published numeric claim carries
// an HTML comment naming the metric it rests on, and a Go gate fails the build
// when the scorecard does not carry that metric, when it says `unmeasured`, or
// when the prose and the scorecard disagree.
//
// Marking is the opt-in, and that is what keeps the gate usable. The
// alternative -- scanning every number in README and docs/public -- flags
// version numbers, ports, replica counts and table values, and the exemption
// list becomes the real policy within a release.
//
//	Replaying a run re-executes no completed step.
//	<!-- proving: metric=durability.resumedStepsReExecuted arm=platform value=0 -->
//
// The comment sits on the line AFTER the sentence it is about, which is what
// lets the failure message quote the prose the number is in.
var claimRe = regexp.MustCompile(`<!--\s*proving:\s*([^>]*?)\s*-->`)

// A PENDING claim uses a DIFFERENT marker, and the difference is load-bearing.
//
// The two say opposite things -- "this number is published and must hold" and
// "this number is NOT published and must not yet hold" -- so one spelling for
// both would make the forward gate fail on every row of the pending table,
// which is exactly what it did the first time. Two markers, two parsers, and
// neither can be read as the other.
//
//	<!-- proving-pending: metric=governance.modelCallsJournaled arm=platform value=1 -->
var pendingRe = regexp.MustCompile(`<!--\s*proving-pending:\s*([^>]*?)\s*-->`)

// Claim is one marked claim, with where it was found.
type Claim struct {
	File   string
	Line   int
	Metric figure.Metric
	Arm    figure.Arm
	// Op is how the prose's value relates to the scorecard's median.
	Op ClaimOp
	// Value is the number written in the prose.
	Value float64
	// Prose is the line above the marker, quoted back in a failure so the
	// author can see which sentence is wrong.
	Prose string
	// Raw is the marker's body, for an error about the marker itself.
	Raw string
}

// ClaimOp is how a claim's stated value relates to the measured median.
type ClaimOp string

const (
	// OpEq -- the prose states the number exactly. The default.
	OpEq ClaimOp = "eq"
	// OpLTE -- the prose states a ceiling ("under 5%"). The measured median
	// must be at or below it.
	OpLTE ClaimOp = "lte"
	// OpGTE -- the prose states a floor ("at least 99%").
	OpGTE ClaimOp = "gte"
)

func (o ClaimOp) valid() bool { return o == OpEq || o == OpLTE || o == OpGTE }

// ParseClaims finds every marker in one file's text.
//
// A malformed marker is returned as an ERROR rather than skipped. A marker
// nobody parses is a claim nobody checks, and it looks exactly like a claim
// that passed.
func ParseClaims(file, text string) ([]Claim, []error) {
	return parseWith(claimRe, file, text)
}

// ParsePendingClaims finds the markers in the "does NOT make yet" table. Same
// shape, different marker: see pendingRe.
func ParsePendingClaims(file, text string) ([]Claim, []error) {
	return parseWith(pendingRe, file, text)
}

func parseWith(re *regexp.Regexp, file, text string) ([]Claim, []error) {
	lines := strings.Split(text, "\n")
	var (
		claims []Claim
		errs   []error
	)
	for i, line := range lines {
		m := re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		c := Claim{File: file, Line: i + 1, Op: OpEq, Arm: figure.ArmPlatform, Raw: m[1]}
		if i > 0 {
			c.Prose = strings.TrimSpace(lines[i-1])
		}
		var (
			sawMetric bool
			sawValue  bool
		)
		for _, field := range strings.Fields(m[1]) {
			k, v, ok := strings.Cut(field, "=")
			if !ok {
				errs = append(errs, fmt.Errorf("%s:%d: %q is not `key=value`", file, i+1, field))
				continue
			}
			switch k {
			case "metric":
				c.Metric, sawMetric = figure.Metric(v), true
			case "arm":
				c.Arm = figure.Arm(v)
			case "op":
				c.Op = ClaimOp(v)
			case "value":
				f, err := strconv.ParseFloat(v, 64)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s:%d: value %q is not a number", file, i+1, v))
					continue
				}
				c.Value, sawValue = f, true
			default:
				errs = append(errs, fmt.Errorf("%s:%d: unknown marker key %q (metric, arm, op, value)", file, i+1, k))
			}
		}
		switch {
		case !sawMetric:
			errs = append(errs, fmt.Errorf("%s:%d: the marker names no metric", file, i+1))
			continue
		case !sawValue:
			errs = append(errs, fmt.Errorf("%s:%d: the marker names no value; a claim with no number cannot outlive a number", file, i+1))
			continue
		case !c.Arm.Valid():
			errs = append(errs, fmt.Errorf("%s:%d: arm %q is not platform or baseline", file, i+1, c.Arm))
			continue
		case !c.Op.valid():
			errs = append(errs, fmt.Errorf("%s:%d: op %q is not eq, lte or gte", file, i+1, c.Op))
			continue
		}
		claims = append(claims, c)
	}
	return claims, errs
}

// ClaimFailure is one claim that does not rest on a number.
type ClaimFailure struct {
	Claim  Claim
	Reason string
}

func (f ClaimFailure) Error() string {
	prose := f.Claim.Prose
	if prose == "" {
		prose = "(no prose on the line above the marker)"
	}
	return fmt.Sprintf("%s:%d: %s\n    claim: %s\n    marker: %s",
		f.Claim.File, f.Claim.Line, f.Reason, prose, f.Claim.Raw)
}

// CheckClaims verifies every claim against the scorecard.
//
// haveScorecard false means no scorecard is committed yet. Every marked claim
// then fails, deliberately: a claim resting on a number that does not exist is
// the exact thing this gate is for, and "there is no scorecard yet" is not an
// excuse for publishing one.
func CheckClaims(claims []Claim, s Scorecard, haveScorecard bool) []ClaimFailure {
	var out []ClaimFailure
	for _, c := range claims {
		if !haveScorecard {
			out = append(out, ClaimFailure{c, "no scorecard is committed, so this claim rests on nothing"})
			continue
		}
		if _, ok := figure.MetricSpec(c.Metric); !ok {
			out = append(out, ClaimFailure{c, fmt.Sprintf("%q is not a registered metric", c.Metric)})
			continue
		}
		entries := s.Find(c.Metric, c.Arm)
		if len(entries) == 0 {
			out = append(out, ClaimFailure{c, fmt.Sprintf("the %s scorecard carries no %s figure on the %s arm", s.Date, c.Metric, c.Arm)})
			continue
		}
		// A metric measured by several scenarios has several numbers. EVERY
		// one must support the claim: publishing a number that holds for one
		// scenario and not another is how a true sentence becomes a
		// misleading one.
		for _, e := range entries {
			if !e.Figure.IsMeasured() {
				out = append(out, ClaimFailure{c, fmt.Sprintf("scenario %s reports %s as unmeasured (%s), so the claim rests on an absence",
					e.Scenario, c.Metric, e.Figure.Absent.Sentence())})
				continue
			}
			got := e.Figure.Stat.Median
			if !satisfies(c.Op, got, c.Value) {
				out = append(out, ClaimFailure{c, fmt.Sprintf("the prose says %s %s but scenario %s measured %s",
					opWord(c.Op), trimNumber(c.Value), e.Scenario, e.Figure.Render())})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Claim.File != out[j].Claim.File {
			return out[i].Claim.File < out[j].Claim.File
		}
		return out[i].Claim.Line < out[j].Claim.Line
	})
	return out
}

// satisfies compares with a tolerance, because a median written into prose is
// a rounded number and an exact float comparison would fail on 0.714 against
// 71.4%. The tolerance is relative for anything but zero, where it is
// absolute -- a claim of zero must be exactly zero, which is the whole point
// of the durability family's headline.
func satisfies(op ClaimOp, got, want float64) bool {
	const rel = 0.005
	tol := math.Abs(want) * rel
	switch op {
	case OpLTE:
		return got <= want+tol
	case OpGTE:
		return got >= want-tol
	default:
		if want == 0 {
			return got == 0
		}
		return math.Abs(got-want) <= tol
	}
}

func opWord(o ClaimOp) string {
	switch o {
	case OpLTE:
		return "at most"
	case OpGTE:
		return "at least"
	}
	return "exactly"
}

func trimNumber(v float64) string {
	if v == math.Trunc(v) {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// PendingClaimsTable is the "Claims this page does NOT make yet" table's rows,
// parsed from a page.
//
// The mirror half of the gate matters as much as the forward half: a claim
// that has BECOME measurable and is still listed as unproven is a different
// kind of stale, and the whole value of that table is that it is true.
type PendingClaimsTable struct {
	// Metrics named in the table's rows, via the same marker syntax in the
	// row's cell.
	Pending []Claim
}

// StillPending reports which pending claims now have a measured figure, and
// therefore should have been promoted out of the table.
func StillPending(pending []Claim, s Scorecard, haveScorecard bool) []ClaimFailure {
	if !haveScorecard {
		return nil
	}
	var out []ClaimFailure
	for _, c := range pending {
		for _, e := range s.Find(c.Metric, c.Arm) {
			if e.Figure.IsMeasured() {
				out = append(out, ClaimFailure{c, fmt.Sprintf(
					"this is listed as not-yet-claimed, but the %s scorecard measures it: %s reports %s. Promote the claim onto the page, or say why the number does not settle it",
					s.Date, e.Scenario, e.Figure.Render())})
				break
			}
		}
	}
	return out
}
