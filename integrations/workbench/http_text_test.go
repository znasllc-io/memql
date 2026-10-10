package workbench

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHTTPFetchReadableExcerpt(t *testing.T) {
	html := "<html><head><script>" + strings.Repeat("irrelevant script;", 10000) + "</script><style>.hidden{display:none}</style></head><body><h1>Official guide</h1><p>Winter temperatures &amp; plant selection.</p></body></html>"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html))
	}))
	defer server.Close()
	integration := &Integration{}
	raw := integration.handleHTTPFetch(context.Background(), nil, map[string]any{"url": server.URL})
	if !raw.OK || raw.Payload.(map[string]any)["body"] != html {
		t.Fatal("raw HTTP mode must preserve the response")
	}
	text := integration.handleHTTPFetch(context.Background(), nil, map[string]any{"url": server.URL, "extractText": true})
	body, _ := text.Payload.(map[string]any)["body"].(string)
	if !text.OK || !strings.Contains(body, "Winter temperatures & plant selection.") || strings.Contains(body, "script") || strings.Contains(body, "<") || len(body) > defaultHTTPTextBytes {
		t.Fatalf("readable result: %#v", text)
	}
	if text.Payload.(map[string]any)["sourceBytes"] != len(html) || text.Payload.(map[string]any)["extractedText"] != true || text.Payload.(map[string]any)["truncated"] != false {
		t.Fatalf("source metadata: %#v", text.Payload)
	}
}

func TestHTTPFetchTextBoundsPreserveUnicodeAndSignalOmittedContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strings.Repeat("🌿", defaultHTTPTextBytes)))
	}))
	defer server.Close()
	integration := &Integration{}
	for _, limit := range []int{5, defaultHTTPTextBytes} {
		args := map[string]any{"url": server.URL, "extractText": true}
		if limit == 5 {
			args["maxBytes"] = limit
		}
		result := integration.handleHTTPFetch(context.Background(), nil, args)
		body, _ := result.Payload.(map[string]any)["body"].(string)
		if !result.OK || len(body) > limit || !utf8.ValidString(body) || result.Payload.(map[string]any)["truncated"] != true {
			t.Fatalf("limit %d: %#v", limit, result)
		}
	}
	invalid := integration.handleHTTPFetch(context.Background(), nil, map[string]any{"url": server.URL, "extractText": "yes"})
	if invalid.OK {
		t.Fatal("unsupported extraction values must not silently return raw HTML")
	}
}

func TestHTTPFetchIdentifiesClientAndPreservesSourceRefusals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/restricted" || !strings.Contains(r.UserAgent(), "https://github.com/znasllc-io/memql") {
			http.Error(w, "Source refuses access", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(r.UserAgent()))
	}))
	defer server.Close()
	integration := &Integration{}
	result := integration.handleHTTPFetch(context.Background(), nil, map[string]any{"url": server.URL})
	if !result.OK || !strings.HasPrefix(result.Payload.(map[string]any)["body"].(string), "MemQL/") {
		t.Fatalf("research fetch did not identify MemQL: %#v", result)
	}
	custom := "OperatorResearch/2.0 (https://github.com/znasllc-io/memql)"
	result = integration.handleHTTPFetch(context.Background(), nil, map[string]any{"url": server.URL, "headers": map[string]any{"User-Agent": custom}})
	if !result.OK || result.Payload.(map[string]any)["body"] != custom {
		t.Fatalf("explicit client identity was overwritten: %#v", result)
	}
	result = integration.handleHTTPFetch(context.Background(), nil, map[string]any{"url": server.URL + "/restricted"})
	if result.OK || result.Payload.(map[string]any)["status"] != http.StatusForbidden {
		t.Fatalf("source refusal must remain a refusal: %#v", result)
	}
}
