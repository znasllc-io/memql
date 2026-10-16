package ociregistry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func taggedPublisher(t *testing.T, origin string) *Publisher {
	t.Helper()
	p, err := NewPublisher(Target{Origin: origin, Repository: "receipts/image", Tag: "v0.29.7", AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVersionTagPublicationAndFreshHostReplay(t *testing.T) {
	v, want := verifiedFixture(t)
	handler := quietRegistry()
	var writes atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			writes.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer s.Close()
	for attempt := 0; attempt < 2; attempt++ {
		before := writes.Load()
		receipt, err := taggedPublisher(t, s.URL).Publish(t.Context(), v)
		if err != nil || receipt.Tag != "v0.29.7" || receipt.ImageDigest != want.ImageDigest {
			t.Fatalf("tagged publication: %+v, %v", receipt, err)
		}
		if attempt == 0 && writes.Load() == 0 || attempt == 1 && writes.Load() != before {
			t.Fatal("initial publish must write; fresh-host replay must only read")
		}
	}
}

func TestVersionTagCollisionRefusesBeforeAnyWrite(t *testing.T) {
	v, _ := verifiedFixture(t)
	handler := quietRegistry()
	var writes atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			writes.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer s.Close()
	if _, err := taggedPublisher(t, s.URL).Publish(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	raw, want := fixture(t, fixtureOptions{mutateConfig: func(c map[string]any) { c["config"] = map[string]any{"Env": []string{"DIFFERENT=true"}} }})
	other, err := Verify(t.Context(), strings.NewReader(string(raw)), want, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	before := writes.Load()
	if _, err := taggedPublisher(t, s.URL).Publish(t.Context(), other); !errors.Is(err, ErrTagConflict) {
		t.Fatal("different content did not refuse", err)
	}
	if writes.Load() != before {
		t.Fatal("tag collision uploaded content")
	}
}

func TestVersionTagLostReplyReconcilesWithoutRepeatingWrite(t *testing.T) {
	for _, unreadable := range []bool{false, true} {
		t.Run(map[bool]string{false: "readback", true: "second-host"}[unreadable], func(t *testing.T) {
			v, _ := verifiedFixture(t)
			handler := quietRegistry()
			var writes atomic.Int64
			var unavailable atomic.Bool
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/manifests/v0.29.7") {
					if r.Method == "PUT" {
						writes.Add(1)
						response := httptest.NewRecorder()
						handler.ServeHTTP(response, r)
						if response.Code != http.StatusCreated {
							t.Errorf("tag write failed: %d", response.Code)
						}
						unavailable.Store(unreadable)
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = connection.Close()
						return
					}
					if unavailable.Load() {
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
						return
					}
				}
				handler.ServeHTTP(w, r)
			}))
			defer s.Close()
			_, err := taggedPublisher(t, s.URL).Publish(t.Context(), v)
			if unreadable && !errors.Is(err, ErrUncertain) || !unreadable && err != nil {
				t.Fatal("wrong lost-response outcome", err)
			}
			unavailable.Store(false)
			if _, err := taggedPublisher(t, s.URL).Publish(t.Context(), v); err != nil {
				t.Fatal(err)
			}
			if writes.Load() != 1 {
				t.Fatalf("tag write repeated %d times", writes.Load())
			}
		})
	}
}

func TestTagTransportCannotBroadenOrEnterAvailability(t *testing.T) {
	p := taggedPublisher(t, "http://127.0.0.1:1234")
	s := &scopedTransport{publisher: p, image: sha([]byte("image")), tag: "v0.29.7"}
	for _, path := range []string{"v0.29.8", "latest", "v0.29.7?anything=true"} {
		r, _ := http.NewRequestWithContext(context.Background(), "PUT", p.target.Origin+"/v2/receipts/image/manifests/"+path, nil)
		if _, err := s.permitted(r); err == nil {
			t.Fatal("unapproved tag allowed", path)
		}
	}
	s.readOnly = true
	for _, method := range []string{"GET", "PUT"} {
		r, _ := http.NewRequestWithContext(context.Background(), method, p.target.Origin+"/v2/receipts/image/manifests/v0.29.7", nil)
		if _, err := s.permitted(r); err == nil {
			t.Fatal("tag entered immutable availability", method)
		}
	}
	for _, tag := range []string{"../x", "v0.29.7?x", "v0.29.7\n", "sha256:abc", strings.Repeat("a", 129)} {
		if _, err := NewPublisher(Target{Origin: p.target.Origin, Repository: p.target.Repository, Tag: tag, AllowLoopbackHTTP: true}); err == nil {
			t.Fatal("invalid tag accepted", tag)
		}
	}
}
