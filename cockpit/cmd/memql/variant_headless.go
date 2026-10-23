//go:build !computeruse

package main

// buildVariant names which build of the one `memql` command this is.
// The command name never changes with the variant (spec D4); the
// variant is an install-time choice surfaced by `memql --version` and
// by the worker's capability registration.
const buildVariant = "headless"
