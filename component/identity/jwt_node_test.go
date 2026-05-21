package identity_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/identity"
)

// TestIssueNodeAccessToken_StampsClassAndBinding pins the load-
// bearing contract for #105: the node-class mint path stamps
// class="node" + the bound nodeId + nodeType. Downstream the
// verifier reads those claims and the NodeService interceptor
// pins the wire to class="node" tokens with a matching
// NodeHello.
func TestIssueNodeAccessToken_StampsClassAndBinding(t *testing.T) {
	dir := t.TempDir()
	km, err := identity.NewKeyManager(dir, "")
	require.NoError(t, err)
	require.NoError(t, km.Load())
	cfg := identity.Config{
		Enabled:     true,
		BaseURL:     "https://identity.test",
		JWTAudience: "memql",
		KeyDir:      dir,
	}
	iss, err := identity.NewJWTIssuer(km, cfg)
	require.NoError(t, err)

	now := time.Now().UTC()
	tok, exp, err := iss.IssueNodeAccessToken(identity.NodeIssueInput{
		IdentityId: "v1:identity:identity:node-cog-1",
		NodeId:     "v1:cluster:node:cognition-1",
		NodeType:   "cognition",
	}, now)
	require.NoError(t, err)
	require.NotEmpty(t, tok)
	assert.True(t, exp.After(now), "exp must be in the future")

	// Re-verify against the same issuer and confirm the claims.
	claims, err := iss.VerifyAccessToken(tok, now.Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, identity.ClassNode, claims.Class)
	assert.Equal(t, "v1:cluster:node:cognition-1", claims.NodeId)
	assert.Equal(t, "cognition", claims.NodeType)
	assert.Equal(t, "v1:identity:identity:node-cog-1", claims.Subject)
}

// TestIssueNodeAccessToken_RejectsEmptyFields locks the input
// validation contract: missing IdentityId / NodeId / NodeType all
// fail at the mint surface. The interceptor would catch them
// downstream too, but failing loud at mint stops a misuse from
// shipping a bad token in the first place.
func TestIssueNodeAccessToken_RejectsEmptyFields(t *testing.T) {
	dir := t.TempDir()
	km, err := identity.NewKeyManager(dir, "")
	require.NoError(t, err)
	require.NoError(t, km.Load())
	iss, err := identity.NewJWTIssuer(km, identity.Config{
		Enabled:     true,
		BaseURL:     "https://identity.test",
		JWTAudience: "memql",
		KeyDir:      dir,
	})
	require.NoError(t, err)

	cases := []struct {
		name string
		in   identity.NodeIssueInput
	}{
		{"missing identityId", identity.NodeIssueInput{NodeId: "v1:cluster:node:x", NodeType: "bff"}},
		{"missing nodeId", identity.NodeIssueInput{IdentityId: "v1:identity:identity:x", NodeType: "bff"}},
		{"missing nodeType", identity.NodeIssueInput{IdentityId: "v1:identity:identity:x", NodeId: "v1:cluster:node:x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := iss.IssueNodeAccessToken(tc.in, time.Now().UTC())
			require.Error(t, err)
		})
	}
}

// TestIssueAccessToken_DefaultsToEmptyClass keeps existing user
// tokens unchanged: the class claim is the omit-empty case for the
// default mint path, so pre-#105 verifiers (which don't read it)
// don't see a new key on the JWT.
func TestIssueAccessToken_DefaultsToEmptyClass(t *testing.T) {
	dir := t.TempDir()
	km, err := identity.NewKeyManager(dir, "")
	require.NoError(t, err)
	require.NoError(t, km.Load())
	iss, err := identity.NewJWTIssuer(km, identity.Config{
		Enabled:     true,
		BaseURL:     "https://identity.test",
		JWTAudience: "memql",
		KeyDir:      dir,
	})
	require.NoError(t, err)

	now := time.Now().UTC()
	tok, _, err := iss.IssueAccessToken(identity.IssueInput{
		UserId: "v1:identity:user:alice",
		Email:  "alice@example.com",
	}, now)
	require.NoError(t, err)

	claims, err := iss.VerifyAccessToken(tok, now.Add(time.Minute))
	require.NoError(t, err)
	assert.Empty(t, claims.Class, "user-class tokens omit the class claim for backward compat")
	assert.Empty(t, claims.NodeId)
	assert.Empty(t, claims.NodeType)
}
