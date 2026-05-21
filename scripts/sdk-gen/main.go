// sdk-gen walks the DSL tree and emits typed Go methods on
// `sdk/go/client.QueryClient` for every query / mutation / logic
// declared in `dsl/**/*.memql`.
//
// Output:
//
//	sdk/go/client/generated_queries.go
//	sdk/go/client/generated_mutations.go
//	sdk/go/client/generated_logics.go
//	sdk/go/client/generated_args.go
//
// Re-run via `make sdk-gen`. CI gate via `make sdk-gen-check`
// (regenerates + git diff --exit-code).
//
// Design notes:
//   - The DSL's args block carries the typed argument schema for every
//     construct -- enough to emit a Go struct with named fields and
//     Go-native types. We lean on the existing parser
//     (component/language/parser) to lift the args schema out of each
//     source rather than re-implement a parser here.
//   - Each generated method composes the engine's wire call from its
//     typed args and dispatches through the unexported
//     QueryClient.execute path. Consumers never see DSL strings.
//   - Return type for queries is `Result` (a wrapped *ExecuteResult
//     with helpers for iterating projected rows). The shape-projected
//     row content remains `map[string]any` for now; the per-shape
//     struct emission is a follow-up the design is intentionally
//     leaving room for (separate issue).
//
// Drift gate:
//   - The generator must be deterministic. Field order in the args
//     struct mirrors source order; methods are sorted by name within
//     each kind. Regenerating against the same DSL tree produces the
//     same bytes.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	// Header of each construct: `<kind> [<Concept>] <Name> {`.
	// The optional middle identifier is the signature-bound concept on
	// queries / mutations / shapes (PR #48 / #49). logic + automation
	// never bind a concept in the signature; for those the middle
	// segment is always absent in authored sources.
	//
	// Anchored at column 0 to avoid matching `logic X { ... }` nested
	// inside automation step bodies -- those are call-site references,
	// not top-level declarations.
	constructHeader = regexp.MustCompile(
		`(?m)^(query|mutation|logic)[ \t]+(?:([A-Za-z_][A-Za-z0-9_]*)[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`,
	)
	// Args block inside a construct body: `args { ... }`. Single match
	// per construct (the parser rejects multiple args blocks).
	argsBlockRe = regexp.MustCompile(`(?s)\bargs[ \t]*\{(.*?)\}`)
	// Single args field declaration:
	//   <name>  <type>  [@required] [@enum("a","b")] [@default("...")] [@description("...")]
	// Multiple lines per block.
	argsFieldRe = regexp.MustCompile(
		`(?m)^[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]+([A-Za-z_][A-Za-z0-9_]*|enum\([^)]*\)|\[\][A-Za-z_][A-Za-z0-9_]*)([ \t]+@[^\n]*)?[ \t]*$`,
	)
	// Description annotation above a construct.
	descRe = regexp.MustCompile(`(?m)^@description\("(.*?)"\)`)
	// Shape reference inside a query body: `shape <name>`.
	shapeRefRe = regexp.MustCompile(`(?m)^[ \t]+shape[ \t]+([A-Za-z_][A-Za-z0-9_]*)`)
)

// Construct captures the generator's view of a single DSL declaration.
type Construct struct {
	Kind        string // "query" | "mutation" | "logic"
	Name        string // function name as it appears in DSL + wire
	Concept     string // signature-bound concept (queries / mutations only); empty for logic
	Description string // from @description annotation
	Args        []ArgField
	ShapeName   string // for queries: the shape the result projects through
	Origin      string // file:line for traceability in generated comments
}

// ArgField is one field in the construct's args block.
type ArgField struct {
	Name        string
	Type        string // raw DSL type: "string", "bool", "number", "object", "array", "datetime", "enum(...)"
	Required    bool
	Description string
	Default     string
	Enum        []string
}

