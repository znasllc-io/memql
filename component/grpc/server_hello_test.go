package memql

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// TestServerHelloNamesTheLanguage: the handshake tells a client which MemQL
// language this node speaks -- the edition, the grammar, and the first release
// of the VS Code extension that carries that grammar (memql#5362, D25).
//
// The extension compares the three with the language it was built from and,
// when the cluster is newer, names the release to install. That release can
// only come from the cluster: an older editor has never heard of it.
//
// DB-free, like the DslSpec handler tests beside it: the handshake reads build
// facts and nothing else.
func TestServerHelloNamesTheLanguage(t *testing.T) {
	s, cs := newSpecSession(t)
	env := &memqlv1.MemqlClientMessage{
		MessageId: "m1",
		Payload: &memqlv1.MemqlClientMessage_ClientHello{
			ClientHello: &memqlv1.ClientHello{ClientId: "memql-vscode", SdkName: "memql-vscode"},
		},
	}
	require.NoError(t, s.handleClientHello(env, env.GetClientHello()))

	hello := cs.lastSent().GetServerHello()
	require.NotNil(t, hello, "the reply to a ClientHello must be a ServerHello")

	assert.Equal(t, langparser.Edition, hello.GetEdition(), "edition")
	assert.Equal(t, langparser.GrammarVersion, hello.GetGrammarVersion(), "grammar_version")
	assert.Equal(t, langparser.EditorRelease, hello.GetEditorRelease(), "editor_release")

	// The fields beside them keep their meanings: `version` is the wire
	// protocol, and none of the new fields may be mistaken for it.
	assert.Equal(t, "v1", hello.GetVersion(), "version stays the wire protocol")
}
