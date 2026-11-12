package edge

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// MemQL OS fetches a finished account export on its own origin through the
// marker. The bff serves the route at its own root, so the marker is stripped,
// and only that exact shape is: the rest of "/_memql/accounts..." keeps the
// ordinary swap.
func TestUpstreamPathMapsOnlyTheAccountExportDownload(t *testing.T) {
	for in, want := range map[string]string{
		"/_memql/accounts/exports/exp-1/content":      "/accounts/exports/exp-1/content",
		"/_memql/accounts/exports/exp-1/content/more": "/memql/accounts/exports/exp-1/content/more",
		"/_memql/accounts/exports/exp-1":              "/memql/accounts/exports/exp-1",
		"/_memql/accounts/exports//content":           "/memql/accounts/exports//content",
		"/_memql/accounts/exports/exp-1/other":        "/memql/accounts/exports/exp-1/other",
		"/_memql/accounts":                            "/memql/accounts",
	} {
		if got := upstreamPath(in); got != want {
			t.Errorf("upstreamPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// A large export outlives the edge's ordinary write deadline while bytes flow,
// the same treatment the Library's content route has.
func TestAccountExportDownloadSurvivesProxyWriteDeadline(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/accounts/exports/exp-1/content" || r.Header.Get("Authorization") != "Bearer fixture" {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Length", "10")
		_, _ = io.WriteString(w, "first-")
		http.NewResponseController(w).Flush()
		time.Sleep(75 * time.Millisecond)
		_, _ = io.WriteString(w, "last")
	}))
	defer upstream.Close()
	h := NewHandler(Options{
		Resolver: staticResolver{site: &Site{ID: "os", Hostname: "os.example.com", Status: "live", Kind: "spa", APIProxy: true}},
		Opener:   mapOpener(map[string]string{"index.html": "ROOT"}), APITarget: upstream.URL,
	})
	edge := httptest.NewUnstartedServer(h)
	edge.Config.WriteTimeout = 20 * time.Millisecond
	edge.Start()
	defer edge.Close()
	r, _ := http.NewRequest(http.MethodGet, edge.URL+"/_memql/accounts/exports/exp-1/content", nil)
	r.Host = "os.example.com"
	r.Header.Set("Authorization", "Bearer fixture")
	response, err := edge.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "first-last" {
		t.Fatalf("proxy truncated an export download: status=%d body=%q error=%v", response.StatusCode, body, err)
	}
}