func main() {
	dslRoot := flag.String("dsl", "dsl", "root of DSL tree")
	outDir := flag.String("out", "sdk/go/client", "directory for generated Go files")
	tsOutDir := flag.String("ts-out", "sdk/ts/src/client", "directory for generated TypeScript files; empty to skip TS emission")
	check := flag.Bool("check", false, "exit non-zero if regenerated files would differ")
	flag.Parse()

	constructs, err := collectConstructs(*dslRoot)
	if err != nil {
		fail(fmt.Errorf("collect constructs: %w", err))
	}
	if len(constructs) == 0 {
		fail(fmt.Errorf("no constructs found under %q", *dslRoot))
	}

	// Group by kind so each output file holds a single kind. Keeps
	// `git diff` readable when only one kind changes.
	byKind := map[string][]Construct{}
	for _, c := range constructs {
		byKind[c.Kind] = append(byKind[c.Kind], c)
	}
	for k := range byKind {
		sort.Slice(byKind[k], func(i, j int) bool { return byKind[k][i].Name < byKind[k][j].Name })
	}

	type outFile struct {
		dir  string
		name string
		body []byte
	}
	out := []outFile{
		{*outDir, "generated_queries.go", emitMethods(byKind["query"], "Query")},
		{*outDir, "generated_mutations.go", emitMethods(byKind["mutation"], "Mutation")},
		{*outDir, "generated_logics.go", emitMethods(byKind["logic"], "Logic")},
	}
	if strings.TrimSpace(*tsOutDir) != "" {
		out = append(out,
			outFile{*tsOutDir, "generated_queries.ts", emitTSMethods(byKind["query"], "query")},
			outFile{*tsOutDir, "generated_mutations.ts", emitTSMethods(byKind["mutation"], "mutation")},
			outFile{*tsOutDir, "generated_logics.ts", emitTSMethods(byKind["logic"], "logic")},
		)
	}

	if *check {
		drift := false
		for _, f := range out {
			path := filepath.Join(f.dir, f.name)
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, f.body) {
				fmt.Fprintf(os.Stderr, "drift: %s\n", path)
				drift = true
			}
		}
		if drift {
			fmt.Fprintf(os.Stderr, "\nGenerated SDK is out of date. Run `make sdk-gen` and commit the result.\n")
			os.Exit(1)
		}
		fmt.Println("sdk-gen: no drift")
		return
	}

	for _, f := range out {
		if err := os.MkdirAll(f.dir, 0o755); err != nil {
			fail(fmt.Errorf("mkdir %s: %w", f.dir, err))
		}
		path := filepath.Join(f.dir, f.name)
		if err := os.WriteFile(path, f.body, 0o644); err != nil {
			fail(fmt.Errorf("write %s: %w", path, err))
		}
		fmt.Printf("wrote %s (%d bytes)\n", path, len(f.body))
	}
}

// collectConstructs walks the DSL tree and parses each query / mutation
// / logic declaration into a Construct.
func collectConstructs(root string) ([]Construct, error) {
	var out []Construct
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".memql") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		src := string(raw)
		matches := constructHeader.FindAllStringSubmatchIndex(src, -1)
		for _, m := range matches {
			// m: [headStart, headEnd, kindStart, kindEnd, conceptStart, conceptEnd, nameStart, nameEnd]
			kind := src[m[2]:m[3]]
			concept := ""
			if m[4] >= 0 {
				concept = src[m[4]:m[5]]
			}
			name := src[m[6]:m[7]]

			// Find the matching closing brace for this construct so we
			// only inspect its body when extracting args / shape ref.
			body, ok := bodyFor(src, m[1]-1)
			if !ok {
				continue
			}

			c := Construct{
				Kind:        kind,
				Name:        name,
				Concept:     concept,
				Description: descriptionFor(src, m[0]),
				Args:        parseArgsBlock(body),
				ShapeName:   shapeRefFor(body),
				Origin:      fmt.Sprintf("%s", path),
			}
			out = append(out, c)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// bodyFor returns the substring of `src` between the construct's
// opening `{` and matching closing `}`. openIdx is the byte offset of
// the opening brace.
func bodyFor(src string, openIdx int) (string, bool) {
	depth := 0
	for i := openIdx; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[openIdx+1 : i], true
			}
		}
	}
	return "", false
}

// descriptionFor walks BACKWARDS from the construct header looking for
// a `@description("...")` annotation in the immediately-preceding
// attribute block. Returns empty string when no description annotation
// is present.
func descriptionFor(src string, headerStart int) string {
	// Walk back to find the start of the attribute preamble: stop at
	// the first non-attribute, non-blank line.
	prev := strings.LastIndex(src[:headerStart], "\n")
	for prev > 0 {
		lineStart := strings.LastIndex(src[:prev], "\n") + 1
		line := strings.TrimSpace(src[lineStart:prev])
		if line == "" {
			prev = lineStart - 1
			continue
		}
		if !strings.HasPrefix(line, "@") {
			break
		}
		prev = lineStart - 1
	}
	preamble := src[prev:headerStart]
	if m := descRe.FindStringSubmatch(preamble); len(m) > 1 {
		return m[1]
	}
	return ""
}

