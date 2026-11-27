package media

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/livekit/protocol/livekit"
)

type ingressContractHTTPClient struct{ requests []*http.Request }

func (c *ingressContractHTTPClient) Do(r *http.Request) (*http.Response, error) {
	c.requests = append(c.requests, r)
	// Empty protobuf is a valid empty response for each generated method. This
	// fixture captures the pinned client's actual paths without network access.
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/protobuf"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
}

func TestLocalUpIngressControlPathsMatchPinnedProtocol(t *testing.T) {
	f := newMediaFixture(t)
	env, _, code := f.run(t, "local-up.sh", f.upArgs(f.dockerStub(), "--ingress=true", "--wait-seconds=0")...)
	if code != 0 || !env.OK {
		t.Fatalf("ingress render failed: exit=%d", code)
	}
	caddy := readFile(t, filepath.Join(f.stateDir, "Caddyfile"))
	block := regexp.MustCompile(`(?s)@control \{([^}]+)\}`).FindStringSubmatch(caddy)
	if len(block) != 2 || !strings.Contains(block[1], "method POST") {
		t.Fatal("fixed POST control matcher missing")
	}
	var paths []string
	for line := range strings.SplitSeq(block[1], "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "path" {
			paths = append(paths, fields[1:]...)
		}
	}
	transport := &ingressContractHTTPClient{}
	client := livekit.NewIngressProtobufClient("http://fixture.invalid", transport)
	ctx := context.Background()
	if _, err := client.CreateIngress(ctx, &livekit.CreateIngressRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListIngress(ctx, &livekit.ListIngressRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.DeleteIngress(ctx, &livekit.DeleteIngressRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.UpdateIngress(ctx, &livekit.UpdateIngressRequest{}); err != nil {
		t.Fatal(err)
	}
	for index, request := range transport.requests {
		wantAllowed := index < 3 // Only the resource adapter's three operations.
		if request.Method != http.MethodPost || slices.Contains(paths, request.URL.Path) != wantAllowed {
			t.Errorf("generated %s %s allowed=%v, want %v", request.Method, request.URL.Path, slices.Contains(paths, request.URL.Path), wantAllowed)
		}
	}
	var ingressPaths []string
	for _, path := range paths {
		if strings.ContainsAny(path, "*?") {
			t.Errorf("control route must be exact: %q", path)
		}
		if strings.HasPrefix(path, livekit.IngressPathPrefix) {
			ingressPaths = append(ingressPaths, path)
		}
	}
	if len(ingressPaths) != 3 || strings.Contains(caddy, "/twirp/livekit.IngressService/") {
		t.Fatal("ingress control matcher must contain exactly the three pinned protocol methods")
	}
}
