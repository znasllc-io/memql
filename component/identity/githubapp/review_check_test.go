package githubapp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

type reviewCheckFixture struct {
	*queueFixture
	report            map[string]any
	writes            int
	listFail, getFail bool
	writeLost         bool
}

func newReviewCheckFixture(t *testing.T) *reviewCheckFixture {
	f := &reviewCheckFixture{queueFixture: newQueueFixture(t)}
	f.hub.on("/repos/acme/widget/commits/"+headSHA+"/check-runs", func(r *http.Request) (int, string) {
		if f.listFail {
			return 503, `{}`
		}
		if r.URL.Query().Get("filter") != "all" || r.URL.Query().Get("check_name") != "MemQL / review" {
			t.Fatal("report lookup lost exact history/name", r.URL)
		}
		runs := []any{}
		if f.report != nil {
			runs = append(runs, f.report)
		}
		return 200, queueJSON(map[string]any{"total_count": len(runs), "check_runs": runs})
	})
	f.hub.on("/repos/acme/widget/check-runs", func(r *http.Request) (int, string) {
		if r.Method != "POST" {
			t.Fatal("unexpected review mutation", r.Method)
		}
		f.writes++
		bodies := f.hub.sentBodies()
		body := sentJSON(t, f.hub, len(bodies)-1)
		if body["name"] != "MemQL / review" || body["head_sha"] != headSHA || body["external_id"] != f.external() || body["status"] != "completed" || body["conclusion"] != "success" {
			t.Fatal("review write identity drift", body)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer fresh-queue-token-") {
			t.Fatal("read credential used for report write")
		}
		f.report = queueRun(71, "MemQL / review", 42, "completed", "success")
		f.report["external_id"] = f.external()
		if f.writeLost {
			return 502, `{}`
		}
		return 201, `{"id":71}`
	})
	f.hub.on("/repos/acme/widget/check-runs/71", func(r *http.Request) (int, string) {
		if r.Method != "GET" {
			t.Fatal("reported check was mutated")
		}
		if f.getFail {
			return 503, `{}`
		}
		return 200, queueJSON(f.report)
	})
	return f
}
func (f *reviewCheckFixture) external() string { return "memql-review:" + strings.Repeat("b", 64) }
func (f *reviewCheckFixture) run() (int64, error) {
	return f.client.ReportReviewCheck(context.Background(), installToken, "acme", "widget", 7, headSHA, "main", "MemQL / review", f.external(), 42, f.authorize)
}
func TestReviewCheckCreateReadbackAndLostResponseRecovery(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "lost response"}[lost], func(t *testing.T) {
			f := newReviewCheckFixture(t)
			f.writeLost = lost
			id, err := f.run()
			if err != nil || id != 71 || f.writes != 1 {
				t.Fatalf("report %d %v writes%d", id, err, f.writes)
			}
			id, err = f.run()
			if err != nil || id != 71 || f.writes != 1 {
				t.Fatalf("recovery duplicated or lost report %d %v writes%d", id, err, f.writes)
			}
		})
	}
}
func TestReviewCheckRefusesUntrustedEvidenceAndUnavailableProvider(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(*reviewCheckFixture)
	}{
		{"foreign app", func(f *reviewCheckFixture) { f.report["app"] = map[string]any{"id": 99}; f.listFail = true }},
		{"wrong head", func(f *reviewCheckFixture) { f.report["head_sha"] = strings.Repeat("c", 40) }},
		{"wrong title", func(f *reviewCheckFixture) { f.report["name"] = "other check" }},
		{"not successful", func(f *reviewCheckFixture) { f.report["conclusion"] = "failure" }},
		{"not completed", func(f *reviewCheckFixture) { f.report["status"] = "in_progress" }},
		{"readback unavailable", func(f *reviewCheckFixture) { f.getFail = true }},
		{"head changed", func(f *reviewCheckFixture) { f.pr["headRefOid"] = strings.Repeat("c", 40) }},
		{"base changed", func(f *reviewCheckFixture) { f.pr["baseRefName"] = "other" }},
		{"closed", func(f *reviewCheckFixture) { f.pr["state"] = "CLOSED" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewCheckFixture(t)
			f.report = queueRun(71, "MemQL / review", 42, "completed", "success")
			f.report["external_id"] = f.external()
			tc.alter(f)
			if _, err := f.run(); err == nil || f.writes != 0 {
				t.Fatalf("untrusted report accepted or changed: %v writes%d", err, f.writes)
			}
		})
	}
}
func TestReviewCheckFreshRevocationAndHeadRacePreventMutation(t *testing.T) {
	t.Run("revoked before write", func(t *testing.T) {
		f := newReviewCheckFixture(t)
		f.authorize = func(context.Context) (string, error) {
			f.grants++
			if f.grants == 2 {
				return "", errors.New("revoked")
			}
			return "writer", nil
		}
		if _, err := f.run(); err == nil || f.writes != 0 {
			t.Fatalf("revoked report wrote %v", err)
		}
	})
	t.Run("head moved after authorization", func(t *testing.T) {
		f := newReviewCheckFixture(t)
		f.beforeSnapshot = func(n int) {
			if n == 2 {
				f.pr["headRefOid"] = strings.Repeat("c", 40)
			}
		}
		if _, err := f.run(); err == nil || f.writes != 0 {
			t.Fatalf("moved head wrote %v", err)
		}
	})
	t.Run("read outage", func(t *testing.T) {
		f := newReviewCheckFixture(t)
		f.listFail = true
		if _, err := f.run(); err == nil || f.writes != 0 {
			t.Fatalf("outage wrote %v", err)
		}
	})
}
func TestReviewCheckExactReadbackCannotBorrowForeignAppOrDigest(t *testing.T) {
	for _, field := range []string{"app", "external_id", "head_sha", "name", "status", "conclusion"} {
		t.Run(field, func(t *testing.T) {
			f := newReviewCheckFixture(t)
			f.report = queueRun(71, "MemQL / review", 42, "completed", "success")
			f.report["external_id"] = f.external()
			if field == "app" {
				f.report[field] = map[string]any{"id": 99}
			} else {
				f.report[field] = "changed"
			}
			if err := f.client.VerifyReviewCheck(context.Background(), installToken, "acme", "widget", 71, headSHA, "MemQL / review", f.external(), 42); err == nil {
				t.Fatal("foreign or changed check accepted")
			}
			f.noWrites(t)
		})
	}
}