// parseArgsBlock extracts the args { ... } block from a construct body
// and returns its fields in source order. Braces are balance-matched
// so a `{N,M}` quantifier inside a `@pattern("...")` regex string
// doesn't terminate the block early (the regex-based extractor
// otherwise grabs the `}` from inside the regex and truncates the
// captured args). See memql#147 for the @pattern annotation surface.
func parseArgsBlock(body string) []ArgField {
	inner, ok := extractArgsBlockBody(body)
	if !ok {
		return nil
	}
	var out []ArgField
	for _, fm := range argsFieldRe.FindAllStringSubmatch(inner, -1) {
		name := fm[1]
		typ := fm[2]
		annotations := ""
		if len(fm) > 3 {
			annotations = fm[3]
		}
		af := ArgField{Name: name, Type: typ}
		if strings.Contains(annotations, "@required") {
			af.Required = true
		}
		if dm := regexp.MustCompile(`@description\("([^"]*)"\)`).FindStringSubmatch(annotations); len(dm) > 1 {
			af.Description = dm[1]
		}
		if dm := regexp.MustCompile(`@default\("([^"]*)"\)`).FindStringSubmatch(annotations); len(dm) > 1 {
			af.Default = dm[1]
		}
		if em := regexp.MustCompile(`@enum\(([^)]*)\)`).FindStringSubmatch(annotations); len(em) > 1 {
			for _, raw := range strings.Split(em[1], ",") {
				af.Enum = append(af.Enum, strings.Trim(strings.TrimSpace(raw), `"`))
			}
		}
		out = append(out, af)
	}
	return out
}

// extractArgsBlockBody finds the `args { ... }` opening brace inside
// the construct body and returns the inner text (between braces),
// using a string-aware brace counter so `}` characters inside
// double-quoted string literals -- including those introduced by
// @pattern("...{N,M}...") regex annotations -- don't terminate the
// block.
func extractArgsBlockBody(body string) (string, bool) {
	// Find the `args {` opening via the existing regex; depth-count
	// from there to find the matching close.
	m := argsBlockRe.FindStringSubmatchIndex(body)
	if len(m) == 0 {
		return "", false
	}
	// Group 1 starts at the byte after `{`.
	start := m[2]
	depth := 1
	inStr := false
	escape := false
	for i := start; i < len(body); i++ {
		c := body[i]
		switch {
		case escape:
			escape = false
		case inStr && c == '\\':
			escape = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// any other char inside a string literal -- skip
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return body[start:i], true
			}
		}
	}
	return "", false
}

func shapeRefFor(body string) string {
	if m := shapeRefRe.FindStringSubmatch(body); len(m) > 1 {
		return m[1]
	}
	return ""
}

// emitMethods produces the generated Go source for one construct kind
// (Query / Mutation / Logic). Returns gofmt-formatted bytes.
func emitMethods(constructs []Construct, kindLabel string) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "// Code generated by scripts/sdk-gen. DO NOT EDIT.\n")
	fmt.Fprintf(&buf, "// Source of truth: dsl/**/*.memql.\n\n")
	fmt.Fprintf(&buf, "package client\n\n")
	fmt.Fprintf(&buf, "import (\n\t\"context\"\n\t\"fmt\"\n\t\"strings\"\n)\n\n")

	// Silence unused-import warnings when a generated file has no
	// constructs that exercise fmt / strings.
	fmt.Fprintf(&buf, "var (\n\t_ = fmt.Sprintf\n\t_ = strings.Builder{}\n)\n\n")

	for _, c := range constructs {
		emitConstruct(&buf, c, kindLabel)
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		// Emit unformatted source so the developer can see the error
		// in context. The CI drift check will catch the mismatch.
		fmt.Fprintf(os.Stderr, "WARN: gofmt failed for %s methods: %v\n", kindLabel, err)
		return buf.Bytes()
	}
	return formatted
}

