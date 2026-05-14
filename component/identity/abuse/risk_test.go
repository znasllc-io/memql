package abuse

import "testing"

func TestScoreDisposablePlusMXFailExceedsThreshold(t *testing.T) {
	in := ScoreInput{
		Email:         "abc123@mailinator.com",
		UserAgent:     "Mozilla/5.0 (Macintosh; Intel Mac OS X) AppleWebKit/605.1.15",
		DisposableHit: true,
		MXFailed:      true,
	}
	got := Score(in)
	want := WeightDisposableHit + WeightMXFail
	if got.Score != want {
		t.Errorf("Score = %d, want %d", got.Score, want)
	}
	if got.Score < 50 {
		t.Errorf("expected disposable+MX-fail to be >= default threshold (50), got %d", got.Score)
	}
}

func TestScoreCleanRequest(t *testing.T) {
	in := ScoreInput{
		Email:     "jane@example.com",
		UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X) AppleWebKit/605.1.15 Safari/605.1.15",
		HourOfDay: 14,
	}
	got := Score(in)
	if got.Score != 0 {
		t.Errorf("clean request scored %d, want 0 (signals: %v)", got.Score, got.Signals)
	}
}

func TestScoreAutomationUA(t *testing.T) {
	in := ScoreInput{
		Email:     "jane@example.com",
		UserAgent: "curl/7.88.1",
		HourOfDay: 14,
	}
	got := Score(in)
	if got.Score != WeightUAAutomation {
		t.Errorf("Score = %d, want %d", got.Score, WeightUAAutomation)
	}
}

func TestScoreEmptyUAFlagsAsAutomation(t *testing.T) {
	in := ScoreInput{Email: "jane@example.com"}
	got := Score(in)
	if got.Score < WeightUAAutomation {
		t.Errorf("empty UA should contribute automation weight, got %d", got.Score)
	}
}

func TestScoreRandomLookingEmail(t *testing.T) {
	in := ScoreInput{
		Email:     "abc7q3l1m9z2k8x4@example.com",
		UserAgent: "Mozilla/5.0",
		HourOfDay: 14,
	}
	got := Score(in)
	if got.Score < WeightEmailRandomLooking {
		t.Errorf("random-looking local part should contribute, got %d", got.Score)
	}
}

func TestScoreOddHour(t *testing.T) {
	in := ScoreInput{
		Email:     "jane@example.com",
		UserAgent: "Mozilla/5.0",
		HourOfDay: 4,
	}
	got := Score(in)
	if got.Score != WeightTimeOfDayOdd {
		t.Errorf("Score = %d, want %d", got.Score, WeightTimeOfDayOdd)
	}
}

func TestScoreHighIPVelocity(t *testing.T) {
	in := ScoreInput{
		Email:      "jane@example.com",
		UserAgent:  "Mozilla/5.0",
		HourOfDay:  14,
		IPVelocity: 5,
	}
	got := Score(in)
	if got.Score != WeightIPVelocityHigh {
		t.Errorf("Score = %d, want %d", got.Score, WeightIPVelocityHigh)
	}
}
