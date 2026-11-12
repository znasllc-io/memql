package httpstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDownloadSurvivesOrdinaryServerWriteDeadline(t *testing.T) {
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writer, request, finish, err := Download(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		defer finish()
		deadline, ok := request.Context().Deadline()
		if !ok || time.Until(deadline) > downloadLimit {
			t.Error("transfer has no bounded lifetime")
		}
		_, _ = io.WriteString(writer, "first-")
		_ = http.NewResponseController(writer).Flush()
		time.Sleep(75 * time.Millisecond)
		_, _ = io.WriteString(writer, "last")
	}))
	s.Config.WriteTimeout = 20 * time.Millisecond
	s.Start()
	defer s.Close()
	response, err := s.Client().Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "first-last" {
		t.Fatalf("active transfer was cut off: %q, %v", body, err)
	}
}

func TestDownloadPreservesEarlierDeadlineAndCancellation(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Minute))
	defer cancel()
	original, _ := ctx.Deadline()
	w, r, finish, err := Download(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil).WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	if got, _ := r.Context().Deadline(); got != original {
		t.Fatal("download widened the caller's lifetime")
	}
	cancel()
	if _, err := w.Write([]byte("late")); !errors.Is(err, context.Canceled) {
		t.Fatalf("write ignored cancellation: %v", err)
	}
}

// A caller-chosen ceiling replaces the transfer lifetime and nothing else;
// Download keeps today's ceiling for every caller that did not choose one.
func TestDownloadWithinSetsTheChosenCeiling(t *testing.T) {
	_, r, finish, err := DownloadWithin(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), LongDownloadLimit)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	deadline, ok := r.Context().Deadline()
	if !ok || time.Until(deadline) <= downloadLimit || time.Until(deadline) > LongDownloadLimit {
		t.Fatalf("ceiling %v, want just under %v", time.Until(deadline), LongDownloadLimit)
	}
	if LongDownloadLimit != 6*time.Hour {
		t.Fatalf("LongDownloadLimit = %v, want 6h", LongDownloadLimit)
	}

	_, r, finish2, err := Download(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer finish2()
	deadline, ok = r.Context().Deadline()
	if !ok || time.Until(deadline) > downloadLimit || time.Until(deadline) < downloadLimit-time.Minute {
		t.Fatalf("Download's ceiling %v, want %v", time.Until(deadline), downloadLimit)
	}
}

// The idle write deadline is the same under a long ceiling: an active transfer
// survives the server's ordinary write deadline.
func TestDownloadWithinKeepsTheIdleWriteDeadline(t *testing.T) {
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writer, _, finish, err := DownloadWithin(w, r, LongDownloadLimit)
		if err != nil {
			t.Error(err)
			return
		}
		defer finish()
		_, _ = io.WriteString(writer, "first-")
		_ = http.NewResponseController(writer).Flush()
		time.Sleep(75 * time.Millisecond)
		_, _ = io.WriteString(writer, "last")
	}))
	s.Config.WriteTimeout = 20 * time.Millisecond
	s.Start()
	defer s.Close()
	response, err := s.Client().Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "first-last" {
		t.Fatalf("active transfer was cut off: %q, %v", body, err)
	}
}
