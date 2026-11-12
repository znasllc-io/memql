package inbound

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A source may carry deliveries larger than the receiver's default cap when its
// own policy says so. GitHub is the case that needed it: a push payload lists
// every pushed commit with its changed files, so one push of a long branch is
// far over 256 KiB. At the default cap the receiver answered that push 413 and
// staged nothing, the pipeline it should have started never opened, and
// nothing in the engine said why.
func TestASourceWithItsOwnCapStagesABodyOverTheReceiverDefault(t *testing.T) {
	eng := &fakeEngine{}
	src := hexSource()
	src.MaxBodyBytes = 4096
	body := `{"pad":"` + strings.Repeat("x", 2048) + `"}`
	rec := httptest.NewRecorder()
	testHandler(t, eng, src).ServeHTTP(rec, signedRequest(t, body))

	if rec.Code != http.StatusAccepted || len(eng.calls) != 1 {
		t.Fatalf("a %d-byte delivery under its source's %d-byte cap (receiver default 1024) was answered %d "+
			"with %d staged: %s", len(body), src.MaxBodyBytes, rec.Code, len(eng.calls), rec.Body)
	}
}

// A refusal for size is logged, naming the source, the cap that refused it and
// the setting that raises that source's cap. It used to be silent: the 413 went
// back to the sender and nothing reached a log an operator reads, so the only
// trace of the lost delivery was in the sender's own delivery history.
func TestABodyOverTheCapIsRefusedWithAWarningNamingTheSetting(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cap   int
		size  int
		limit string
	}{
		{name: "the receiver's default cap", cap: 0, size: 2048, limit: "limit=1024"},
		{name: "the source's own cap", cap: 4096, size: 5000, limit: "limit=4096"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := &fakeEngine{}
			src := hexSource()
			src.MaxBodyBytes = tc.cap
			var logs bytes.Buffer
			h := testHandler(t, eng, src)
			h.logger = slog.New(slog.NewTextHandler(&logs, nil))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, signedRequest(t, strings.Repeat("x", tc.size)))

			if rec.Code != http.StatusRequestEntityTooLarge || len(eng.calls) != 0 {
				t.Fatalf("a %d-byte body over a %s answered %d with %d staged", tc.size, tc.limit, rec.Code, len(eng.calls))
			}
			line := logs.String()
			for _, want := range []string{
				"level=WARN",
				`msg="inbound receiver: request refused, body over the cap"`,
				"source=acme",
				tc.limit,
				"setting=MEMQL_INBOUND_SOURCE_ACME_MAX_BODY_BYTES",
			} {
				if !strings.Contains(line, want) {
					t.Errorf("the refusal log is missing %q:\n%s", want, line)
				}
			}
		})
	}
}

// countingReader records how much of a body the handler read.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// A body whose declared length is already over the cap is refused before a byte
// of it is read: there is nothing to learn from reading it, and reading it is
// the allocation the cap exists to prevent.
func TestADeclaredLengthOverTheCapIsRefusedBeforeTheBodyIsRead(t *testing.T) {
	eng := &fakeEngine{}
	body := &countingReader{r: strings.NewReader(strings.Repeat("x", 2048))}
	r := httptest.NewRequest(http.MethodPost, "/inbound/acme", body)
	r.ContentLength = 2048
	// Signed, or the request is refused for its missing signature first: the
	// length is the question here.
	r.Header.Set("X-Sig", "00")
	rec := httptest.NewRecorder()
	testHandler(t, eng, hexSource()).ServeHTTP(rec, r)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a declared 2048 bytes against a 1024-byte cap answered %d", rec.Code)
	}
	if body.n != 0 {
		t.Errorf("read %d bytes of a body whose declared length was already over the cap", body.n)
	}
}

