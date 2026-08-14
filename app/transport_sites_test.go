//go:build bff

package app

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/server"
)

// A declared-but-unserved route is exactly the defect memql#3713's fix
// round closes: HandlerAuthorizedPaths() described POST /sites/{id}/bundles
// before any handler served it. This is the other half of that fix, mirrored
// from transport_edge_test.go's TestEdgeRootMountSurvivesTheUnauthenticatedSurfaceAssertion
// for the write side: it exercises the REAL, live HandlerAuthorizedPaths()
// (via server.AssertUnauthenticatedSurfaceDeclared, not a stand-in) against
// the EXACT pattern mountSiteBundleEndpoints registers, so if
// SitesBundlePaths() and the boot-completeness declaration ever drift apart,
// this is what catches it -- in either direction: a route registered but not
// declared, or (the original bug) a route declared but never registered.
func TestSiteBundleRouteSurvivesTheUnauthenticatedSurfaceAssertion(t *testing.T) {
	// The exact pattern mountSiteBundleEndpoints registers: "POST " + each
	// of server.SitesBundlePaths().
	var registered []string
	for _, path := range server.SitesBundlePaths() {
		registered = append(registered, "POST "+path)
	}
	a := &App{registeredRoutes: registered}

	whole := a.unauthenticatedSurfaceRoutes(true)
	if err := server.AssertUnauthenticatedSurfaceDeclared(whole); err != nil {
		t.Fatalf("POST /sites/{id}/bundles must be declared in HandlerAuthorizedPaths() -- %v", err)
	}
}

func TestSiteBundleBlobWriterUsesTheRealUploaderWhenConfigured(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	w := siteBundleBlobWriter(logger, &fakeFileUploader{}, "sites-container")

	if _, ok := w.(unconfiguredBlobWriter); ok {
		t.Fatal("got unconfiguredBlobWriter despite a configured uploader + container")
	}
	if buf.Len() != 0 {
		t.Errorf("a configured uploader must not warn: %q", buf.String())
	}
}

func TestSiteBundleBlobWriterWarnsAndRefusesWhenUploaderIsNil(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	w := siteBundleBlobWriter(logger, nil, "sites-container")

	if err := w.Put(context.Background(), "sites/s1/v1/index.html", []byte("x")); err == nil {
		t.Error("unconfigured BlobWriter must refuse every Put, not silently succeed")
	}
	if !strings.Contains(buf.String(), "not configured") {
		t.Errorf("expected a boot-time warning naming the misconfiguration, got %q", buf.String())
	}
}

func TestSiteBundleBlobWriterWarnsAndRefusesWhenContainerIsEmpty(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	w := siteBundleBlobWriter(logger, &fakeFileUploader{}, "")

	if err := w.Put(context.Background(), "sites/s1/v1/index.html", []byte("x")); err == nil {
		t.Error("unconfigured BlobWriter must refuse every Put, not silently succeed")
	}
	if !strings.Contains(buf.String(), "not configured") {
		t.Errorf("expected a boot-time warning naming the misconfiguration, got %q", buf.String())
	}
}

// Never a silent success -- this is the property that makes it SAFE for
// mountSiteBundleEndpoints to mount the route unconditionally rather than
// refusing to boot: Publisher.Publish only reaches SiteStore.UpdateBundleRef
// after every Put succeeds, so a Put that fails immediately never flips a
// site's bundleRef to point at bytes that were never written.
func TestUnconfiguredBlobWriterAlwaysFails(t *testing.T) {
	var w unconfiguredBlobWriter
	if err := w.Put(context.Background(), "any/key", []byte("x")); err == nil {
		t.Fatal("unconfiguredBlobWriter.Put succeeded; it must always fail")
	}
}

// fakeFileUploader satisfies server.FileUploader (and therefore, by
// identical method set, edge.AzureUploader) without a real Azure client.
type fakeFileUploader struct{}

func (fakeFileUploader) Upload(_ context.Context, _, objectName string, _ []byte, _ string) (string, error) {
	return "https://example.blob.core.windows.net/c/" + objectName, nil
}
