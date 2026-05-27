// Package nodetoken implements the row-persistence side of the node-
// class JWT credential family. Distinct from workertoken/ + pat/ in
// that the credential material itself (the JWT) is minted by the
// JWTIssuer, not by this package -- node tokens are self-validating
// via JWKS, so there's no Mint() that needs to hand back a plaintext
// bearer. What this package adds is the v1:identity:identity row that
// makes the credential visible to /admin/tokens + revocable via the
// verifier's StreamInterceptorWithNodeRevocation path.
//
// Why row persistence exists at all: pre-#343 the /node/bootstrap
// endpoint minted JWTs with a synthetic `sub` claim
// (`v1:identity:identity:node:<nodeType>:<nodeId>`) and never wrote
// a row. The verifier trusted the signed claim so peer auth worked,
// but three operational gaps remained:
//
//  1. /admin/tokens couldn't list bootstrap-minted tokens (no row to
//     walk).
//  2. No per-token revocation -- operators had to rotate the bootstrap
//     secret and wait up to the token TTL (30 days) for outstanding
//     tokens to expire.
//  3. No persistent audit trail (issuance logs roll over).
//
// memql#343 closes those by persisting a row keyed on the same
// canonical id the synthetic shape already produced; the JWT subject
// claim continues to point to the row, but now the row actually
// exists.
//
// Per-credential shape:
//
//   - id:         `v1:identity:identity:node:<nodeType>:<nodeId>`
//                  (deterministic; re-bootstrap of the same node hits
//                  the same row)
//   - identityType: `node_token`
//   - credentials.keyHash: the JWT `jti` claim of the most-recent mint
//                          (informational; JWT validation goes through
//                          JWKS, not the store)
//   - credentials.bootstrappedAt / bootstrappedFrom: preserved across
//                  re-bootstraps; record origin (timestamp + source IP)
//   - credentials.lastBootstrappedAt: updated on every re-bootstrap
//   - credentials.revokedAt: stamped when operator clicks revoke;
//                  the verifier's NodeRevocationCheck reads this on
//                  every NodeService.Stream open
//
// Operator-CLI-minted tokens (the original #105 path) go through
// JWTIssuer.IssueNodeAccessToken directly and don't use this package;
// they're served by the existing PAT-style mint flow.
package nodetoken