// A raised cap must not undo what the default cap protects: what an
// unauthenticated caller can make the node allocate before the signature check
// runs. A body over the receiver's default is therefore read by a bounded
// number of requests at once, and one that cannot get a turn quickly is
// answered 503 and logged. A body under the default needs no turn, so a crowd
// of large requests cannot hold up ordinary deliveries.
func TestReadsOverTheDefaultCapAreBoundedAndSmallReadsAreNot(t *testing.T) {
	eng := &fakeEngine{}
	src := hexSource()
	src.MaxBodyBytes = 4096
	var logs bytes.Buffer
	h := testHandler(t, eng, src)
	h.logger = slog.New(slog.NewTextHandler(&logs, nil))
	h.largeBodyWait = time.Millisecond
	if cap(h.largeBodies) == 0 {
		t.Fatalf("the handler has no bound on reads over the default cap")
	}
	for i := 0; i < cap(h.largeBodies); i++ {
		h.largeBodies <- struct{}{}
	}
	big := `{"pad":"` + strings.Repeat("x", 2048) + `"}`

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signedRequest(t, big))
	if rec.Code != http.StatusServiceUnavailable || len(eng.calls) != 0 {
		t.Fatalf("with every large-read turn taken, a large delivery answered %d with %d staged", rec.Code, len(eng.calls))
	}
	if !strings.Contains(logs.String(), `msg="inbound receiver: request refused, too many large bodies being read"`) ||
		!strings.Contains(logs.String(), "source=acme") {
		t.Errorf("the busy refusal was not logged with its source:\n%s", logs.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, signedRequest(t, `{"event":"ping"}`))
	if rec.Code != http.StatusAccepted || len(eng.calls) != 1 {
		t.Fatalf("a body under the default cap waited for a large-read turn: answered %d with %d staged", rec.Code, len(eng.calls))
	}

	<-h.largeBodies
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, signedRequest(t, big))
	if rec.Code != http.StatusAccepted || len(eng.calls) != 2 {
		t.Fatalf("with a turn free, a large delivery answered %d with %d staged", rec.Code, len(eng.calls))
	}
	if got, want := len(h.largeBodies), cap(h.largeBodies)-1; got != want {
		t.Errorf("the large read kept its turn after it finished: %d turns taken, want %d", got, want)
	}
}

// The env spelling of a source's own cap, and the fail-closed rule for it: a
// cap that is not a positive number of bytes costs the source its slot, like
// every other setting the receiver cannot make sense of, rather than being
// quietly read as "no cap of its own".
func TestLoadConfigReadsASourcesOwnBodyCap(t *testing.T) {
	configure := func(t *testing.T, maxBody string) {
		t.Setenv("MEMQL_INBOUND_SOURCE_ALLOWLIST", "acme")
		t.Setenv("MEMQL_INBOUND_SOURCE_ACME_SIGNATURE_SCHEME", SchemeHMACSHA256Hex)
		t.Setenv("MEMQL_INBOUND_SOURCE_ACME_SECRET", "shh")
		t.Setenv("MEMQL_INBOUND_SOURCE_ACME_SIGNATURE_HEADER", "X-Sig")
		t.Setenv("MEMQL_INBOUND_SOURCE_ACME_MAX_BODY_BYTES", maxBody)
	}
	t.Run("a positive cap", func(t *testing.T) {
		configure(t, " 26214400 ")
		src, ok := LoadConfig(quietLogger()).Sources["acme"]
		if !ok || src.MaxBodyBytes != 26214400 {
			t.Fatalf("MAX_BODY_BYTES did not resolve: admitted=%v cap=%d", ok, src.MaxBodyBytes)
		}
	})
	t.Run("unset", func(t *testing.T) {
		configure(t, "")
		src, ok := LoadConfig(quietLogger()).Sources["acme"]
		if !ok || src.MaxBodyBytes != 0 {
			t.Fatalf("an unset MAX_BODY_BYTES must leave the receiver's default in force: admitted=%v cap=%d", ok, src.MaxBodyBytes)
		}
	})
	for _, bad := range []string{"lots", "0", "-1", "1.5"} {
		t.Run("MAX_BODY_BYTES="+bad, func(t *testing.T) {
			configure(t, bad)
			if src, ok := LoadConfig(quietLogger()).Sources["acme"]; ok {
				t.Errorf("a source whose cap is %q was admitted: %+v", bad, src)
			}
		})
	}
}
