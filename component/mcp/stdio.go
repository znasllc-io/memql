package mcp

import (
	"io"
	"os"
)

// realStdout holds the process's real stdout file once RedirectStdoutForStdio
// has run. The MCP JSON-RPC protocol owns stdout exclusively, so the redirect
// repoints os.Stdout at stderr and the protocol head writes here instead.
var realStdout *os.File

// RedirectStdoutForStdio captures the process's real stdout for the MCP
// JSON-RPC protocol and repoints os.Stdout at os.Stderr, so any component
// that logs to "stdout" -- the service logger (main.go binds it to os.Stdout),
// or a stray fmt.Println -- cannot corrupt the wire.
//
// It MUST run once, before the service logger is constructed; the mcp build
// installs it from a build-tagged init in package main, so it is not active
// (and cannot interfere) in any non-mcp build or in this package's own tests.
// Idempotent.
func RedirectStdoutForStdio() {
	if realStdout != nil {
		return
	}
	realStdout = os.Stdout
	os.Stdout = os.Stderr
}

// StdioStreams returns the reader and writer the MCP stdio transport binds
// to: the process's real stdin, and the real stdout captured by
// RedirectStdoutForStdio. When the redirect was not installed (e.g. tests),
// it falls back to the current os.Stdin / os.Stdout.
func StdioStreams() (io.Reader, io.Writer) {
	if realStdout != nil {
		return os.Stdin, realStdout
	}
	return os.Stdin, os.Stdout
}
