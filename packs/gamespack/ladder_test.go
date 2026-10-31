package gamespack

import (
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/work"
)

func TestStateOfNamesAwaitingApproval(t *testing.T) {
	cases := []struct {
		rung work.Rung
		open string
		want string
	}{
		{work.RungShadow, "", "shadow"},
		{work.RungShadow, "promotion-abc", "awaiting_approval"},
		{work.RungCanary, "", "canary"},
		{work.RungTrusted, "", "trusted"},
		{work.RungRetired, "", "retired"},
	}
	for _, c := range cases {
		if got := stateOf(c.rung, c.open); got != c.want {
			t.Errorf("stateOf(%s, %q) = %s, want %s", c.rung, c.open, got, c.want)
		}
	}
}

func TestLadderStateReadsTheStoredRow(t *testing.T) {
	row := map[string]any{
		"rung":                "canary",
		"shadowMatches":       float64(0),
		"canaryMatches":       float64(3),
		"failures":            float64(1),
		"promotionApprovalId": "",
		"distinctBindings":    map[string]any{"1.count": []any{"sha256:a", "sha256:b"}},
		"lastReplayAt":        "2026-10-18T10:00:00Z",
	}
	s := ladderStateOf(row)
	if s.Rung != work.RungCanary || s.CanaryMatches != 3 || s.Failures != 1 || len(s.DistinctBindings["1.count"]) != 2 {
		t.Fatalf("state = %+v", s)
	}
	if !s.LastReplayAt.Equal(time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("lastReplayAt = %v", s.LastReplayAt)
	}
}

// The ladder is component/work.Advance; this pins how the games pack feeds
// it: k distinct values PER HOLE, counted over canonical JSON.
func TestPromotionNeedsFiveMatchesAndTwoValuesOfEveryHole(t *testing.T) {
	holes := []string{"1.count", "2.count"}
	s := work.LadderState{Rung: work.RungShadow}
	p := work.DefaultLadderPolicy()
	at := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	for i, count := range []float64{4, 4, 4, 4, 4} {
		tr := work.Advance(s, work.LadderEvent{Kind: work.EventShadowCompared, At: at, Match: true, FreeParameters: holes,
			Bindings: bindingValues(map[string]any{"1.count": count, "2.count": count})}, p)
		if tr.Propose {
			t.Fatalf("match %d proposed with one value per hole", i+1)
		}
		s = tr.State
	}
	tr := work.Advance(s, work.LadderEvent{Kind: work.EventShadowCompared, At: at, Match: true, FreeParameters: holes,
		Bindings: bindingValues(map[string]any{"1.count": float64(6), "2.count": float64(6)})}, p)
	if !tr.Propose {
		t.Fatalf("six matches across two values of every hole must propose: %s", tr.Reason)
	}
}

func TestBindingValuesKeepTypesApart(t *testing.T) {
	a := bindingValues(map[string]any{"0.count": float64(4)})
	b := bindingValues(map[string]any{"0.count": "4"})
	if a["0.count"] == b["0.count"] {
		t.Fatal(`4 and "4" must count as two bindings`)
	}
}

func TestServeOfFollowsDecideServe(t *testing.T) {
	cases := []struct {
		rung    work.Rung
		serve   string
		standby bool
	}{
		{work.RungTrusted, "skill", false},
		{work.RungCanary, "skill", true},
		{work.RungShadow, "model", false},
		{work.RungRetired, "model", false},
	}
	for _, c := range cases {
		serve, standby, _ := serveOf(c.rung)
		if serve != c.serve || standby != c.standby {
			t.Errorf("serveOf(%s) = %s/%v, want %s/%v", c.rung, serve, standby, c.serve, c.standby)
		}
	}
}

func TestPromotionIDPinsTheDigest(t *testing.T) {
	if promotionIDFor("abcdef0123456789ffff") != "promotion-abcdef0123456789" {
		t.Fatalf("promotion id = %s", promotionIDFor("abcdef0123456789ffff"))
	}
}

// A promotion is open only for a skill in shadow: the rung is read first, so a skill on
// any other rung is never "awaiting approval", whatever its row says about a promotion.
func TestStateOfReadsTheRungBeforeAnyOpenPromotion(t *testing.T) {
	for _, rung := range []work.Rung{work.RungCanary, work.RungTrusted, work.RungRetired} {
		if got := stateOf(rung, "promotion-abc"); got != string(rung) {
			t.Errorf("stateOf(%s, an open promotion) = %s, want %s", rung, got, rung)
		}
	}
}
