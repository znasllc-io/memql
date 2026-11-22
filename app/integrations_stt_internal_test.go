//go:build !planner

package app

import (
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// The STT bootstrap used to resolve an OpenAI API key out of the process
// environment through a prefix-elision chain (#1371). Epic memql#5088 removed
// every manually entered vendor key from the product, so there is no chain and
// no key: transcription authenticates with the same federated bearer every
// other OpenAI consumer on the node uses, obtained from the engine.
//
// What is worth pinning is the REFUSAL. Both STT paths must decline to start
// when no bearer is available, because the alternative -- dialing OpenAI's
// Realtime WebSocket with an empty Authorization header -- fails as a
// handshake error on a path nobody watches, minutes after boot, rather than as
// one line at startup saying transcription is off.
func TestSTTBearerIsAbsentWithoutAnEngine(t *testing.T) {
	a := &App{}

	bearer, ok := a.openAIBearerForSTT()
	if ok {
		t.Fatal("a node with no engine reported an OpenAI bearer")
	}
	if bearer != nil {
		t.Error("the refusal returned a non-nil bearer function, which a caller would then call")
	}
}

// TestSTTBearerIsAbsentWithoutFederation is the same refusal one layer in: an
// engine is present, but no OpenAI provider on it carries federation ids. That
// is the state of every freshly installed cluster, local or cloud, so it is
// the common case rather than an edge one.
//
// The engine is a zero value rather than a booted one on purpose: this asserts
// the guard, and booting an engine here would need a database and would make a
// unit test of the STT wiring depend on one.
func TestSTTBearerIsAbsentWithoutFederation(t *testing.T) {
	a := &App{engine: &memql.MemQLEngine{}}

	if _, ok := a.openAIBearerForSTT(); ok {
		t.Fatal("a cluster with no OpenAI federation reported a bearer")
	}
}
