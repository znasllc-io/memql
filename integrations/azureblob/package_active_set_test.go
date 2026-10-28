package azureblob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/znasllc-io/memql/component/auth"
)

type activeSetContextKey struct{}

type activeSetFixture struct {
	mu                                        sync.Mutex
	exists                                    bool
	body                                      []byte
	etag                                      string
	puts, gets                                int
	lostWrite, failedRead, badReply, mismatch bool
	changedReadVersion                        bool
	ctx                                       context.Context
}

func activeSetClient(t *testing.T, transport downloadTransport) *AzureBlobUploader {
	t.Helper()
	c, err := azblob.NewClientWithNoCredential("https://blob.test", &azblob.ClientOptions{ClientOptions: policy.ClientOptions{
		Transport: transport, Retry: policy.RetryOptions{MaxRetries: -1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return &AzureBlobUploader{client: c}
}

func activeSetResponse(r *http.Request, status int, body []byte, etag, code string) *http.Response {
	h := http.Header{}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if etag != "" {
		h.Set("ETag", etag)
	}
	if code != "" {
		h.Set("x-ms-error-code", code)
	}
	return &http.Response{Request: r, StatusCode: status, Status: http.StatusText(status), Header: h, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}
}

func (f *activeSetFixture) do(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != "/container/packages/active.json" || r.URL.RawQuery != "" {
		return nil, fmt.Errorf("unexpected active-set destination: %s", r.URL.Path)
	}
	if f.ctx != nil {
		want, _ := auth.AccessFromContext(f.ctx)
		got, _ := auth.AccessFromContext(r.Context())
		deadline, _ := f.ctx.Deadline()
		actual, _ := r.Context().Deadline()
		if got != want || !deadline.Equal(actual) || r.Context().Value(activeSetContextKey{}) != "same caller" {
			return nil, errors.New("caller scope changed")
		}
	}
	switch r.Method {
	case http.MethodGet:
		f.gets++
		if f.failedRead {
			return nil, errors.New("lost read response")
		}
		if !f.exists {
			return activeSetResponse(r, 404, nil, "", "BlobNotFound"), nil
		}
		if match := r.Header.Get("If-Match"); match != "" && match != f.etag {
			return activeSetResponse(r, 412, nil, "", "ConditionNotMet"), nil
		}
		body := bytes.Clone(f.body)
		if f.mismatch && len(body) > 0 {
			body[0] ^= 1
		}
		etag := f.etag
		if f.changedReadVersion {
			etag = `"changed-version"`
		}
		return activeSetResponse(r, 200, body, etag, ""), nil
	case http.MethodPut:
		f.puts++
		match, absent := r.Header.Get("If-Match"), r.Header.Get("If-None-Match")
		if (match == "") == (absent == "") || (absent != "" && absent != "*") {
			return nil, errors.New("write lacks exactly one conditional version")
		}
		if (absent == "*" && f.exists) || (match != "" && (!f.exists || match != f.etag)) {
			return activeSetResponse(r, 412, nil, "", "ConditionNotMet"), nil
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		f.body, f.exists, f.etag = data, true, fmt.Sprintf("\"version-%d\"", f.puts)
		if f.lostWrite {
			return nil, errors.New("write accepted; response lost")
		}
		etag := f.etag
		if f.badReply {
			etag = ""
		}
		return activeSetResponse(r, 201, nil, etag, ""), nil
	default:
		return nil, errors.New("unexpected active-set method")
	}
}

func TestPackageActiveSetConditionalCreateAndReplace(t *testing.T) {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "actor", Role: "owner"})
	ctx = context.WithValue(ctx, activeSetContextKey{}, "same caller")
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	f := &activeSetFixture{ctx: ctx}
	u := activeSetClient(t, f.do)
	absent, err := u.ReadPackageActiveSet(ctx, "container")
	if err != nil || absent.Exists || absent.ETag != "" || len(absent.Data) != 0 {
		t.Fatalf("absence: %+v %v", absent, err)
	}
	first := []byte(`{"one":"prefix/","unrelated":"retained/"}`)
	created, err := u.CompareAndSwapPackageActiveSet(ctx, "container", absent, first)
	if err != nil || !created.Exists || created.ETag != `"version-1"` || !bytes.Equal(created.Data, first) {
		t.Fatalf("create: %+v %v", created, err)
	}
	next := []byte(`{"one":"next/","unrelated":"retained/"}`)
	replaced, err := u.CompareAndSwapPackageActiveSet(ctx, "container", created, next)
	if err != nil || replaced.ETag != `"version-2"` || !bytes.Equal(replaced.Data, next) || f.puts != 2 {
		t.Fatalf("replace: %+v %v writes=%d", replaced, err, f.puts)
	}
	// This adapter transports bounded bytes; package map semantics belong to its
	// caller. There is no alternate product parser or domain-selection policy.
	if _, err = u.CompareAndSwapPackageActiveSet(ctx, "container", replaced, []byte("opaque")); err != nil {
		t.Fatal(err)
	}
}

func TestPackageActiveSetStaleVersionsNeverOverwrite(t *testing.T) {
	f := &activeSetFixture{exists: true, body: []byte("current"), etag: `"current"`}
	u := activeSetClient(t, f.do)
	for _, expected := range []PackageActiveSetSnapshot{{}, {Exists: true, ETag: `"stale"`, Data: []byte("old")}} {
		before := f.puts
		_, err := u.CompareAndSwapPackageActiveSet(context.Background(), "container", expected, []byte("replacement"))
		if !errors.Is(err, ErrPackageActiveSetChanged) || f.puts != before+1 || string(f.body) != "current" {
			t.Fatalf("stale version repeated or replaced: %v writes=%d body=%q", err, f.puts-before, f.body)
		}
	}
}

func TestPackageActiveSetCreateConflictRequiresObservedAbsence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		expected PackageActiveSetSnapshot
		code     string
		refused  bool
	}{
		{name: "create already exists", code: "BlobAlreadyExists", refused: true},
		{name: "existing version remains uncertain", expected: PackageActiveSetSnapshot{Exists: true, ETag: `"prior"`, Data: []byte("old")}, code: "BlobAlreadyExists"},
		{name: "different conflict remains uncertain", code: "ContainerAlreadyExists"},
		{name: "unclassified conflict remains uncertain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			u := activeSetClient(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodPut || (tc.expected.Exists && r.Header.Get("If-Match") != tc.expected.ETag) || (!tc.expected.Exists && r.Header.Get("If-None-Match") != "*") {
					t.Fatalf("unexpected conditional request: %s", r.Method)
				}
				return activeSetResponse(r, http.StatusConflict, nil, "", tc.code), nil
			})
			_, err := u.CompareAndSwapPackageActiveSet(context.Background(), "container", tc.expected, []byte("new"))
			if calls != 1 || errors.Is(err, ErrPackageActiveSetChanged) != tc.refused || errors.Is(err, ErrBlobCommitUncertain) == tc.refused {
				t.Fatalf("conflict classification/replay: refused=%v calls=%d err=%v", tc.refused, calls, err)
			}
		})
	}
}

