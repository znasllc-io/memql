//go:build edge

package main

// The edge binary needs no init-time special casing -- this file has no
// init() and adds no behaviour. It exists only so the `edge` build tag has a
// main package file to select (most node types have none; main.go alone
// covers them, since it carries no build tag of its own), and so a reader
// grepping for a node type finds its app/build_edge.go, app/transport_edge.go
// and main_edge.go together. main_mcp.go is the one other file at this path,
// and it exists for a real reason (redirecting stdout before the stdio
// JSON-RPC protocol claims it) -- not because of a convention this file
// follows.
