package edge

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A healthy origin does not compensate for an expiring EDGE connection. This
// crosses a real TCP proxy hop through the recording writer used in production.
func TestArtifactDownloadSurvivesProxyWriteDeadline(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/artifacts/owned-file/content" || r.Header.Get("Authorization") != "Bearer fixture" {
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
		Resolver: staticResolver{site: &Site{ID: "s1", Hostname: "shop.example.com", Status: "live", Kind: "spa", APIProxy: true}},
		Opener:   mapOpener(map[string]string{"index.html": "ROOT"}), APITarget: upstream.URL,
	})
	edge := httptest.NewUnstartedServer(h)
	edge.Config.WriteTimeout = 20 * time.Millisecond
	edge.Start()
	defer edge.Close()
	r, _ := http.NewRequest(http.MethodGet, edge.URL+"/_memql/artifacts/owned-file/content", nil)
	r.Host = "shop.example.com"
	r.Header.Set("Authorization", "Bearer fixture")
	response, err := edge.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "first-last" {
		t.Fatalf("proxy truncated an active authorized download: status=%d body=%q error=%v", response.StatusCode, body, err)
	}
}
