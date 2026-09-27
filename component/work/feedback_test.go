package work

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// "A dislike without an axis is refused" (#5416 acceptance) -- the question is
// the point (D21).
func TestADislikeWithoutAnAxisIsRefused(t *testing.T) {
	if err := ValidateFeedback(VerdictDislike, Axes{}, "it is wrong"); !errors.Is(err, ErrFeedbackAxisRequired) {
		t.Fatalf("err = %v, want ErrFeedbackAxisRequired", err)
	}
	if err := ValidateFeedback(VerdictDislike, Axes{Process: true}, ""); err != nil {
		t.Fatalf("a dislike naming an axis needs no reason: %v", err)
	}
}

func TestALikeNeedsNoAxis(t *testing.T) {
	for _, v := range []Verdict{VerdictLike, VerdictNeutral} {
		if err := ValidateFeedback(v, Axes{}, ""); err != nil {
			t.Errorf("%s: %v", v, err)
		}
	}
}

func TestAnUnseenVerdictIsNotAVerdict(t *testing.T) {
	if err := ValidateFeedback(ParseVerdict("meh"), Axes{Product: true}, ""); !errors.Is(err, ErrFeedbackVerdictInvalid) {
		t.Errorf("err = %v, want ErrFeedbackVerdictInvalid", err)
	}
}

func TestAReasonPastAParagraphIsRefused(t *testing.T) {
	if err := ValidateFeedback(VerdictLike, Axes{}, strings.Repeat("r", maxFeedbackReasonBytes+1)); !errors.Is(err, ErrFeedbackReasonTooLong) {
		t.Errorf("err = %v, want ErrFeedbackReasonTooLong", err)
	}
}

func TestNeutralNeverDisagrees(t *testing.T) {
	for _, val := range []ValidatorVerdict{{}, {Axes: Axes{Product: true}}} {
		if Disagrees(VerdictNeutral, val) {
			t.Errorf("neutral disagreed with %+v", val)
		}
	}
}

func TestALikeOnAFlaggedAnswerDisagrees(t *testing.T) {
	if !Disagrees(VerdictLike, ValidatorVerdict{Axes: Axes{Performance: true}}) {
		t.Error("a like on an answer the validator flagged is the disagreement D22 keeps")
	}
	if Disagrees(VerdictLike, ValidatorVerdict{}) {
		t.Error("a like on a passed answer agrees")
	}
}

func TestADislikeOnAPassedAnswerDisagrees(t *testing.T) {
	if !Disagrees(VerdictDislike, ValidatorVerdict{}) {
		t.Error("a dislike on an answer the validator passed is a disagreement")
	}
	if Disagrees(VerdictDislike, ValidatorVerdict{Axes: Axes{Product: true}}) {
		t.Error("a dislike on a flagged answer agrees, whichever axes each named")
	}
}

func TestAxesReadInOneOrder(t *testing.T) {
	a := Axes{Performance: true, Product: true}
	if got, want := a.Names(), []string{"product", "performance"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Names = %v, want %v", got, want)
	}
	if got := ParseAxes(a.Object()); got != a {
		t.Errorf("round trip = %+v", got)
	}
	if got := AxesFromNames([]string{"Process", "nonsense"}); got != (Axes{Process: true}) {
		t.Errorf("AxesFromNames = %+v", got)
	}
	if (ValidatorVerdict{}).Word() != "pass" || (ValidatorVerdict{Axes: Axes{Process: true}}).Word() != "flag" {
		t.Error("the stored verdict words are pass and flag")
	}
}

func TestFeedbackContentIsOneReadableSentence(t *testing.T) {
	cases := []struct {
		v       Verdict
		key     string
		version int
		axes    Axes
		reason  string
		want    string
	}{
		{VerdictDislike, "draft", 2, Axes{Product: true, Process: true}, "the totals are missing", "Disliked version 2 of draft (product, process): the totals are missing."},
		{VerdictLike, "", 0, Axes{}, "", "Liked the run."},
		{VerdictNeutral, "publish", 1, Axes{}, "", "Marked neutral version 1 of publish."},
	}
	for _, c := range cases {
		if got := FeedbackContent(c.v, c.key, c.version, c.axes, c.reason); got != c.want {
			t.Errorf("FeedbackContent = %q, want %q", got, c.want)
		}
	}
}
