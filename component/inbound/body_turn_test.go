package inbound

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a log sink the server's goroutine writes and the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// largeTurnHandler is a receiver whose acme source may carry 4096 bytes over
// the receiver's 1024. Its pacing is generous -- a five-second grace, thirty to
// hold -- so a test that only needs the deadlines in force passes on a starved
// build node; the test that watches a turn being lost shortens the grace itself.
func largeTurnHandler(t *testing.T, logs *syncBuffer) *Handler {
	t.Helper()
	src := hexSource()
	src.MaxBodyBytes = 4096
	h := testHandler(t, &fakeEngine{}, src)
	h.logger = slog.New(slog.NewTextHandler(logs, nil))
	h.largeBodyWait = time.Millisecond
	h.largeBodyGrace, h.largeBodyRate, h.largeBodyHold = 5*time.Second, 64<<10, 30*time.Second
	return h
}

// A request that would take a large-body turn must carry its source's
// signature header: without one it cannot be verified whatever its body says,
// so it is refused 401 before a byte of the body is read and before any turn
// is asked for. An unsigned crowd therefore cannot hold the turns at all --
// proved here with every turn already taken, where asking for one would have
// answered 503.
func TestAnUnsignedLargeRequestIsRefusedBeforeItsBodyOrATurn(t *testing.T) {
	var logs syncBuffer
	h := largeTurnHandler(t, &logs)
	for i := 0; i < cap(h.largeBodies); i++ {
		h.largeBodies <- struct{}{}
	}
	body := &countingReader{r: strings.NewReader(strings.Repeat("x", 2048))}
	r := httptest.NewRequest(http.MethodPost, "/inbound/acme", body)
	r.ContentLength = 2048
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unsigned 2048-byte request answered %d, want 401 before any turn", rec.Code)
	}
	if body.n != 0 {
		t.Errorf("read %d bytes of a request that carried no signature header", body.n)
	}
}

// A request that finds a turn free gets it, even when its wait has already run
// out by the time it asks -- which is what a throttled container does to a
// goroutine between arming its timer and reaching the select. A zero wait makes
// that moment certain: the timer is due before the select runs, so a select
// over both would pick at random and give away about half the free turns.
func TestAFreeTurnIsTakenEvenWhenItsWaitHasAlreadyRunOut(t *testing.T) {
	var logs syncBuffer
	h := largeTurnHandler(t, &logs)
	h.largeBodyWait = 0
	for i := 0; i < 200; i++ {
		if !h.takeLargeBodyTurn(context.Background()) {
			t.Fatalf("attempt %d: a free turn was refused because the wait had run out", i+1)
		}
		<-h.largeBodies
	}
}

// A body holding a large-body turn has to keep arriving. Trickling bytes is the
// cheap way to hold a turn for as long as the server lets a request read (15
// seconds by default), and two such senders would starve every large delivery.
// A body that falls behind largeBodyRate once largeBodyGrace has passed loses
// its turn: the read is cut, the request is answered 408 and logged, and the
// turn is free again.
//
// The hold stays at thirty seconds and the trickle would need forty to finish,
// so an answer inside the five-second bound below can only be the pace cutting
// it -- with room for a throttled build node to be slow about saying so.
func TestALargeBodyThatStopsArrivingLosesItsTurn(t *testing.T) {
	var logs syncBuffer
	h := largeTurnHandler(t, &logs)
	h.largeBodyGrace = 50 * time.Millisecond
	srv := httptest.NewServer(h)
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	full := strings.Repeat("x", 2048)
	fmt.Fprintf(conn, "POST /inbound/acme HTTP/1.1\r\nHost: receiver\r\nContent-Type: application/json\r\n"+
		"X-Sig: %s\r\nContent-Length: 2048\r\n\r\n", hex.EncodeToString(sign(testSecret, []byte(full))))
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				if _, err := conn.Write([]byte("x")); err != nil {
					return
				}
			}
		}
	}()
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	started := time.Now()
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer to a trickling body: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("a body trickling a byte every 20ms answered %d, want 408", resp.StatusCode)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Errorf("the trickling body held its turn for %s; its pace should have cut it within moments of a 50ms grace", took)
	}
	if n := len(h.largeBodies); n != 0 {
		t.Errorf("%d turn(s) still taken after the slow read was cut", n)
	}
	if !strings.Contains(logs.String(), `msg="inbound receiver: request refused, body arrived too slowly"`) ||
		!strings.Contains(logs.String(), "source=acme") {
		t.Errorf("the slow refusal was not logged with its source:\n%s", logs.String())
	}
}

// The pacing costs a delivery that arrives at speed nothing: through a real
// server, where the read deadlines are in force, a large body sent whole is
// staged and its turn returned.
func TestALargeBodyAtSpeedIsStagedThroughARealServer(t *testing.T) {
	var logs syncBuffer
	h := largeTurnHandler(t, &logs)
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := `{"pad":"` + strings.Repeat("x", 3000) + `"}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/inbound/acme", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Sig", hex.EncodeToString(sign(testSecret, []byte(body))))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a %d-byte body sent whole answered %d, want 202:\n%s", len(body), resp.StatusCode, logs.String())
	}
	if n := len(h.largeBodies); n != 0 {
		t.Errorf("%d turn(s) still taken after the request was answered", n)
	}
}
