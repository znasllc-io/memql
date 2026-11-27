package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/znasllc-io/memql/component/calls"
)

func TestMediaSignalDiagnosticsNormalSocketCloseIsNotFailure(t *testing.T) {
	for _, phase := range []string{"client_read", "provider_read"} {
		for _, err := range []error{io.EOF, net.ErrClosed, &websocket.CloseError{Code: 1000, Text: "PRIVATE"}, &websocket.CloseError{Code: 1001, Text: "PRIVATE"}} {
			var observed error
			a := &calls.MediaSignalAdmission{Observe: func(_ string, _ time.Duration, e error) { observed = e }}
			mediaSignalObserve(a, phase, time.Now(), err)
			if observed != nil {
				t.Fatal("normal socket shutdown became a failure")
			}
		}
	}
	var observed error
	a := &calls.MediaSignalAdmission{Observe: func(_ string, _ time.Duration, e error) { observed = e }}
	mediaSignalObserve(a, "provider_read", time.Now(), &websocket.CloseError{Code: 1011, Text: "PRIVATE"})
	if observed == nil {
		t.Fatal("abnormal provider failure was hidden")
	}
}

func TestMediaSignalDiagnosticsConfirmationFailureFinishesOneReceipt(t *testing.T) {
	kind, first := signalJoin(t, false)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(kind, first)
		_, _, _ = conn.ReadMessage()
	}))
	defer provider.Close()
	a := signalAdmission(provider.URL)
	var mu sync.Mutex
	phases := map[string]int{}
	finished := make(chan struct{}, 1)
	a.Observe = func(phase string, _ time.Duration, _ error) { mu.Lock(); phases[phase]++; mu.Unlock() }
	a.Confirm = func(context.Context) error { return context.DeadlineExceeded }
	closed := false
	a.Close = func(_ context.Context, definitive bool) error {
		mu.Lock()
		closed = definitive
		mu.Unlock()
		return nil
	}
	a.Finish = func() { finished <- struct{}{} }
	h := httptest.NewServer(NewMediaSignalHandler(signalServiceFunc(func(context.Context, string) (*calls.MediaSignalAdmission, error) { return a, nil })))
	defer h.Close()
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.URL, "http")+"/media/rtc?access_token=issued", nil)
	if conn != nil {
		conn.Close()
		t.Fatal("unconfirmed admission was released")
	}
	if err == nil || response == nil || response.StatusCode != 404 {
		t.Fatal("confirmation refusal changed")
	}
	response.Body.Close()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("terminal diagnostic did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	if !closed || phases["upstream_dial"] != 1 || phases["upstream_initial"] != 1 || phases["downstream_upgrade"] != 0 {
		t.Fatal("diagnostic phase sequence changed")
	}
	select {
	case <-finished:
		t.Fatal("duplicate terminal receipt")
	default:
	}
}
