package azureblob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

type verifiedFixture struct {
	mu                     sync.Mutex
	blocks                 map[string][]byte
	body                   []byte
	exists                 bool
	commits, writes        int
	lostCommit, failedRead bool
	badETag, extraBody     bool
	onBlock                func()
	stageErr               error
	headBarrier            chan struct{}
	heads                  int
}

func verifiedSHA(body []byte) string {
	hash := sha256.Sum256(body)
	return hex.EncodeToString(hash[:])
}

func verifiedClient(t *testing.T, f *verifiedFixture) *AzureBlobUploader {
	t.Helper()
	f.blocks = map[string][]byte{}
	c, err := azblob.NewClientWithNoCredential("https://blob.test", &azblob.ClientOptions{ClientOptions: policy.ClientOptions{
		Transport: downloadTransport(f.do), Retry: policy.RetryOptions{MaxRetries: -1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return &AzureBlobUploader{client: c}
}

func (f *verifiedFixture) do(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	status := http.StatusOK
	body := []byte(nil)
	headers := http.Header{"Etag": {`"stored-v1"`}, "Content-Length": {strconv.Itoa(len(f.body))}}
	switch {
	case r.Method == http.MethodHead:
		if !f.exists {
			status = 404
			headers.Set("x-ms-error-code", "BlobNotFound")
			if f.headBarrier != nil {
				f.heads++
				if f.heads == 2 {
					close(f.headBarrier)
				}
				f.mu.Unlock()
				<-f.headBarrier
				f.mu.Lock()
			}
		}
	case r.Method == http.MethodGet:
		if f.failedRead {
			return nil, errors.New("lost read connection")
		}
		if r.Header.Get("If-Match") != `"stored-v1"` {
			return nil, errors.New("read lacks observed ETag")
		}
		body = bytes.Clone(f.body)
		if f.extraBody {
			body = append(body, 'x')
		}
		if f.badETag {
			headers.Set("ETag", `"replacement-v2"`)
		}
	case r.Method == http.MethodPut && r.URL.Query().Get("comp") == "block":
		if f.stageErr != nil {
			return nil, f.stageErr
		}
		chunk, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		if len(chunk) > verifiedBlockBytes {
			return nil, errors.New("unbounded block")
		}
		id := r.URL.Query().Get("blockid")
		if _, exists := f.blocks[id]; exists {
			return nil, errors.New("concurrent upload reused a block name")
		}
		f.blocks[id] = chunk
		if f.onBlock != nil {
			f.onBlock()
		}
		status = 201
	case r.Method == http.MethodPut && r.URL.Query().Get("comp") == "blocklist":
		f.commits++
		if r.Header.Get("If-None-Match") != "*" {
			return nil, errors.New("commit can overwrite existing data")
		}
		if f.exists {
			status = 412
			headers.Set("x-ms-error-code", "ConditionNotMet")
			break
		}
		var list struct{ Latest []string }
		if err := xml.NewDecoder(r.Body).Decode(&list); err != nil {
			return nil, err
		}
		for _, id := range list.Latest {
			f.body = append(f.body, f.blocks[id]...)
		}
		f.exists, f.writes = true, f.writes+1
		if f.lostCommit {
			return nil, errors.New("commit accepted, connection lost")
		}
		status = 201
	default:
		return nil, fmt.Errorf("unexpected blob request: %s", r.Method)
	}
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: headers,
		Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
}

func TestVerifiedStreamCommitsAndReconcilesActualBytes(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint("lost response=", lost), func(t *testing.T) {
			f := &verifiedFixture{lostCommit: lost}
			u := verifiedClient(t, f)
			body := bytes.Repeat([]byte("z"), verifiedBlockBytes+13)
			digest := verifiedSHA(body)
			got, err := u.CreateVerifiedStream(context.Background(), "test", "archive", bytes.NewReader(body), int64(len(body)), digest, "application/octet-stream")
			if err != nil || got.SHA256 != digest || got.Size != int64(len(body)) || got.ETag != `"stored-v1"` || len(f.blocks) != 2 || f.writes != 1 {
				t.Fatalf("upload: %+v %v; blocks=%d writes=%d", got, err, len(f.blocks), f.writes)
			}
			// A replacement caller can adopt without consuming another source.
			next := &AzureBlobUploader{client: u.client}
			recovered, err := next.CreateVerifiedStream(context.Background(), "test", "archive", bytes.NewReader(nil), int64(len(body)), digest, "")
			if err != nil || recovered != got || f.commits != 1 {
				t.Fatalf("recovery repeated upload: %+v %v; commits=%d", recovered, err, f.commits)
			}
		})
	}
}

func TestVerifiedStreamRefusesInvalidInputsBeforeCommit(t *testing.T) {
	for _, name := range []string{"short", "long", "digest", "oversized", "negative", "bad digest", "nil source", "read error", "stage error", "cancel"} {
		t.Run(name, func(t *testing.T) {
			f := &verifiedFixture{}
			u := verifiedClient(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			source := io.Reader(strings.NewReader("abc"))
			size, digest := int64(3), verifiedSHA([]byte("abc"))
			switch name {
			case "short":
				size++
			case "long":
				size--
			case "digest":
				digest = verifiedSHA([]byte("xyz"))
			case "oversized":
				size = MaxVerifiedStreamBytes + 1
			case "negative":
				size = -1
			case "bad digest":
				digest = strings.ToUpper(digest)
			case "nil source":
				source = nil
			case "read error":
				source = failingDownloadReader{}
			case "stage error":
				f.stageErr = errors.New("stage failed")
			case "cancel":
				f.onBlock = cancel
			}
			if got, err := u.CreateVerifiedStream(ctx, "test", "archive", source, size, digest, ""); err == nil || got != (VerifiedBlob{}) || f.commits != 0 {
				t.Fatalf("invalid source committed: %+v %v; commits=%d", got, err, f.commits)
			}
		})
	}
}

func TestVerifiedStreamNeverAdoptsMetadataOrChangedContent(t *testing.T) {
	for _, name := range []string{"wrong bytes", "wrong size", "changed version", "overlong body", "read error"} {
		t.Run(name, func(t *testing.T) {
			f := &verifiedFixture{exists: true, body: []byte("abc")}
			size := int64(3)
			switch name {
			case "wrong bytes":
				f.body = []byte("xyz")
			case "wrong size":
				size++
			case "changed version":
				f.badETag = true
			case "overlong body":
				f.extraBody = true
			case "read error":
				f.failedRead = true
			}
			u := verifiedClient(t, f)
			if got, err := u.CreateVerifiedStream(context.Background(), "test", "archive", strings.NewReader("abc"), size, verifiedSHA([]byte("abc")), ""); err == nil || got != (VerifiedBlob{}) || f.commits != 0 || len(f.blocks) != 0 {
				t.Fatalf("existing object was adopted or modified: %+v %v", got, err)
			}
		})
	}
}

func TestVerifiedStreamUnconfirmedCommitIsNotSuccess(t *testing.T) {
	f := &verifiedFixture{lostCommit: true, failedRead: true}
	u := verifiedClient(t, f)
	_, err := u.CreateVerifiedStream(context.Background(), "test", "archive", strings.NewReader("abc"), 3, verifiedSHA([]byte("abc")), "")
	if !errors.Is(err, ErrBlobCommitUncertain) || f.commits != 1 || !f.exists {
		t.Fatalf("unconfirmed commit: %v; commits=%d exists=%v", err, f.commits, f.exists)
	}
	f.failedRead = false
	if _, err := u.VerifyStream(context.Background(), "test", "archive", 3, verifiedSHA([]byte("abc"))); err != nil || f.commits != 1 {
		t.Fatalf("read-only reconciliation: %v", err)
	}
}

func TestVerifiedStreamConcurrentCreatorsCannotMixOrOverwrite(t *testing.T) {
	f := &verifiedFixture{headBarrier: make(chan struct{})}
	u := verifiedClient(t, f)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if _, err := u.CreateVerifiedStream(context.Background(), "test", "archive", strings.NewReader("abc"), 3, verifiedSHA([]byte("abc")), ""); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if f.writes != 1 || len(f.blocks) != 2 || string(f.body) != "abc" {
		t.Fatalf("concurrent creators changed content: writes=%d blocks=%d", f.writes, len(f.blocks))
	}
}

func TestVerifiedStreamReceiptNeverIncludesClientCredentials(t *testing.T) {
	f := &verifiedFixture{exists: true, body: []byte("abc")}
	c, err := azblob.NewClientWithNoCredential("https://blob.test/?sig=private-token&se=expiry", &azblob.ClientOptions{ClientOptions: policy.ClientOptions{
		Transport: downloadTransport(f.do), Retry: policy.RetryOptions{MaxRetries: -1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	u := &AzureBlobUploader{client: c}
	got, err := u.VerifyStream(context.Background(), "test", "archive", 3, verifiedSHA([]byte("abc")))
	if err != nil || got.URL != "https://blob.test/test/archive" {
		t.Fatal("receipt was not an unsigned object reference", err)
	}
	if _, err := (*AzureBlobUploader)(nil).VerifyStream(context.Background(), "test", "archive", 3, verifiedSHA([]byte("abc"))); err == nil {
		t.Fatal("nil storage accepted verification")
	}
}
