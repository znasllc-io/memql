package library

import (
	"context"
	"encoding/json"
	"errors"
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
		{"forbidden", "refused", "text/html", 403, false},
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
			if !tc.ok {
				var unavailable *imageSourceUnavailable
				if !errors.As(err, &unavailable) {
					t.Fatalf("remote failure misclassified: %v", err)
				}
			}
			if tc.ok && string(got) != tc.body {
				t.Fatal("image bytes changed")
			}
		})
	}
}

func TestRevisionImageReplacementBounds(t *testing.T) {
	spec := revisionImageSpec{Mode: "generate", Name: "portrait.png", Alt: "Portrait", Prompt: "Portrait"}
	for _, tc := range []struct {
		images []revisionImageSpec
		mode   string
	}{
		{nil, "import"}, {[]revisionImageSpec{spec, spec}, "generate"}, {[]revisionImageSpec{spec}, "import"},
	} {
		raw, _ := json.Marshal(map[string]any{"images": tc.images, "limitations": ""})
		if _, err := (&Integration{}).handleRevisionImagePlan(t.Context(), map[string]any{"response": string(raw), "minImages": 1, "maxImages": 1, "mode": tc.mode}, 0); err == nil {
			t.Fatal("replacement silently omitted, expanded or changed acquisition mode")
		}
	}
}

func TestRevisionImageUsage(t *testing.T) {
	image := reviewAttachment{ArtifactID: "portrait-1", Name: "Portrait (1).jpg", MIME: "image/jpeg", URI: "../portrait-1/Portrait%20%281%29.jpg"}
	prepared := reviewAttachment{ArtifactID: "figure-2", Name: "Figure.png", MIME: "image/png", URI: "../figure-2/Figure.png"}
	captured := map[string]any{"comments": []map[string]any{{"attachments": []reviewAttachment{image, {ArtifactID: "notes", Name: "Evidence.md", MIME: "text/markdown", URI: "../notes/Evidence.md"}}}}}
	for _, tc := range []struct {
		name, content string
		want          bool
	}{
		{"inline", "![Portrait](" + image.URI + ")", true},
		{"reference", "![Portrait][photo]\n\n[photo]: " + image.URI, true},
		{"prose", image.URI, false},
		{"ordinary link", "[Portrait](" + image.URI + ")", false},
		{"inline code", "`![Portrait](" + image.URI + ")`", false},
		{"fenced code", "```md\n![Portrait](" + image.URI + ")\n```", false},
		{"different artifact same name", "![Portrait](../portrait-9/Portrait%20%281%29.jpg)", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := tc.content + "\n\n![Figure](" + prepared.URI + ")"
			raw, err := revisionImageUsage(captured, content, []preparedRevisionImage{{Reference: prepared}})
			var usage []map[string]any
			wantCount := 2
			if tc.name == "different artifact same name" {
				wantCount = 3
			}
			if err != nil || json.Unmarshal([]byte(raw), &usage) != nil || len(usage) != wantCount {
				t.Fatalf("image-only usage facts: %s (%v)", raw, err)
			}
			if usage[0]["embedded"] != tc.want || usage[0]["uri"] != image.URI || usage[0]["source"] != "attachment" || usage[1]["embedded"] != true || usage[1]["source"] != "prepared" {
				t.Fatalf("wrong reference comparison: %s", raw)
			}
			if wantCount == 3 && (usage[2]["uri"] != "../portrait-9/Portrait%20%281%29.jpg" || usage[2]["source"] != "candidate" || usage[2]["embedded"] != true) {
				t.Fatalf("saved replacement omitted or confused with old attachment: %s", raw)
			}
		})
	}
	if raw, err := revisionImageUsage(nil, "No images.", nil); err != nil || raw != "[]" {
		t.Fatalf("empty facts must be a JSON array: %s (%v)", raw, err)
	}
}

func TestRevisionImageUsageIncludesOnlyActualUnattachedImages(t *testing.T) {
	content := "![One][saved]\n\n[saved]: ../saved/portrait.jpg\n\n![Again](../saved/portrait.jpg)\n\n![Two](https://example.com/image.png)\n\n" +
		"[Link](../linked/portrait.jpg)\n\n`![Code](../code/portrait.jpg)`\n\n```md\n![Example](../example/portrait.jpg)\n```"
	want := `[{"embedded":true,"source":"candidate","uri":"../saved/portrait.jpg"},{"embedded":true,"source":"candidate","uri":"https://example.com/image.png"}]`
	for range 10 {
		got, err := revisionImageUsage(nil, content, nil)
		if err != nil || got != want {
			t.Fatalf("non-deterministic or incomplete parsed inventory: %s (%v)", got, err)
		}
	}
}
