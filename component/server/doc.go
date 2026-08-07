// Package server implements memQL's HTTP surface: the documented exceptions
// to the gRPC-first endpoint policy (health probes, WebSocket upgrades, file
// uploads, inbound webhooks) plus the middleware they share.
//
// The handlers here are hand-written. This file previously carried two
// `//go:generate` directives claiming the package was generated from an
// OpenAPI spec by oapi-codegen; every path they named was absent from the
// repository -- the spec, the post-processor, and the `server.gen.go` they
// claimed to produce -- so `make generate` had been broken for some time and
// nobody ran it (memql#3215). They are removed rather than repaired: the
// package is not generated, and a directive is not the place to record an
// aspiration. If OpenAPI generation is wanted here, it is separate work to be
// filed as such.
//
// TestGoGenerateDirectivesReferenceExistingPaths (repository root) now holds
// the line for every directive in the tree, so the next one cannot rot
// silently.
package server
