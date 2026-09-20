package mcp

// source_size.go -- the MCP surface's bound on DSL source a client sends.
//
// Tokenising costs memory in proportion to the tokens in a source, and the
// work after it grows faster than the source does (the measurement is in
// component/language/parser/source_size.go). The two tools that take DSL
// source refuse one over parser.MaxSourceBytes in callMCPTool, before their
// handlers run, with the same coded refusal the gRPC stream answers.

import "github.com/znasllc-io/memql/component/language/parser"

// dslSourceArgs names, per tool, the argument that carries DSL source.
var dslSourceArgs = map[string]string{
	toolDefine:              "bundle",
	toolRunInlineAutomation: "source",
}

// oversizedToolSource refuses a call whose DSL source argument is over
// parser.MaxSourceBytes. Nil for every other tool and argument.
func oversizedToolSource(name string, args map[string]any) error {
	arg, ok := dslSourceArgs[name]
	if !ok {
		return nil
	}
	source, _ := args[arg].(string)
	return parser.CheckSourceSize(source)
}
