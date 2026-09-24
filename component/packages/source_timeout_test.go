package packages

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// A slow, healthy transfer must survive many idle intervals; a stalled one
// must be interrupted while Read is blocked. These use actual HTTP bodies so
// cancellation exercises the same transport boundary as the GitHub download.
func TestSourceBodyProgressAndStall(t *testing.T) {
	for _, stall := range []bool{false, true} {
		name := "progress"
		if stall {
			name = "stall"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				if stall {
					<-r.Context().Done()
					return
				}
				for i := 0; i < 12; i++ {
					select {
					case <-r.Context().Done():
						return
					case <-time.After(15 * time.Millisecond):
					}
					_, _ = w.Write([]byte("a"))
					w.(http.Flusher).Flush()
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(&sourceBody{ReadCloser: resp.Body, cancel: cancel, idle: 100 * time.Millisecond})
			if stall {
				if err == nil || !errors.Is(context.Cause(ctx), errSourceReadIdle) {
					t.Fatalf("stalled source not cancelled: %v, cause %v", err, context.Cause(ctx))
				}
				return
			}
			if err != nil || string(got) != "aaaaaaaaaaaa" {
				t.Fatalf("healthy source was cut off: %q, %v", got, err)
			}
		})
	}
}

type sourceTestTransport struct {
	base   http.RoundTripper
	target *url.URL
}

func (r sourceTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	u := *req.URL
	u.Scheme = r.target.Scheme
	u.Host = r.target.Host
	req.URL = &u
	return r.base.RoundTrip(req)
}
func sourceTestFetcher(t *testing.T, handler http.HandlerFunc) *githubFetcher {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	target, _ := url.Parse(server.URL)
	client := sourceHTTPClient()
	client.Transport = sourceTestTransport{base: client.Transport, target: target}
	return &githubFetcher{http: client, tempDir: t.TempDir()}
}

func TestFetchRepoRetainsCompleteSource(t *testing.T) {
	const version = "0123456789abcdef0123456789abcdef01234567"
	files := spaOnlyPackage()
	files["public/media.bin"] = &fstest.MapFile{Data: []byte("retained media bytes")}
	archive := gitHubTarball(t, "acme-widget-"+version, files)
	fetcher := sourceTestFetcher(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) })
	snap, err := fetcher.FetchRepo(context.Background(), RepoSource{RepoUrl: "https://github.com/acme/widget", Ref: version}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	// Confirm/retry consumes the same retained archive, including non-analysis
	// assets. No source filtering or repository re-read may stand in for it.
	root, err := ExtractTarGz(bytes.NewReader(snap.Bytes), t.TempDir(), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range files {
		got, err := fs.ReadFile(os.DirFS(root), name)
		if err != nil || string(got) != string(want.Data) {
			t.Fatalf("snapshot lost %s: %q, %v", name, got, err)
		}
	}
	if snap.Version != version {
		t.Fatalf("version=%s", snap.Version)
	}
}

func TestFetchRepoCancellationInterruptsBodyAndCleansUp(t *testing.T) {
	started := make(chan struct{})
	fetcher := sourceTestFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := fetcher.FetchRepo(ctx, RepoSource{RepoUrl: "https://github.com/acme/widget"}, Limits{})
		result <- err
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancelled fetch succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt read")
	}
	entries, err := os.ReadDir(fetcher.tempDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial source left behind: %v, %v", entries, err)
	}
}

func TestFetchRepoPreservesArchiveLimits(t *testing.T) {
	archive := gitHubTarball(t, "acme-widget-01234567", fstest.MapFS{"media.bin": &fstest.MapFile{Data: []byte(strings.Repeat("a", 1024))}})
	fetcher := sourceTestFetcher(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) })
	_, err := fetcher.FetchRepo(context.Background(), RepoSource{RepoUrl: "https://github.com/acme/widget"}, Limits{MaxFileBytes: 512})
	if RefusalCode(err) != CodeSourceTooLarge {
		t.Fatalf("size guard lost: %v", err)
	}
}

// Even the synchronous caller cannot leave a continuously streaming source
// unbounded. A shorter caller budget still wins over the source ceiling.
func TestFetchRepoDeadlineIsBounded(t *testing.T) {
	for _, shorter := range []bool{false, true} {
		ctx := context.Background()
		cancel := func() {}
		if shorter {
			ctx, cancel = context.WithTimeout(ctx, time.Second)
		}
		func() {
			defer cancel()
			base := sourceHTTPClient()
			base.Transport = sourceDeadlineTransport{t: t, shorter: shorter}
			f := &githubFetcher{http: base}
			_, _ = f.FetchRepo(ctx, RepoSource{RepoUrl: "https://github.com/acme/widget"}, Limits{})
		}()
	}
}

type sourceDeadlineTransport struct {
	t       *testing.T
	shorter bool
}

func (r sourceDeadlineTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	deadline, ok := req.Context().Deadline()
	if !ok {
		r.t.Fatal("unbounded source request")
	}
	remaining := time.Until(deadline)
	if remaining > sourceDownloadTimeout || (!r.shorter && remaining < sourceDownloadTimeout-time.Second) || (r.shorter && remaining > time.Second) {
		r.t.Fatalf("unexpected source budget: %s", remaining)
	}
	return nil, errors.New("deadline captured")
}

func TestSourceDownloadErrorNamesTheFailedStage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	err := sourceDownloadError(ctx, context.DeadlineExceeded)
	if RefusalCode(err) != CodeSourceUnreadable || !strings.Contains(err.Error(), "GitHub source download") {
		t.Fatalf("raw transport deadline reached the user: %v", err)
	}
	ctx2, cancel2 := context.WithCancelCause(context.Background())
	defer cancel2(nil)
	cancel2(errSourceReadIdle)
	err = sourceDownloadError(ctx2, context.Canceled)
	if RefusalCode(err) != CodeSourceUnreadable || !strings.Contains(err.Error(), "received no data") {
		t.Fatalf("idle failure lost its reason: %v", err)
	}
	ctx3, cancel3 := context.WithCancel(context.Background())
	cancel3()
	if err = sourceDownloadError(ctx3, context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("user cancellation was relabeled: %v", err)
	}
}
