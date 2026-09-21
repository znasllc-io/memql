package mcp

// source_size.go -- the MCP surface's bounds on DSL source a client sends.
//
// Tokenising costs memory in proportion to the tokens in a source, and the
// work after it grows faster than the source does (the measurement, and the
// two bounds, are in component/language/parser/source_size.go). The two tools
// that take DSL source are bounded in callMCPTool, before their handlers run,
// with the same coded refusal the gRPC stream answers: `define` takes a
// BUNDLE of constructs and gets the larger bound, `run_inline_automation`
// takes one automation's source and gets the file bound.

import "github.com/znasllc-io/memql/component/language/parser"

// dslSourceArg names, per tool, the argument that carries DSL source and the
// bound that argument earns.
type dslSourceArg struct {
	arg   string
	check func(string) error
}

var dslSourceArgs = map[string]dslSourceArg{
	toolDefine:              {arg: "bundle", check: parser.CheckBundleSize},
	toolRunInlineAutomation: {arg: "source", check: parser.CheckSourceSize},
}

// oversizedToolSource refuses a call whose DSL source argument is over its
// bound. Nil for every other tool and argument.
func oversizedToolSource(name string, args map[string]any) error {
	carrier, ok := dslSourceArgs[name]
	if !ok {
		return nil
	}
	source, _ := args[carrier.arg].(string)
	return carrier.check(source)
}
