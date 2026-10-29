package githubapp

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func pipelineCheckRow(id int64, external string) map[string]any {
	r := queueRun(id, "MemQL / product", 42, "queued", "")
	r["external_id"] = external
	return r
}

func TestFindPipelineCheckRecoversEveryOwnedStateWithoutWriting(t *testing.T) {
	for _, status := range []string{"queued", "in_progress", "completed"} {
		t.Run(status, func(t *testing.T) {
			now := time.Now().UTC()
			hub := newHub()
			row := pipelineCheckRow(71, "native-run")
			row["status"] = status
			// A failed completed check is still the same effect receipt, never success.
			if status == "completed" {
				row["conclusion"] = "failure"
			}
			hub.on("/repos/acme/widget/commits/"+headSHA+"/check-runs", func(r *http.Request) (int, string) {
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+installToken {
					t.Fatal("observation changed method or credential")
				}
				q := r.URL.Query()
				if q.Get("filter") != "all" || q.Get("check_name") != "MemQL / product" || q.Get("per_page") != "100" {
					t.Fatal("missing complete exact-name query", q)
				}
				return 200, queueJSON(map[string]any{"total_count": 1, "check_runs": []any{row}})
			})
			got, err := testClient(t, hub, &now).FindPipelineCheck(t.Context(), installToken, "acme", "widget", headSHA, "MemQL / product", "native-run", 42)
			if err != nil || got != 71 || len(hub.seen()) != 1 {
				t.Fatalf("got %d %v requests%d", got, err, len(hub.seen()))
			}
		})
	}
}

func TestFindPipelineCheckRequiresCompleteUnambiguousHistory(t *testing.T) {
	for _, name := range []string{"absent", "foreign App", "foreign external ID", "missing total", "null list", "too large", "short page", "wrong head", "wrong name", "missing App", "duplicate identity", "duplicate ID", "unknown status", "provider unavailable"} {
		t.Run(name, func(t *testing.T) {
			now := time.Now().UTC()
			hub := newHub()
			row := pipelineCheckRow(71, "native-run")
			out := map[string]any{"total_count": 1, "check_runs": []any{row}}
			wantError := true
			status := 200
			switch name {
			case "absent":
				out["total_count"] = 0
				out["check_runs"] = []any{}
				wantError = false
			case "foreign App":
				row["app"] = map[string]any{"id": 99}
				wantError = false
			case "foreign external ID":
				row["external_id"] = "another-run"
				wantError = false
			case "missing total":
				delete(out, "total_count")
			case "null list":
				out["check_runs"] = nil
			case "too large":
				out["total_count"] = 1001
			case "short page":
				out["total_count"] = 2
			case "wrong head":
				row["head_sha"] = strings.Repeat("a", 40)
			case "wrong name":
				row["name"] = "MemQL / other"
			case "missing App":
				delete(row, "app")
			case "duplicate identity":
				out["total_count"] = 2
				out["check_runs"] = []any{row, pipelineCheckRow(72, "native-run")}
			case "duplicate ID":
				out["total_count"] = 2
				out["check_runs"] = []any{row, row}
			case "unknown status":
				row["status"] = "invented"
			case "provider unavailable":
				status = 503
			}
			hub.json("/repos/acme/widget/commits/"+headSHA+"/check-runs", status, queueJSON(out))
			got, err := testClient(t, hub, &now).FindPipelineCheck(t.Context(), installToken, "acme", "widget", headSHA, "MemQL / product", "native-run", 42)
			if (err != nil) != wantError || got != 0 {
				t.Fatalf("got %d %v wantError%v", got, err, wantError)
			}
			if err != nil && strings.Contains(err.Error(), installToken) {
				t.Fatal("credential leaked in error")
			}
			for _, r := range hub.seen() {
				if r.Method != http.MethodGet {
					t.Fatal("observer wrote", r.Method)
				}
			}
		})
	}
}

