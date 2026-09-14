package client

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// handshakeAgainst runs Connection.handshake over a mock stream that answers
// the ClientHello with hello, and returns the connection it filled in.
func handshakeAgainst(t *testing.T, hello *memqlv1.ServerHello) *Connection {
	t.Helper()
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	t.Cleanup(d.Stop)

	c := &Connection{dispatcher: d}
	errCh := make(chan error, 1)
	go func() { errCh <- c.handshake(t.Context()) }()

	var sent *memqlv1.MemqlClientMessage
	select {
	case sent = <-stream.sendCh:
	case <-time.After(2 * time.Second):
		t.Fatal("the ClientHello never went out")
	}
	require.NotNil(t, sent.GetClientHello(), "the handshake must open with a ClientHello")
	stream.recvCh <- &memqlv1.MemqlServerMessage{
		CorrelateTo: sent.GetMessageId(),
		Payload:     &memqlv1.MemqlServerMessage_ServerHello{ServerHello: hello},
	}
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("the handshake never finished")
	}
	return c
}

// TestHandshakeRecordsTheLanguageTheNodeSpeaks: the three language fields
// ServerHello carries (memql#5362) land on the Connection beside the build
// facts, so a Go client can compare the cluster's grammar with its own.
func TestHandshakeRecordsTheLanguageTheNodeSpeaks(t *testing.T) {
	c := handshakeAgainst(t, &memqlv1.ServerHello{
		NodeId:         "bff",
		Version:        "v1",
		EngineVersion:  "v0.20.0",
		EngineCommit:   "0123456789ab",
		Edition:        "2026",
		GrammarVersion: "2026.09-example-grammar-0123abcd",
		EditorRelease:  "0.4.0",
	})

	assert.Equal(t, "bff", c.NodeId)
	assert.Equal(t, "v1", c.Version)
	assert.Equal(t, "v0.20.0", c.EngineVersion)
	assert.Equal(t, "0123456789ab", c.EngineCommit)
	assert.Equal(t, "2026", c.Edition)
	assert.Equal(t, "2026.09-example-grammar-0123abcd", c.GrammarVersion)
	assert.Equal(t, "0.4.0", c.EditorRelease)
}

// TestHandshakeAgainstAnOlderNodeLeavesTheLanguageEmpty: a node that predates
// the fields states none of them, and the empty string is how a caller tells
// "older than this contract" apart from a value -- the same convention
// EngineVersion uses.
func TestHandshakeAgainstAnOlderNodeLeavesTheLanguageEmpty(t *testing.T) {
	c := handshakeAgainst(t, &memqlv1.ServerHello{NodeId: "bff", Version: "v1", EngineVersion: "v0.19.0"})

	assert.Equal(t, "", c.Edition)
	assert.Equal(t, "", c.GrammarVersion)
	assert.Equal(t, "", c.EditorRelease)
}
