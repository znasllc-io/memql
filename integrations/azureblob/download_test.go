package azureblob

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

type downloadTransport func(*http.Request) (*http.Response, error)

func (f downloadTransport) Do(r *http.Request) (*http.Response, error) { return f(r) }

type trackedDownloadBody struct {
	io.Reader
	closed bool
}

func (b *trackedDownloadBody) Close() error { b.closed = true; return nil }

func downloadFixture(t *testing.T, reader io.Reader, length int64) (*AzureBlobUploader, *trackedDownloadBody) {
	t.Helper()
	body := &trackedDownloadBody{Reader: reader}
	transport := downloadTransport(func(r *http.Request) (*http.Response, error) {
		headers := http.Header{}
		if length >= 0 {
			headers.Set("Content-Length", strconv.FormatInt(length, 10))
		}
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: headers, Body: body, ContentLength: length, Request: r}, nil
	})
	client, err := azblob.NewClientWithNoCredential("https://blob.test", &azblob.ClientOptions{ClientOptions: policy.ClientOptions{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	return &AzureBlobUploader{client: client}, body
}

func TestDownloadLimitBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		length, limit int64
		wantError     bool
	}{
		{"below", "abc", 3, 4, false},
		{"exact", "abcd", 4, 4, false},
		{"oversized header", "abcde", 5, 4, true},
		{"chunked below", "abc", -1, 4, false},
		{"chunked exact", "abcd", -1, 4, false},
		{"chunked oversized", "abcde", -1, 4, true},
		{"underreported size", "abcde", 2, 4, true},
		{"short body", "abc", 4, 8, true},
		{"long body", "abcd", 3, 8, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, body := downloadFixture(t, strings.NewReader(tc.content), tc.length)
			got, err := u.DownloadWithLimit(context.Background(), "memql", "archive", tc.limit)
			if tc.wantError {
				if err == nil || got != nil {
					t.Fatalf("oversize returned partial success: %d bytes, %v", len(got), err)
				}
			} else if err != nil || string(got) != tc.content {
				t.Fatalf("got %q, %v", got, err)
			}
			if !body.closed {
				t.Fatal("download body not closed")
			}
		})
	}
}

type zeroDownloadReader struct{}