func TestFindPipelineCheckReadsLaterPagesEvenAfterFindingMatch(t *testing.T) {
	for _, change := range []string{"later match", "earlier match", "total changed", "repeated row", "second match"} {
		t.Run(change, func(t *testing.T) {
			now := time.Now().UTC()
			hub := newHub()
			hub.on("/repos/acme/widget/commits/"+headSHA+"/check-runs", func(r *http.Request) (int, string) {
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				total := 101
				rows := []any{}
				if page == 1 {
					for id := int64(1); id <= 100; id++ {
						external := fmt.Sprintf("other-%d", id)
						if id == 1 && change != "later match" {
							external = "native-run"
						}
						rows = append(rows, pipelineCheckRow(id, external))
					}
				} else if page == 2 {
					external := "other-101"
					if change == "later match" || change == "second match" {
						external = "native-run"
					}
					id := int64(101)
					if change == "repeated row" {
						id = 1
					}
					rows = append(rows, pipelineCheckRow(id, external))
					if change == "total changed" {
						total = 100
					}
				} else {
					t.Fatal("unbounded pagination", page)
				}
				return 200, queueJSON(map[string]any{"total_count": total, "check_runs": rows})
			})
			got, err := testClient(t, hub, &now).FindPipelineCheck(t.Context(), installToken, "acme", "widget", headSHA, "MemQL / product", "native-run", 42)
			want := int64(0)
			if change == "later match" {
				want = 101
			}
			if change == "earlier match" {
				want = 1
			}
			if (err == nil) != (want != 0) || got != want || len(hub.seen()) != 2 {
				t.Fatalf("got%d %v calls%d", got, err, len(hub.seen()))
			}
		})
	}
}

func TestFindPipelineCheckRejectsUnboundRequestBeforeRead(t *testing.T) {
	now := time.Now().UTC()
	hub := newHub()
	c := testClient(t, hub, &now)
	for _, tc := range []struct {
		head, name, external string
		app                  int64
	}{{"short", "name", "id", 42}, {headSHA, "", "id", 42}, {headSHA, "name", "", 42}, {headSHA, "name", "id", 0}} {
		if _, err := c.FindPipelineCheck(context.Background(), installToken, "acme", "widget", tc.head, tc.name, tc.external, tc.app); err == nil {
			t.Fatal("unbound request accepted", tc)
		}
	}
	if len(hub.seen()) != 0 {
		t.Fatal("unbound request reached provider")
	}
}

func TestFindPipelineCheckPreservesAcceptedLongCheckName(t *testing.T) {
	now := time.Now().UTC()
	hub := newHub()
	name := "MemQL / " + strings.Repeat("product", 30)
	row := pipelineCheckRow(71, "native-run")
	row["name"] = name
	hub.on("/repos/acme/widget/commits/"+headSHA+"/check-runs", func(r *http.Request) (int, string) {
		if r.Method != http.MethodGet || r.URL.Query().Get("check_name") != name {
			t.Fatal("observer truncated or changed accepted check name")
		}
		return 200, queueJSON(map[string]any{"total_count": 1, "check_runs": []any{row}})
	})
	got, err := testClient(t, hub, &now).FindPipelineCheck(t.Context(), installToken, "acme", "widget", headSHA, name, "native-run", 42)
	if err != nil || got != 71 {
		t.Fatalf("got %d %v", got, err)
	}
}

func TestPullRequestBaseIdentityIsNeverInferred(t *testing.T) {
	for _, base := range []string{`{"sha":"base","ref":"release","repo":{"full_name":"acme/widget"}}`, `{"sha":"base","ref":"release","repo":null}`, `{}`} {
		now := time.Now().UTC()
		hub := newHub().json("/repos/acme/widget/pulls/7", 200, `{"number":7,"state":"open","head":{"sha":"head","ref":"feature","repo":{"full_name":"acme/widget"}},"base":`+base+`}`)
		got, err := testClient(t, hub, &now).PullRequest(t.Context(), installToken, "acme", "widget", 7)
		if err != nil {
			t.Fatal(err)
		}
		if base == `{}` {
			if got.BaseRef != "" || got.BaseRepository != "" {
				t.Fatal("invented base identity", got)
			}
		} else if got.BaseRef != "release" {
			t.Fatal("lost retargeted branch", got)
		}
		if strings.Contains(base, `null`) && got.BaseRepository != "" {
			t.Fatal("invented base repository", got)
		}
	}
}
