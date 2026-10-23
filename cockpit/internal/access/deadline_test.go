package access

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/config"
)

// The round-trip deadline must NOT cover the sign-in.
//
// EnsureValidToken is the interactive one here -- a person typed this command,
// so an expired token opens a browser. That is human-paced: find the window,
// pick an account, approve. Twenty seconds is a perfectly good ceiling for
// "the cluster did not answer" and a terrible one for "the human has not
// finished logging in", where it would kill the sign-in partway through with a
// deadline error naming nothing the person did wrong.
func TestFetchDoesNotPutTheSignInUnderTheRoundTripDeadline(t *testing.T) {
	var seen context.Context
	stubToken(t, func(ctx context.Context, _ config.ClusterConfig) (string, error) {
		seen = ctx
		return "", errStop
	})

	_, err := Fetch(context.Background(), config.ClusterConfig{
		Name: "acme", Endpoint: "https://api.acme.test",
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("Fetch returned %v, want the stub's error", err)
	}
	if seen == nil {
		t.Fatal("the sign-in was never reached")
	}
	if deadline, ok := seen.Deadline(); ok {
		t.Errorf("the sign-in ran under a deadline %s away; it must inherit the caller's context",
			time.Until(deadline).Round(time.Second))
	}
}

// The round trip, by contrast, MUST be bounded -- a person is waiting at a
// prompt, and a cluster that has not answered is a fact worth printing rather
// than something to keep waiting for.
//
// THE PEER IS A LOCAL SOCKET THAT ACCEPTS AND NEVER SPEAKS. That is the only
// shape that actually exercises the ceiling: an unresolvable hostname (the
// obvious choice, and what this test used to do) fails on NXDOMAIN in
// milliseconds, so it passes with the deadline deleted outright -- and it
// makes a real DNS query from `go test ./...`, which on a resolver with a
// wildcard redirect resolves, connects nowhere, and hangs the package binary
// until Go's ten-minute panic.
func TestFetchBoundsTheRoundTripAfterTheSignIn(t *testing.T) {
	endpoint := silentListener(t)
	stubToken(t, func(context.Context, config.ClusterConfig) (string, error) {
		return "token", nil
	})
	shortenTimeout(t, 400*time.Millisecond)

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := Fetch(context.Background(), config.ClusterConfig{Name: "acme", Endpoint: endpoint})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Fetch succeeded against a peer that never speaks")
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Errorf("Fetch took %s against a %s ceiling", elapsed, callTimeout)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Fetch never returned against a %s ceiling: the round trip is unbounded", callTimeout)
	}
}

var errStop = errors.New("stop before the network")

func stubToken(t *testing.T, fn func(context.Context, config.ClusterConfig) (string, error)) {
	t.Helper()
	restore := ensureToken
	t.Cleanup(func() { ensureToken = restore })
	ensureToken = fn
}

func shortenTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	restore := callTimeout
	t.Cleanup(func() { callTimeout = restore })
	callTimeout = d
}

// silentListener accepts connections and says nothing on them, holding each
// open so the client waits rather than seeing a reset. Hermetic: no DNS, no
// outbound packet, no dependency on what the local resolver does with an
// unknown name.
func silentListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				_ = c.Close()
			}
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, c)
		}
	}()
	return "http://" + ln.Addr().String()
}
