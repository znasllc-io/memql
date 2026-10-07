package client

import (
	"context"
	"crypto/x509"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCustomRootsCannotSilentlySelectPlaintext(t *testing.T) {
	_, err := Connect(context.Background(), ConnectConfig{Endpoint: "http://127.0.0.1:1", RootCAs: x509.NewCertPool()})
	require.ErrorContains(t, err, "require a TLS endpoint")
}

func TestConnectDeadlineBoundsStalledTLS(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		conn, err := Connect(ctx, ConnectConfig{Endpoint: "https://" + listener.Addr().String()})
		if conn != nil {
			conn.Close()
		}
		done <- err
	}()
	select {
	case conn := <-accepted:
		defer conn.Close() // accept TCP but never answer its TLS handshake
	case <-time.After(time.Second):
		t.Fatal("connection did not reach the fixture")
	}
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(time.Second):
		t.Fatal("Connect ignored the caller deadline while opening its stream")
	}
}
