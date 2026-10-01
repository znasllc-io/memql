package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/znasllc-io/memql/component/architecture/model"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// protoFiles are the engine's proto descriptors: every gRPC service the engine
// defines is in one of them.
func protoFiles() []protoreflect.FileDescriptor {
	return []protoreflect.FileDescriptor{
		memqlv1.File_memql_proto,
		memqlv1.File_worker_proto,
		memqlv1.File_deploy_control_proto,
		nodev1.File_node_proto,
	}
}

// registerRe is the generated registration call, `Register<Service>Server(`.
var registerRe = regexp.MustCompile(`^Register([A-Za-z0-9_]+)Server$`)

// registrant is where a service's server gets registered: the function (or
// method of Recv) in Pkg whose body calls Register<Service>Server, and the
// package-level functions that construct what it belongs to.
type registrant struct {
	Service      string // short name, MemqlService
	Pkg          string
	Recv, Func   string
	Constructors []string
}

// servicesPass records every gRPC service and its RPCs, then a serves edge
// from each role to each service whose server that role's build CONSTRUCTS.
//
// WHY CONSTRUCTS, AND NOT "LINKS". Every role links every registering package
// -- component/worker and component/deploycontrol are in all seven binaries'
// dependency sets -- so "the binary contains the code" would say every role
// serves every service, which is false: only the agent registers
// WorkerService, only identity DeployControlService. What differs between
// roles is which files their build TAG compiles, and the construction of a
// server is a call in one of those files: worker.NewService in an
// agent-tagged app file, deploycontrol.NewService in an identity-tagged one,
// memqlgrpc.NewServer in an untagged one (every role), NewNodeServer in the
// untagged node bootstraps (every role -- identity and edge fall through to
// the bff bootstrap, which is why their Deployments expose a node port).
//
// So: find each Register<S>Server call, take the function it sits in, and its
// receiver type's constructors (the package-level functions returning that
// type; the function itself when it has no receiver). A role serves S when a
// file its build compiles calls one of those constructors. Syntax only, per
// file, resolved through that file's imports -- no type checking, so it costs
// a parse of each file once.
func servicesPass(b *builder, builds []roleBuild) error {
	known := map[string]string{} // short name -> full name
	for _, fd := range protoFiles() {
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			s := svcs.Get(i)
			full := string(s.FullName())
			known[string(s.Name())] = full
			b.node(model.GRPCServiceID(full), model.PlatformService, string(s.Name()), map[string]string{
				"proto": filepath.Base(fd.Path()),
			})
			methods := s.Methods()
			for j := 0; j < methods.Len(); j++ {
				m := methods.Get(j)
				b.node(model.RPCID(full, string(m.Name())), model.PlatformRPC, string(m.Name()), map[string]string{
					"input":           string(m.Input().FullName()),
					"output":          string(m.Output().FullName()),
					"clientStreaming": strconv.FormatBool(m.IsStreamingClient()),
					"serverStreaming": strconv.FormatBool(m.IsStreamingServer()),
				})
				b.edge(model.GRPCServiceID(full), model.RPCID(full, string(m.Name())), model.PlatformContains, nil)
			}
		}
	}

	files := newFileSet()
	var all []listedPackage
	seenPkg := map[string]bool{}
	for _, rb := range builds {
		for _, p := range rb.pkgs {
			key := p.ImportPath + "\x00" + strings.Join(p.GoFiles, ",")
			if !seenPkg[key] {
				seenPkg[key] = true
				all = append(all, p)
			}
		}
	}
	regs, err := findRegistrants(files, all, known)
	if err != nil {
		return err
	}
	for short := range known {
		found := false
		for _, r := range regs {
			found = found || r.Service == short
		}
		if !found {
			return fmt.Errorf("no package any role links calls Register%sServer, so nothing can say which role serves %s", short, known[short])
		}
	}

	for _, rb := range builds {
		served := map[string]string{} // short -> via
		for _, p := range rb.pkgs {
			for _, name := range p.GoFiles {
				f, imports, err := files.parse(filepath.Join(p.Dir, name))
				if err != nil {
					return err
				}
				for _, r := range regs {
					if _, done := served[r.Service]; done {
						continue
					}
					if via, ok := callsConstructor(f, imports, p.ImportPath, r); ok {
						served[r.Service] = via
					}
				}
			}
		}
		shorts := make([]string, 0, len(served))
		for s := range served {
			shorts = append(shorts, s)
		}
		sort.Strings(shorts)
		for _, s := range shorts {
			b.edge(model.ServiceID(string(rb.role.Type)), model.GRPCServiceID(known[s]), model.PlatformServes, map[string]string{"via": served[s]})
		}
	}
	return nil
}

