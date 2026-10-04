package memql

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/secret"
)

// memql#5480: the outbound worker fails a secret target permanently when the
// resolver MISSED (nobody stored the secret) or could not DECRYPT it, and
// retries it on any other error -- a timeout, a dropped connection, an engine
// still booting. Both answers are an operator's fix, and both have to be told
// apart from a lookup that failed by their type, never by their text.

func TestAResolverMissIsRecognisedByItsPredicate(t *testing.T) {
	miss := &notFoundVariableError{name: "DISCORD_X", kind: "secret"}
	require.True(t, IsVariableNotFound(miss))
	require.True(t, IsVariableNotFound(fmt.Errorf("lookup: %w", miss)), "a wrapped miss is still a miss")
	require.True(t, isNotFoundVariable(miss), "ResolveSecret's partition-then-global fallback reads the same predicate")
	require.False(t, IsSecretUndecryptable(miss))
	require.False(t, IsVariableNotFound(errors.New(`secret "DISCORD_X" not found`)),
		"the predicate reads the type: an error that merely says it is a miss is not one")
	require.False(t, IsVariableNotFound(nil))
}

func TestAnUndecryptableSecretIsRecognisedByItsPredicate(t *testing.T) {
	junk := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("not a ciphertext ", 3)))

	t.Setenv(secret.EnvMasterKey, strings.Repeat("ab", 32))
	_, err := secret.Decrypt(junk)
	require.Error(t, err, "precondition: a junk ciphertext does not open")
	wrapped := undecryptableSecret("DISCORD_X", err)
	require.True(t, IsSecretUndecryptable(wrapped))
	require.True(t, IsSecretUndecryptable(fmt.Errorf("resolve: %w", wrapped)), "a wrapped failure is still one")
	require.False(t, IsVariableNotFound(wrapped))
	require.Equal(t, `secret "DISCORD_X": `+err.Error(), wrapped.Error(), "the message an operator reads is unchanged")
	require.ErrorIs(t, wrapped, err, "the decrypt error stays in the chain")

	// A node with no master key cannot decrypt anything, which is the
	// operator's to fix as well.
	t.Setenv(secret.EnvMasterKey, "")
	_, err = secret.Decrypt(junk)
	require.Error(t, err)
	require.True(t, IsSecretUndecryptable(undecryptableSecret("DISCORD_X", err)))
}