func TestPackageActiveSetUncertainWriteNeedsReadOnlyReconciliation(t *testing.T) {
	for _, kind := range []string{"lost reply", "missing version", "failed read", "wrong bytes", "changed version"} {
		t.Run(kind, func(t *testing.T) {
			f := &activeSetFixture{lostWrite: kind == "lost reply", badReply: kind == "missing version", failedRead: kind == "failed read", mismatch: kind == "wrong bytes", changedReadVersion: kind == "changed version"}
			u := activeSetClient(t, f.do)
			_, err := u.CompareAndSwapPackageActiveSet(context.Background(), "container", PackageActiveSetSnapshot{}, []byte("expected"))
			if !errors.Is(err, ErrBlobCommitUncertain) || f.puts != 1 || !f.exists {
				t.Fatalf("unknown outcome was lost/replayed: %v writes=%d", err, f.puts)
			}
			f.failedRead, f.mismatch, f.changedReadVersion = false, false, false
			// A replacement adapter can observe without resending the effect.
			other := activeSetClient(t, f.do)
			got, err := other.ReadPackageActiveSet(context.Background(), "container")
			if err != nil || !got.Exists || string(got.Data) != "expected" || got.ETag == "" || f.puts != 1 {
				t.Fatalf("reconcile: %+v %v writes=%d", got, err, f.puts)
			}
		})
	}
}

