package extract

import (
	"go/ast"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/znasllc-io/memql/component/architecture/model"
)

// observeMarkerPrefix is the comment marker that opts a function or
// method into a specific observability level. Authored as a leading
// doc comment immediately above the decl:
//
//	//memql:observe verbose
//	//memql:observe verbose redact=password,token
//	//memql:observe off
//	func (h *Handler) Login(user, password string) error { ... }
//
// The level is one of off / count / meta / verbose. Optional
// redact=NAMES restricts which named arguments may be captured at
// the verbose level; everything else is auto-redacted.
const observeMarkerPrefix = "//memql:observe"

// ObserveSpec is the parsed form of one //memql:observe directive.
type ObserveSpec struct {
	Level      string
	RedactArgs []string
}

// stampObserveMarkers walks every analyzed package's AST and copies
// any //memql:observe directives onto the matching Method / Func
// nodes in the model. It also stamps the default `observable`
// attribute on every Method, Func, and Interface node (true) and
// every Field node (false) so the cockpit renderer knows where it
// makes sense to query for observability data.
//
// Called from ExtractTypes after the structural pass; no-op when
// the model has no Method/Func/Interface/Field nodes (e.g. type
// pass was disabled).
func stampObserveMarkers(m *model.Model, plans []ServicePlan, loaded []loadedTypePass) {
	// Build the FQN -> ObserveSpec map by walking each package's
	// AST. We use the ast.Object resolver to map FuncDecl identifiers
	// back to their declared identity, then translate to a model ID.
	specs := make(map[model.ID]ObserveSpec)
	for _, l := range loaded {
		for _, p := range l.pkgs {
			for _, f := range p.Syntax {
				for _, decl := range f.Decls {
					fn, ok := decl.(*ast.FuncDecl)
					if !ok || fn.Doc == nil {
						continue
					}
					spec, ok := parseObserveDoc(fn.Doc)
					if !ok {
						continue
					}
					id, ok := funcDeclID(p.PkgPath, fn)
					if !ok {
						continue
					}
					specs[id] = spec
				}
			}
		}
	}

	// Patch onto the existing nodes.
	for i := range m.Nodes {
		n := &m.Nodes[i]
		switch n.Kind {
		case model.KindMethod, model.KindFunc:
			if n.Attrs == nil {
				n.Attrs = map[string]string{}
			}
			n.Attrs["observable"] = "true"
			if s, ok := specs[n.ID]; ok {
				if s.Level != "" {
					n.Attrs["observe_level"] = s.Level
				}
				if len(s.RedactArgs) > 0 {
					n.Attrs["redact_args"] = strings.Join(s.RedactArgs, ",")
				}
			}
		case model.KindInterface:
			if n.Attrs == nil {
				n.Attrs = map[string]string{}
			}
			n.Attrs["observable"] = "true"
		case model.KindField:
			if n.Attrs == nil {
				n.Attrs = map[string]string{}
			}
			n.Attrs["observable"] = "false"
		}
	}
}

// parseObserveDoc looks for //memql:observe in the doc-comment block
// and parses the directive on that line. Returns ok=false when no
// marker is present. Multiple markers on the same decl take the
// last-wins (consistent with how vet directives stack).
func parseObserveDoc(doc *ast.CommentGroup) (spec ObserveSpec, ok bool) {
	for _, c := range doc.List {
		// c.Text includes the leading //; trim and check.
		line := strings.TrimSpace(c.Text)
		if !strings.HasPrefix(line, observeMarkerPrefix) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, observeMarkerPrefix))
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			// Bare //memql:observe means "use the package default" --
			// represented by empty Level and consumed by the runtime.
			ok = true
			continue
		}
		spec.Level = fields[0]
		for _, extra := range fields[1:] {
			if strings.HasPrefix(extra, "redact=") {
				names := strings.TrimPrefix(extra, "redact=")
				for _, n := range strings.Split(names, ",") {
					n = strings.TrimSpace(n)
					if n != "" {
						spec.RedactArgs = append(spec.RedactArgs, n)
					}
				}
			}
		}
		ok = true
	}
	return spec, ok
}

// funcDeclID converts an *ast.FuncDecl into the matching model.ID.
// For methods, the receiver type is unwrapped through one level of
// pointer indirection so MethodID lines up with the structural pass.
func funcDeclID(pkgPath string, fn *ast.FuncDecl) (model.ID, bool) {
	if fn.Name == nil {
		return "", false
	}
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return model.FuncID(pkgPath, fn.Name.Name), true
	}
	recvType, ok := receiverTypeName(fn.Recv.List[0].Type)
	if !ok {
		return "", false
	}
	return model.MethodID(pkgPath, recvType, fn.Name.Name), true
}

// receiverTypeName unwraps the receiver expression (T or *T or T[X]
// or *T[X]) and returns the bare receiver type name.
func receiverTypeName(expr ast.Expr) (string, bool) {
	for {
		switch t := expr.(type) {
		case *ast.StarExpr:
			expr = t.X
		case *ast.IndexExpr: // generic receiver: T[X]
			expr = t.X
		case *ast.IndexListExpr: // generic receiver: T[X, Y]
			expr = t.X
		case *ast.Ident:
			return t.Name, true
		default:
			return "", false
		}
	}
}

// loadedTypePass is the structure ExtractTypes builds internally to
// pair a plan with its loaded packages; declared here so the marker
// stamper can be invoked over the same set without re-loading.
type loadedTypePass struct {
	plan ServicePlan
	pkgs []*packages.Package
}
