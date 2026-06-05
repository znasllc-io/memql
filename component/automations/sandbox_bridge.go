package automations

// sandbox_bridge.go -- registers the automation-compile hook the Gate 1
// authoring sandbox (issue #956) calls for the `automation` construct kind.
//
// component/memql cannot import component/automations directly (automations
// imports memql -> cycle), so the sandbox exposes a hook seam and this
// package fills it from init(). The bridge wraps the loader's read-only
// single-source compile path (Loader.CompileSource), binding candidate
// automations against the registry the sandbox hands in (an isolated clone)
// without ever touching the filesystem or live engine state.

import (
	"io"
	"log/slog"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

func init() {
	memql.SetSandboxAutomationCompiler(compileAutomationForSandbox)
}

// compileAutomationForSandbox compiles one candidate automation source
// against the supplied (read-only) registry and returns its name. A loader
// is constructed per call with the sandbox's registry so concept-reference
// resolution binds against exactly the constructs in scope (core registry +
// any bundle-defined concepts the sandbox overlaid onto its clone). The
// loader logs nothing of interest here, so its logger is discarded.
func compileAutomationForSandbox(source, origin string, registry memoryNodes.Registry) (string, error) {
	loader := NewLoader(LoaderOptions{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		Registry: registry,
	})
	automation, err := loader.CompileSource(source, origin)
	if err != nil {
		return "", err
	}
	return automation.Name, nil
}
