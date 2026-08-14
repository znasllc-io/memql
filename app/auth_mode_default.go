//go:build !identity && !edge

package app

// verifierRequired tells configAndAuth + transportBase whether this
// binary must have MEMQL_IDENTITY_VERIFIER_BASE_URL configured. True for
// every node-type that consumes identity-issued JWTs (bff, voice,
// cognition, agent, planner, default). Two node types override this to
// false, each for its own reason, and each in its own file: the identity
// binary is the JWKS authority and does not verify against itself
// (auth_mode_identity.go); the edge is not an auth boundary at all --
// it serves public bytes to anonymous visitors (auth_mode_edge.go). Both
// exclusions belong in THIS file's build constraint, not just the other
// two files' -- three files declaring the same const with overlapping
// build tags is a duplicate-declaration compile error, not a precedence
// rule Go resolves for you.
const verifierRequired = true
