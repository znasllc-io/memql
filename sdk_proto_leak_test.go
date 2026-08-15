package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// protoSeamAllowlist is the closed set of exported SDK symbols permitted to
// name a `memqlv1.*` wire type, each paired with why it is the TRANSPORT rather
// than a typed surface.
//
// sdk/go/CLAUDE.md §2 states the rule as "Raw memqlv1.* protobuf types do not
// appear in the public surface. Internal SDK code does the proto<->SDK
// translation in one place." This map IS that one place, named. Every entry is
// a stream carrier whose entire job is to move wire messages: a Dispatcher that
// did not accept a MemqlClientMessage would have nothing to multiplex, and a
// worker Connection that did not return a WorkerServerMessage would not be a
// pass-through. They are the floor the typed packages (authoring, sense,
// dslspec, pack, voice) are built ON, and those must stay clean.
//
// Adding a row here is the moment to ask whether the symbol is really
// transport. If it belongs to a typed surface, the fix is a wrapper type in the
// SDK -- which is what memql#3874 did for authoring.PromoteOption rather than
// adding it to this list.
var protoSeamAllowlist = map[string]string{
	"client.NewDispatcher":             "takes the raw MemqlService_StreamClient it multiplexes; the constructor is where the wire enters the SDK",
	"client.Dispatcher.Send":           "sends one wire message on the stream -- the message IS the argument",
	"client.Dispatcher.SendAndWait":    "sends a wire message and returns the correlated wire reply; the typed packages adapt it",
	"client.Dispatcher.Events":         "the shared channel of uncorrelated server messages, consumed and adapted by callers",
	"client.Dispatcher.RegisterStream": "session-keyed channel of server messages for the streaming protocols",
	"worker.Connection.Stream":         "returns the underlying WorkerService bidi stream the caller drives",
	"worker.Connection.Send":           "documented pass-through to Stream().Send",
	"worker.Connection.Recv":           "documented pass-through to Stream().Recv",
}