func emitConstruct(buf *bytes.Buffer, c Construct, kindLabel string) {
	exportedName := pascalCase(c.Name)
	argsTypeName := exportedName + "Args"

	// Doc comment lifted from @description.
	if c.Description != "" {
		writeDoc(buf, exportedName+" -- "+c.Description)
	} else {
		fmt.Fprintf(buf, "// %s wraps the %s named %q.\n", exportedName, strings.ToLower(kindLabel), c.Name)
	}
	if c.Concept != "" {
		fmt.Fprintf(buf, "//\n// Bound concept: %s.\n", c.Concept)
	}

	// Args struct.
	fmt.Fprintf(buf, "type %s struct {\n", argsTypeName)
	for _, a := range c.Args {
		writeArgField(buf, a)
	}
	fmt.Fprintf(buf, "}\n\n")

	// Method on QueryClient.
	writeDoc(buf, exportedName+" calls the engine "+strings.ToLower(kindLabel)+" "+c.Name+".")
	fmt.Fprintf(buf, "func (qc *QueryClient) %s(ctx context.Context, args %s) (*Result, error) {\n", exportedName, argsTypeName)
	fmt.Fprintf(buf, "\tcall := %sBuild(args)\n", exportedName)
	fmt.Fprintf(buf, "\treturn qc.executeNamed(ctx, %q, call)\n", c.Name)
	fmt.Fprintf(buf, "}\n\n")

	// Builder function -- composes the DSL call string from typed args.
	// Lives next to the method for traceability + testability.
	fmt.Fprintf(buf, "func %sBuild(args %s) string {\n", exportedName, argsTypeName)
	if len(c.Args) == 0 {
		fmt.Fprintf(buf, "\t_ = args\n\treturn %q\n", c.Name+"({})")
	} else {
		fmt.Fprintf(buf, "\tvar b strings.Builder\n")
		fmt.Fprintf(buf, "\tb.WriteString(%q)\n", c.Name+"({")
		first := true
		for _, a := range c.Args {
			emitArgComposer(buf, a, first)
			if first {
				first = false
			}
		}
		fmt.Fprintf(buf, "\tb.WriteString(\"})\")\n")
		fmt.Fprintf(buf, "\treturn b.String()\n")
	}
	fmt.Fprintf(buf, "}\n\n")
}

func writeArgField(buf *bytes.Buffer, a ArgField) {
	if a.Description != "" {
		fmt.Fprintf(buf, "\t// %s\n", a.Description)
	}
	if len(a.Enum) > 0 {
		fmt.Fprintf(buf, "\t// Enum: %s\n", strings.Join(a.Enum, " | "))
	}
	if a.Default != "" {
		fmt.Fprintf(buf, "\t// Default: %q\n", a.Default)
	}
	fmt.Fprintf(buf, "\t%s %s\n", pascalCase(a.Name), goType(a.Type))
	if !a.Required {
		// Track which fields are optional so the call composer can
		// emit them only when set. Encoded as a parallel `<Name>Set
		// bool` field per zero-value-ambiguous type. For string we
		// rely on "empty means unset"; for bool we can't, so we add
		// the explicit Set bool. For numeric the same applies.
		switch a.Type {
		case "bool", "boolean":
			fmt.Fprintf(buf, "\t%sSet bool // set true to send %s; required because zero-value bool is ambiguous\n", pascalCase(a.Name), a.Name)
		}
	}
}

func emitArgComposer(buf *bytes.Buffer, a ArgField, first bool) {
	field := pascalCase(a.Name)
	// Optional fields skip emission when at the Go zero value
	// (string=="" / bool zero only when !FieldSet). Required fields
	// always emit.
	emitGuard := func(action func()) {
		if a.Required {
			action()
			return
		}
		switch a.Type {
		case "bool", "boolean":
			fmt.Fprintf(buf, "\tif args.%sSet {\n", field)
			action()
			fmt.Fprintf(buf, "\t}\n")
		case "string":
			fmt.Fprintf(buf, "\tif args.%s != \"\" {\n", field)
			action()
			fmt.Fprintf(buf, "\t}\n")
		case "number", "int", "integer":
			fmt.Fprintf(buf, "\tif args.%s != 0 {\n", field)
			action()
			fmt.Fprintf(buf, "\t}\n")
		default:
			// Object / array / unknown: emit unconditionally; the
			// DSL parser tolerates nil values.
			action()
		}
	}

	emitGuard(func() {
		if !first {
			fmt.Fprintf(buf, "\tif b.Len() > %d { b.WriteString(\", \") }\n", len("FNAME({")+10)
		}
		fmt.Fprintf(buf, "\tb.WriteString(%q)\n", a.Name+": ")
		fmt.Fprintf(buf, "\tb.WriteString(%s)\n", renderArg(field, a.Type))
	})
}