func TestPackageActiveSetReadRefusesUnknownAbsenceAndInvalidBodies(t *testing.T) {
	for _, kind := range []string{"container missing", "denied", "transport", "missing ETag", "weak ETag", "missing length", "oversize", "empty", "short", "long", "retired"} {
		t.Run(kind, func(t *testing.T) {
			var closed *trackedDownloadBody
			calls := 0
			u := activeSetClient(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if kind == "transport" {
					return nil, errors.New("read unavailable")
				}
				response := activeSetResponse(r, 200, []byte("abc"), `"version"`, "")
				switch kind {
				case "container missing":
					return activeSetResponse(r, 404, nil, "", "ContainerNotFound"), nil
				case "denied":
					return activeSetResponse(r, 403, nil, "", "AuthorizationFailure"), nil
				case "missing ETag":
					response.Header.Del("ETag")
				case "weak ETag":
					response.Header.Set("ETag", `W/"version"`)
				case "missing length":
					response.Header.Del("Content-Length")
				case "oversize":
					response.Header.Set("Content-Length", strconv.Itoa(MaxPackageActiveSetBytes+1))
				case "empty":
					response.Header.Set("Content-Length", "0")
				case "short":
					response.Header.Set("Content-Length", "4")
				case "long":
					response.Header.Set("Content-Length", "2")
				case "retired":
					response.Header.Set("x-ms-meta-"+retiredStreamMetadata, "marker")
				}
				closed = &trackedDownloadBody{Reader: strings.NewReader("abc")}
				response.Body = closed
				return response, nil
			})
			got, err := u.ReadPackageActiveSet(context.Background(), "container")
			if err == nil || got.Exists || got.ETag != "" || got.Data != nil || calls != 1 {
				t.Fatalf("invalid read became absence/success: %+v %v calls=%d", got, err, calls)
			}
			if closed != nil && !closed.closed {
				t.Fatal("body leaked")
			}
		})
	}
}

func TestPackageActiveSetRejectsInvalidWriteBeforeProvider(t *testing.T) {
	u := activeSetClient(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid write reached provider")
		return nil, nil
	})
	for _, tc := range []struct {
		expected PackageActiveSetSnapshot
		data     []byte
	}{
		{PackageActiveSetSnapshot{}, nil},
		{PackageActiveSetSnapshot{}, make([]byte, MaxPackageActiveSetBytes+1)},
		{PackageActiveSetSnapshot{Exists: true}, []byte("x")},
		{PackageActiveSetSnapshot{Exists: true, ETag: "*"}, []byte("x")},
		{PackageActiveSetSnapshot{Exists: true, ETag: "\"bad\r\nversion\""}, []byte("x")},
		{PackageActiveSetSnapshot{ETag: `"version"`}, []byte("x")},
		{PackageActiveSetSnapshot{Data: []byte("old")}, []byte("x")},
	} {
		if _, err := u.CompareAndSwapPackageActiveSet(context.Background(), "container", tc.expected, tc.data); err == nil {
			t.Fatal("invalid write accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := u.CompareAndSwapPackageActiveSet(ctx, "container", PackageActiveSetSnapshot{}, []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled write: %v", err)
	}
}

func TestPackageActiveSetConcurrentReplicasKeepOneVersion(t *testing.T) {
	f := &activeSetFixture{exists: true, body: []byte("old"), etag: `"initial"`}
	expected := PackageActiveSetSnapshot{Exists: true, ETag: `"initial"`, Data: []byte("old")}
	first, second := activeSetClient(t, f.do), activeSetClient(t, f.do)
	start := make(chan struct{})
	results := make(chan error, 2)
	for index, client := range []*AzureBlobUploader{first, second} {
		go func(n int, u *AzureBlobUploader) {
			<-start
			_, err := u.CompareAndSwapPackageActiveSet(context.Background(), "container", expected, []byte(fmt.Sprintf("replica-%d", n)))
			results <- err
		}(index, client)
	}
	close(start)
	success, conflicts := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, ErrPackageActiveSetChanged) {
			conflicts++
		} else {
			t.Fatalf("unexpected concurrent result: %v", err)
		}
	}
	if success != 1 || conflicts != 1 || f.puts != 2 || !strings.HasPrefix(string(f.body), "replica-") {
		t.Fatalf("CAS results success=%d conflicts=%d puts=%d body=%q", success, conflicts, f.puts, f.body)
	}
}
