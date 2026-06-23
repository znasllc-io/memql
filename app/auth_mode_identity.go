//go:build identity

package app

// verifierRequired is false on the identity binary -- identity IS
// the JWKS authority, so it does not verify against itself. Identity
// nodes have MEMQL_IDENTITY_VERIFIER_BASE_URL unset; configAndAuth skips
// the per-node verifier setup and transportBase falls back to a
// minimal gRPC chain (operator-aware credential only -- nothing
// else has business hitting identity's MemqlService.Stream
// directly).
const verifierRequired = false