// renderArg returns a Go expression that produces the MemQL literal
// form for an arg field of the given DSL type.
func renderArg(field, typ string) string {
	switch typ {
	case "string", "datetime":
		return fmt.Sprintf("fmt.Sprintf(\"%%q\", args.%s)", field)
	case "bool", "boolean":
		return fmt.Sprintf("fmt.Sprintf(\"%%v\", args.%s)", field)
	case "number", "int", "integer":
		return fmt.Sprintf("fmt.Sprintf(\"%%v\", args.%s)", field)
	case "object", "array":
		// Object / array values arrive as map[string]any / []any;
		// hand off to renderMemQLValue (defined in support.go).
		return fmt.Sprintf("renderMemQLValue(args.%s)", field)
	default:
		if strings.HasPrefix(typ, "enum(") {
			return fmt.Sprintf("fmt.Sprintf(\"%%q\", args.%s)", field)
		}
		if strings.HasPrefix(typ, "[]") {
			return fmt.Sprintf("renderMemQLValue(args.%s)", field)
		}
		return fmt.Sprintf("fmt.Sprintf(\"%%q\", args.%s)", field)
	}
}

// goType maps a DSL type token to its Go-side counterpart for the
// generated args struct.
func goType(typ string) string {
	switch typ {
	case "string", "datetime":
		return "string"
	case "bool", "boolean":
		return "bool"
	case "number":
		return "float64"
	case "int", "integer":
		return "int"
	case "object":
		return "map[string]any"
	case "array":
		return "[]any"
	}
	if strings.HasPrefix(typ, "enum(") {
		return "string"
	}
	if strings.HasPrefix(typ, "[]") {
		// Typed slice: `[]string`, `[]object`, ... -- map element
		// type recursively.
		return "[]" + goType(strings.TrimPrefix(typ, "[]"))
	}
	return "any"
}

func writeDoc(buf *bytes.Buffer, text string) {
	for _, line := range strings.Split(text, "\n") {
		fmt.Fprintf(buf, "// %s\n", line)
	}
}

// pascalCase converts a camelCase or snake_case identifier to
// PascalCase (the Go-exported form). Best-effort, doesn't handle every
// edge case but covers every name produced by the DSL.
func pascalCase(s string) string {
	if s == "" {
		return ""
	}
	parts := []rune{}
	upperNext := true
	for _, r := range s {
		if r == '_' || r == '-' {
			upperNext = true
			continue
		}
		if upperNext {
			if r >= 'a' && r <= 'z' {
				r = r - 'a' + 'A'
			}
			upperNext = false
		}
		parts = append(parts, r)
	}
	return string(parts)
}

// =============================================================================
// TypeScript emission
//
// Mirrors emitMethods + emitConstruct for the sdk/ts surface. Uses
// TypeScript declaration merging so the generated method signature
// shows up on QueryClient (matching the Go SDK's `qc.QueryActiveSpaces`
// receiver-method shape).
// =============================================================================

func emitTSMethods(constructs []Construct, kindLabel string) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "// Code generated by scripts/sdk-gen. DO NOT EDIT.\n")
	fmt.Fprintf(&buf, "// Source of truth: dsl/**/*.memql.\n\n")
	fmt.Fprintf(&buf, "import { QueryClient, type QueryCallOptions } from \"./query.js\";\n")
	fmt.Fprintf(&buf, "import type { Result } from \"./types.js\";\n")
	fmt.Fprintf(&buf, "import { renderMemQLValue } from \"./memqlValue.js\";\n\n")

	for _, c := range constructs {
		emitTSConstruct(&buf, c, kindLabel)
	}

	if len(constructs) == 0 {
		// Avoid an empty file -- emit a placeholder export so the
		// drift gate stays stable and the file still type-checks.
		fmt.Fprintf(&buf, "export {};\n")
	}

	return buf.Bytes()
}