// fileSet parses each file once, across every role that compiles it.
type fileSet struct {
	fset   *token.FileSet
	parsed map[string]*ast.File
}

func newFileSet() *fileSet {
	return &fileSet{fset: token.NewFileSet(), parsed: map[string]*ast.File{}}
}

// parse returns the file and its imports, name -> path. A file's import name
// is its explicit alias or, absent one, the last path element -- which is the
// package name for every repository package this pass resolves constructors
// in (a mismatch only loses an edge, which the pass's own checks then report).
func (s *fileSet) parse(path string) (*ast.File, map[string]string, error) {
	f, ok := s.parsed[path]
	if !ok {
		var err error
		f, err = parser.ParseFile(s.fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
		}
		s.parsed[path] = f
	}
	imports := map[string]string{}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		imports[name] = p
	}
	return f, imports, nil
}

// findRegistrants scans every package for Register<S>Server calls.
func findRegistrants(files *fileSet, pkgs []listedPackage, known map[string]string) ([]registrant, error) {
	var regs []registrant
	seen := map[string]bool{}
	for _, p := range pkgs {
		var decls []*ast.FuncDecl
		for _, name := range p.GoFiles {
			f, _, err := files.parse(filepath.Join(p.Dir, name))
			if err != nil {
				return nil, err
			}
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok {
					decls = append(decls, fd)
				}
			}
		}
		for _, fd := range decls {
			if fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				m := registerRe.FindStringSubmatch(sel.Sel.Name)
				if m == nil {
					return true
				}
				if _, ok := known[m[1]]; !ok {
					return true
				}
				r := registrant{Service: m[1], Pkg: p.ImportPath, Recv: recvName(fd), Func: fd.Name.Name}
				key := r.Service + "\x00" + r.Pkg + "\x00" + r.Recv + "\x00" + r.Func
				if seen[key] {
					return true
				}
				seen[key] = true
				if r.Recv == "" {
					r.Constructors = []string{r.Func}
				} else {
					for _, c := range decls {
						if c.Recv == nil && returns(c, r.Recv) {
							r.Constructors = append(r.Constructors, c.Name.Name)
						}
					}
					sort.Strings(r.Constructors)
				}
				regs = append(regs, r)
				return true
			})
		}
	}
	sort.Slice(regs, func(i, j int) bool {
		a, b := regs[i], regs[j]
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		if a.Pkg != b.Pkg {
			return a.Pkg < b.Pkg
		}
		return a.Recv+"."+a.Func < b.Recv+"."+b.Func
	})
	return regs, nil
}

// recvName is a method's receiver type name without pointer or type
// parameters, or "" for a function.
func recvName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return ""
	}
	t := fd.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	switch x := t.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.IndexExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.IndexListExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}

// returns reports whether fd returns T or *T among its results.
func returns(fd *ast.FuncDecl, typeName string) bool {
	if fd.Type.Results == nil {
		return false
	}
	for _, r := range fd.Type.Results.List {
		t := r.Type
		if star, ok := t.(*ast.StarExpr); ok {
			t = star.X
		}
		if id, ok := t.(*ast.Ident); ok && id.Name == typeName {
			return true
		}
	}
	return false
}

// callsConstructor reports whether f calls one of r's constructors: as
// alias.Name through an import of r.Pkg, or as a bare Name inside r.Pkg.
func callsConstructor(f *ast.File, imports map[string]string, pkg string, r registrant) (string, bool) {
	var via string
	ast.Inspect(f, func(n ast.Node) bool {
		if via != "" {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			x, ok := fn.X.(*ast.Ident)
			if !ok || imports[x.Name] != r.Pkg {
				return true
			}
			for _, c := range r.Constructors {
				if fn.Sel.Name == c {
					via = r.Pkg + "." + c
				}
			}
		case *ast.Ident:
			if pkg != r.Pkg {
				return true
			}
			for _, c := range r.Constructors {
				if fn.Name == c {
					via = r.Pkg + "." + c
				}
			}
		}
		return true
	})
	return via, via != ""
}
