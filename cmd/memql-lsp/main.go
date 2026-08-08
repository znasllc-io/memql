// Command memql-lsp is the MemQL offline Language Server: a standalone binary
// that speaks LSP over stdio and serves .memql files from disk, with no cluster
// and no auth (handoff Part I §4). It exists so a .memql author gets first-class
// editing (highlighting, diagnostics, completion, hover, signature help) in
// VS Code, powered by the same MemQL Sense brain the Cockpit uses.
//
// Flags:
//
//	--root <dir>   workspace root (default: current working directory)
//	--stdio        communicate over stdin/stdout (default; the only transport)
//	--log <path>   write logs to this file; empty logs to stderr, NEVER stdout
//	               (stdout is the JSON-RPC channel)
//
// Subcommands:
//
//	gen-grammar <out.json>   generate the TextMate grammar from dslspec and write
//	                         it to <out.json> (regenerate on every GrammarVersion
//	                         bump); see internal/grammar.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/tliron/commonlog"
	_ "github.com/tliron/commonlog/simple" // registers the default logging backend
	glspserver "github.com/tliron/glsp/server"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	// Subcommands are dispatched before the server flag set.
	if len(args) > 0 && args[0] == "gen-grammar" {
		return runGenGrammar(args[1:])
	}

	fs := flag.NewFlagSet(lsName, flag.ContinueOnError)
	var (
		root    = fs.String("root", "", "workspace root directory (default: current working directory)")
		stdio   = fs.Bool("stdio", true, "communicate over stdin/stdout (the only supported transport)")
		logPath = fs.String("log", "", "write logs to this file; empty logs to stderr (never stdout)")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Configure logging BEFORE anything can emit. The simple backend writes to
	// the given file, or to stderr when the path is empty -- never to stdout,
	// which carries the LSP JSON-RPC stream.
	if *logPath != "" {
		commonlog.Configure(1, logPath)
	} else {
		commonlog.Configure(1, nil)
	}
	log := commonlog.GetLogger(lsName)

	if !*stdio {
		fmt.Fprintln(os.Stderr, "ERROR: only the --stdio transport is supported")
		return 2
	}

	workspaceRoot := *root
	if workspaceRoot == "" {
		if cwd, err := os.Getwd(); err == nil {
			workspaceRoot = cwd
		}
	}

	srv := newServer(workspaceRoot, log)
	// newCustomHandler wraps the generated protocol.Handler so the custom
	// `memql/runnableConstructs` request is reachable; every standard LSP
	// method delegates through it untouched. See runnable.go.
	glspSrv := glspserver.NewServer(newCustomHandler(srv), lsName, false)
	if err := glspSrv.RunStdio(); err != nil {
		log.Errorf("memql-lsp exited with error: %s", err)
		return 1
	}
	return 0
}