func emitTSConstruct(buf *bytes.Buffer, c Construct, kindLabel string) {
	camelName := lowerFirst(pascalCase(c.Name))
	argsTypeName := pascalCase(c.Name) + "Args"
	builderName := "build" + pascalCase(c.Name)

	// JSDoc comment lifted from @description.
	if c.Description != "" {
		fmt.Fprintf(buf, "/** %s */\n", escapeJSDoc(c.Description))
	} else {
		fmt.Fprintf(buf, "/** %s wraps the %s named %q. */\n", camelName, kindLabel, c.Name)
	}
	if c.Concept != "" {
		fmt.Fprintf(buf, "// Bound concept: %s.\n", c.Concept)
	}

	// Args interface.
	fmt.Fprintf(buf, "export interface %s {\n", argsTypeName)
	for _, a := range c.Args {
		emitTSArgField(buf, a)
	}
	fmt.Fprintf(buf, "}\n\n")

	// Builder function -- composes the DSL call string from typed args.
	fmt.Fprintf(buf, "export function %s(args: %s): string {\n", builderName, argsTypeName)
	if len(c.Args) == 0 {
		fmt.Fprintf(buf, "  void args;\n")
		fmt.Fprintf(buf, "  return %q;\n", c.Name+"({})")
	} else {
		fmt.Fprintf(buf, "  const parts: string[] = [];\n")
		for _, a := range c.Args {
			emitTSArgComposer(buf, a)
		}
		fmt.Fprintf(buf, "  return %q + parts.join(\", \") + \"})\";\n", c.Name+"({")
	}
	fmt.Fprintf(buf, "}\n\n")

	// Method augmentation on QueryClient.
	fmt.Fprintf(buf, "declare module \"./query.js\" {\n")
	fmt.Fprintf(buf, "  interface QueryClient {\n")
	if len(c.Args) == 0 {
		fmt.Fprintf(buf, "    %s(args?: %s, opts?: QueryCallOptions): Promise<Result>;\n", camelName, argsTypeName)
	} else {
		fmt.Fprintf(buf, "    %s(args: %s, opts?: QueryCallOptions): Promise<Result>;\n", camelName, argsTypeName)
	}
	fmt.Fprintf(buf, "  }\n")
	fmt.Fprintf(buf, "}\n\n")

	// Runtime method assignment.
	fmt.Fprintf(buf, "QueryClient.prototype.%s = function (this: QueryClient, args: %s = {} as %s, opts?: QueryCallOptions): Promise<Result> {\n", camelName, argsTypeName, argsTypeName)
	fmt.Fprintf(buf, "  return this.executeNamed(%q, %s(args), opts);\n", c.Name, builderName)
	fmt.Fprintf(buf, "};\n\n")
}

func emitTSArgField(buf *bytes.Buffer, a ArgField) {
	if a.Description != "" {
		fmt.Fprintf(buf, "  /** %s */\n", escapeJSDoc(a.Description))
	}
	if len(a.Enum) > 0 {
		fmt.Fprintf(buf, "  // Enum: %s\n", strings.Join(a.Enum, " | "))
	}
	if a.Default != "" {
		fmt.Fprintf(buf, "  // Default: %q\n", a.Default)
	}
	optional := ""
	if !a.Required {
		optional = "?"
	}
	fmt.Fprintf(buf, "  %s%s: %s;\n", a.Name, optional, tsType(a))
}

func emitTSArgComposer(buf *bytes.Buffer, a ArgField) {
	if a.Required {
		fmt.Fprintf(buf, "  parts.push(\"%s: \" + renderMemQLValue(args.%s));\n", a.Name, a.Name)
		return
	}
	// Optional fields skip emission when undefined. Other zero
	// values (empty string, false, 0) DO emit because the engine's
	// parseValue accepts them.
	fmt.Fprintf(buf, "  if (args.%s !== undefined) parts.push(\"%s: \" + renderMemQLValue(args.%s));\n", a.Name, a.Name, a.Name)
}

func tsType(a ArgField) string {
	switch a.Type {
	case "string", "datetime":
		return "string"
	case "bool", "boolean":
		return "boolean"
	case "number", "int", "integer":
		return "number"
	case "object":
		return "Record<string, unknown>"
	case "array":
		return "unknown[]"
	}
	if strings.HasPrefix(a.Type, "enum(") {
		return "string"
	}
	if strings.HasPrefix(a.Type, "[]") {
		elem := tsType(ArgField{Type: strings.TrimPrefix(a.Type, "[]")})
		return elem + "[]"
	}
	return "unknown"
}

func lowerFirst(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	if r[0] >= 'A' && r[0] <= 'Z' {
		r[0] = r[0] - 'A' + 'a'
	}
	return string(r)
}

func escapeJSDoc(s string) string {
	// Replace */ with *\/ to avoid closing the JSDoc block prematurely.
	return strings.ReplaceAll(s, "*/", "*\\/")
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "sdk-gen: %v\n", err)
	os.Exit(1)
}
