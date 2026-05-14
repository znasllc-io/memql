//go:build !identity

package app

// verifierRequired tells configAndAuth + transportBase whether this
// binary must have IDENTITY_VERIFIER_BASE_URL configured. True for
// every node-type that consumes identity-issued JWTs (bff, voice,
// cognition, agent, planner, default). The identity binary itself
// is the JWKS authority -- it does not verify against itself --
// and overrides this to false in auth_mode_identity.go.
const verifierRequired = true
