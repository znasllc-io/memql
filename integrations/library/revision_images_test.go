package library

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestRevisionImageSourceGuards(t *testing.T) {
	for _, source := range []string{"http://example.org/image.png", "https://user:secret@example.org/image.png", "https://127.0.0.1/image.png", "https://[::ffff:127.0.0.1]/image.png", "https://169.254.169.254/image.png", "https://100.64.0.1/image.png", "https://example.org:1234/image.png", "file:///tmp/image.png"} {
		if checkImageURL(source) == nil {
			t.Errorf("accepted %s", source)
		}
	}
	for _, address := range []string{"127.0.0.1", "192.168.1.5", "10.0.0.1", "100.64.1.1", "198.18.1.1", "0.1.1.1", "::1", "fc00::1", "64:ff9b::7f00:1", "2002:7f00:1::"} {
		if imagePublicAddress(net.ParseIP(address)) {
			t.Errorf("accepted private/transition address %s", address)
		}
	}
	if !imagePublicAddress(net.ParseIP("8.8.8.8")) || checkImageURL("https://example.org/image.png?size=512") != nil {
		t.Fatal("public sources refused")
	}
	client := revisionImageClient()
	defer client.CloseIdleConnections()
	private, _ := url.Parse("https://127.0.0.1/image.png")
	if client.CheckRedirect(&http.Request{URL: private}, nil) == nil {
		t.Fatal("private redirect accepted")
	}
	transport := client.Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("ambient proxy enabled")
	}
	if connection, err := transport.DialContext(t.Context(), "tcp", "127.0.0.1:443"); err == nil {
		connection.Close()
		t.Fatal("dial admitted a private DNS/literal target")
	}
}
func TestRevisionImagePlanRejectsUnverifiedSourcesBeforeEffects(t *testing.T) {
	valid := revisionImageSpec{Mode: "generate", Name: "portrait.png", Alt: "Illustrated portrait", Prompt: "An editorial illustration"}
	for name, mutate := range map[string]func(*revisionImageSpec){"path": func(s *revisionImageSpec) { s.Name = "../portrait.png" }, "source claim": func(s *revisionImageSpec) { s.URL = "https://example.org/photo.png" }, "empty alt": func(s *revisionImageSpec) { s.Alt = " " }, "unsupported": func(s *revisionImageSpec) { s.Name = "portrait.svg" }, "missing rights": func(s *revisionImageSpec) {
		s.Mode = "import"
		s.Prompt = ""
		s.URL = "https://example.org/photo.png"
		s.SourceURL = "https://example.org/source"
	}} {
		t.Run(name, func(t *testing.T) {
			spec := valid
			mutate(&spec)
			raw, _ := json.Marshal(map[string]any{"images": []revisionImageSpec{spec}, "limitations": ""})
			if _, err := (&Integration{}).handleRevisionImagePlan(context.Background(), map[string]any{"response": string(raw)}, 0); err == nil {
				t.Fatal("invalid plan admitted")
			}
		})
	}
	raw, _ := json.Marshal(map[string]any{"images": []revisionImageSpec{valid}, "limitations": ""})
	if _, err := (&Integration{}).handleRevisionImagePlan(t.Context(), map[string]any{"response": string(raw)}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Integration{}).handleRevisionImagePlan(t.Context(), map[string]any{"response": string(raw) + " {}"}, 0); err == nil {
		t.Fatal("trailing content admitted")
	}
	if _, err := (&Integration{}).handleRevisionImagePlan(t.Context(), map[string]any{"response": strings.Repeat("x", (128<<10)+1)}, 0); err == nil {
		t.Fatal("oversized plan admitted")
	}
}

type imageFetchTransport func(*http.Request) (*http.Response, error)

func (f imageFetchTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestRevisionImageFetchRejectsPagesAndOversizedBodies(t *testing.T) {
	for _, tc := range []struct {
		name, body, mime string
		status           int
		ok               bool
	}{
		{"image", string(attachmentPNG()), "image/png", 200, true},
		{"page", "<html>not an image</html>", "text/html", 200, false},
		{"missing", "missing", "image/png", 404, false},
		{"large", strings.Repeat("x", (8<<20)+1), "image/png", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: imageFetchTransport(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Fatal("credentials attached")
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": []string{tc.mime}}, Body: io.NopCloser(strings.NewReader(tc.body)), ContentLength: -1}, nil
			})}
			got, err := fetchRevisionImageWithClient(t.Context(), "https://images.example/portrait.png", client)
			if (err == nil) != tc.ok {
				t.Fatalf("result %v", err)
			}
			if tc.ok && string(got) != tc.body {
				t.Fatal("image bytes changed")
			}
		})
	}
}