func (zeroDownloadReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// This is the production size mismatch: a complete archive above the attachment
// ceiling but below the package source ceiling. Small fixtures never caught it.
func TestDownloadCallerBudgetExceedsAttachmentLimit(t *testing.T) {
	const size int64 = maxDownloadBytes + 1
	u, body := downloadFixture(t, io.LimitReader(zeroDownloadReader{}, size), size)
	if data, err := u.Download(context.Background(), "memql", "snapshot"); err == nil || data != nil {
		t.Fatal("default download accepted oversized blob")
	}
	if !body.closed {
		t.Fatal("rejected body not closed")
	}
	u, body = downloadFixture(t, io.LimitReader(zeroDownloadReader{}, size), size)
	data, err := u.DownloadWithLimit(context.Background(), "memql", "snapshot", size)
	if err != nil || int64(len(data)) != size {
		t.Fatalf("caller budget failed: %d bytes, %v", len(data), err)
	}
	if !body.closed {
		t.Fatal("successful body not closed")
	}
}

type failingDownloadReader struct{}

func (failingDownloadReader) Read(p []byte) (int, error) {
	copy(p, "abc")
	return 3, io.ErrUnexpectedEOF
}
func TestDownloadReadFailureReturnsNoPartialBytes(t *testing.T) {
	u, body := downloadFixture(t, failingDownloadReader{}, 10)
	data, err := u.DownloadWithLimit(context.Background(), "memql", "snapshot", 20)
	if !errors.Is(err, io.ErrUnexpectedEOF) || data != nil {
		t.Fatalf("got %d bytes, %v", len(data), err)
	}
	if !body.closed {
		t.Fatal("failed body not closed")
	}
}

func TestDownloadRejectsInvalidBudgets(t *testing.T) {
	for _, limit := range []int64{-1, 0, math.MaxInt64} {
		u, _ := downloadFixture(t, strings.NewReader(""), 0)
		if _, err := u.DownloadWithLimit(context.Background(), "memql", "snapshot", limit); err == nil {
			t.Fatalf("accepted invalid budget %d", limit)
		}
	}
}

// IsBlobAbsent is the difference between one log reported as gone for good and
// a read the failure path retries, so it reads what the service said through
// the real SDK, and never the words around it: a 500's text can hold "404" or
// "BlobNotFound" in a path or a request id.
func TestIsBlobAbsentReadsTheServiceCodeNotTheText(t *testing.T) {
	const stored = "https://blob.test/memql-library/library/404/BlobNotFound/step.log"
	uploader := func(t *testing.T, respond downloadTransport) *AzureBlobUploader {
		t.Helper()
		client, err := azblob.NewClientWithNoCredential("https://blob.test", &azblob.ClientOptions{ClientOptions: policy.ClientOptions{
			Transport: respond, Retry: policy.RetryOptions{MaxRetries: -1},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return &AzureBlobUploader{client: client}
	}
	answering := func(status int, code string) downloadTransport {
		return func(r *http.Request) (*http.Response, error) {
			headers := http.Header{}
			if code != "" {
				headers.Set("x-ms-error-code", code)
			}
			return &http.Response{StatusCode: status, Status: strconv.Itoa(status), Header: headers, Body: io.NopCloser(strings.NewReader("RequestId:404 BlobNotFound")), Request: r}, nil
		}
	}
	for name, tc := range map[string]struct {
		respond downloadTransport
		want    bool
	}{
		"the blob is not there": {answering(404, "BlobNotFound"), true},
		"the container is not":  {answering(404, "ContainerNotFound"), false},
		"a 404 with no code":    {answering(404, ""), false},
		"storage is busy":       {answering(503, "ServerBusy"), false},
		"storage failed":        {answering(500, "InternalError"), false},
		"storage refuses":       {answering(403, "AuthorizationFailure"), false},
		"the connection drops":  {func(*http.Request) (*http.Response, error) { return nil, errors.New("connection reset") }, false},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := uploader(t, tc.respond).DownloadStreamURL(context.Background(), stored)
			if err == nil {
				t.Fatal("the read succeeded")
			}
			if got := IsBlobAbsent(err); got != tc.want {
				t.Fatalf("IsBlobAbsent(%v) = %v, want %v", err, got, tc.want)
			}
		})
	}
	t.Run("a retired stream", func(t *testing.T) {
		_, err := uploader(t, func(r *http.Request) (*http.Response, error) {
			headers := http.Header{"X-Ms-Meta-" + retiredStreamMetadata: {"marker"}}
			return &http.Response{StatusCode: 200, Status: "200 OK", Header: headers, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}).DownloadStreamURL(context.Background(), stored)
		if !errors.Is(err, ErrBlobRetired) || !IsBlobAbsent(err) {
			t.Fatalf("a tombstone answered %v, absent=%v", err, IsBlobAbsent(err))
		}
	})
	t.Run("a stored value naming no object", func(t *testing.T) {
		// The bare key a chunked upload's row stores: storage is never asked,
		// so no retry can change the answer.
		_, err := uploader(t, func(*http.Request) (*http.Response, error) {
			t.Fatal("storage was asked for a bare key")
			return nil, nil
		}).DownloadStreamURL(context.Background(), "library/user-a/f-1/step.log")
		if err == nil || !IsBlobAbsent(err) {
			t.Fatalf("a bare key answered %v, absent=%v", err, IsBlobAbsent(err))
		}
	})
	if IsBlobAbsent(nil) || IsBlobAbsent(context.DeadlineExceeded) {
		t.Fatal("no failure, or a timeout, read as an absent blob")
	}
}