// TestSDKPublicSurfaceHasNoProtoLeak walks every non-test .go file under
// sdk/go/ and fails when an EXPORTED declaration names a type from the
// generated proto package (memql#3874).
//
// # Why the exported surface specifically
//
// The defect this catches is not that the SDK uses proto types -- it must, it
// speaks a gRPC protocol. It is that a consumer can REACH one. An exported
// symbol whose type names a wire message is an escape hatch past the boundary
// the SDK exists to hold: whoever wants to set a field the SDK has not wrapped
// yet will find it, and that is precisely the moment the boundary is
// load-bearing. authoring.PromoteOption was that shape -- nobody needed to
// construct one, and anybody could.
//
// # What counts as exported
//
// Exported funcs, exported types (including the field types of exported struct
// fields and the methods of exported interfaces), exported vars/consts, and
// methods on exported receivers. An unexported helper doing the translation is
// the CORRECT pattern (protoDiagnostics, protoConceptDiffs) and is not flagged.
func TestSDKPublicSurfaceHasNoProtoLeak(t *testing.T) {
	const sdkRoot = "sdk/go"

	type finding struct {
		key  string
		pos  string
		what string
	}
	var findings []finding
	filesScanned := 0
	filesWithProtoImport := 0
	pkgs := map[string]bool{}

	fset := token.NewFileSet()
	err := filepath.Walk(sdkRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		filesScanned++
		pkgDir := filepath.Base(filepath.Dir(path))
		pkgs[pkgDir] = true

		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", path, parseErr)
		}

		// Resolve the local name the generated proto package is bound to in
		// this file. Keyed off the import path, not a hardcoded "memqlv1", so
		// renaming the alias cannot silently disarm the check.
		alias := ""
		for _, imp := range f.Imports {
			if strings.Contains(imp.Path.Value, "component/grpc/gen") {
				alias = "memqlv1"
				if imp.Name != nil {
					alias = imp.Name.Name
				}
			}
		}
		if alias == "" {
			return nil
		}
		filesWithProtoImport++

		mentionsProto := func(n ast.Node) bool {
			found := false
			ast.Inspect(n, func(x ast.Node) bool {
				if sel, ok := x.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == alias {
						found = true
					}
				}
				return true
			})
			return found
		}
		record := func(pos token.Pos, key, what string) {
			findings = append(findings, finding{
				key:  key,
				pos:  fset.Position(pos).String(),
				what: what,
			})
		}

		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				recvType := ""
				if d.Recv != nil {
					ast.Inspect(d.Recv, func(x ast.Node) bool {
						if id, ok := x.(*ast.Ident); ok && ast.IsExported(id.Name) && recvType == "" {
							recvType = id.Name
						}
						return true
					})
					// A method on an unexported receiver is unreachable from
					// outside the package, so it is not part of the surface.
					if recvType == "" {
						continue
					}
				}
				if !mentionsProto(d.Type) {
					continue
				}
				if recvType == "" {
					record(d.Pos(), pkgDir+"."+d.Name.Name, "func "+d.Name.Name)
				} else {
					record(d.Pos(), pkgDir+"."+recvType+"."+d.Name.Name,
						fmt.Sprintf("method (%s).%s", recvType, d.Name.Name))
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if !s.Name.IsExported() {
							continue
						}
						switch typ := s.Type.(type) {
						case *ast.StructType:
							for _, fld := range typ.Fields.List {
								exported := len(fld.Names) == 0 // embedded
								for _, n := range fld.Names {
									if n.IsExported() {
										exported = true
									}
								}
								if exported && mentionsProto(fld.Type) {
									name := "<embedded>"
									if len(fld.Names) > 0 {
										name = fld.Names[0].Name
									}
									record(fld.Pos(), pkgDir+"."+s.Name.Name+"."+name,
										fmt.Sprintf("field %s.%s", s.Name.Name, name))
								}
							}
						case *ast.InterfaceType:
							for _, m := range typ.Methods.List {
								if mentionsProto(m.Type) {
									name := "<embedded>"
									if len(m.Names) > 0 {
										name = m.Names[0].Name
									}
									record(m.Pos(), pkgDir+"."+s.Name.Name+"."+name,
										fmt.Sprintf("interface method %s.%s", s.Name.Name, name))
								}
							}
						default:
							if mentionsProto(s.Type) {
								record(s.Pos(), pkgDir+"."+s.Name.Name, "type "+s.Name.Name)
							}
						}
					case *ast.ValueSpec:
						if s.Type == nil || !mentionsProto(s.Type) {
							continue
						}
						for _, n := range s.Names {
							if n.IsExported() {
								record(n.Pos(), pkgDir+"."+n.Name, "var/const "+n.Name)
							}
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", sdkRoot, err)
	}

	// Coverage. A path typo or a walk that silently matched nothing would make
	// every assertion below vacuously true, so the instrument states what it
	// examined and refuses to pass on an empty sweep.
	t.Logf("scanned %d non-test .go files across %d packages under %s; %d import the generated proto package",
		filesScanned, len(pkgs), sdkRoot, filesWithProtoImport)
	if filesScanned == 0 {
		t.Fatalf("scanned 0 files under %s -- the check examined nothing, so its pass means nothing", sdkRoot)
	}
	if filesWithProtoImport == 0 {
		t.Fatalf("no file under %s imports component/grpc/gen -- either the SDK stopped speaking the wire protocol (unlikely) or the import detection broke", sdkRoot)
	}

	var leaks []string
	for _, f := range findings {
		if _, allowed := protoSeamAllowlist[f.key]; allowed {
			continue
		}
		leaks = append(leaks, fmt.Sprintf("  %s: %s (key %q)", f.pos, f.what, f.key))
	}
	if len(leaks) > 0 {
		sort.Strings(leaks)
		t.Errorf("%d exported SDK symbol(s) name a memqlv1 wire type, which puts the wire in the public surface (sdk/go/CLAUDE.md §2):\n%s\n\n"+
			"Fix by wrapping the value in an SDK-owned type -- see authoring.PromoteOption (memql#3874) for the opaque-struct shape.\n"+
			"If the symbol really IS transport, add it to protoSeamAllowlist with the reason.",
			len(leaks), strings.Join(leaks, "\n"))
	}

	// The allowlist is falsifiable in the other direction too: an entry that no
	// longer matches anything is a stale exemption, and a stale exemption is a
	// hole nobody is looking at.
	seen := map[string]bool{}
	for _, f := range findings {
		seen[f.key] = true
	}
	var stale []string
	for key := range protoSeamAllowlist {
		if !seen[key] {
			stale = append(stale, key)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("protoSeamAllowlist has %d entr(y/ies) matching no exported symbol -- remove them, a stale exemption is an unwatched hole:\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}
}
