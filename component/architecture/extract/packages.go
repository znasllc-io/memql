package extract

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/znasllc-io/memql/component/architecture/model"
)

// ServicePlan pairs a module directory with its arch.yaml. The
// extractor builds one of these per module in the workspace, then
// walks them in order to populate the model.
type ServicePlan struct {
	ModuleDir string
	Arch      *ArchYAML
	// ModulePath is the Go module path (e.g. "github.com/znasllc-io/memql"),
	// resolved by the package loader. Used to identify which loaded
	// packages "belong" to this service for the Contains edge.
	ModulePath string
}

// Plan resolves the workspace into ServicePlans by reading each
// module's go.mod (via the package loader, so module-replace
// directives are honored).
//
// A GO MODULE IS NOT A SERVICE (memql#3244).
//
// If any module in the workspace declares an arch.yaml, ONLY the declaring
// modules become services. Undeclared modules are skipped: a repository that
// has adopted the model owns its own service decomposition, and its declared
// roots say which packages belong to which service. If NO module declares one,
// every module is planned -- so an unadopted repo keeps the previous behaviour
// and arch.yaml stays opt-in exactly as its own header promises.
//
// Planning every module was harmless while the repo was one module. The module
// split (epic memql#3228) made it wrong in two ways at once:
//
//   - MODELLING. memQL's root arch.yaml declares ONE service whose roots are
//     `github.com/znasllc-io/memql/...` -- the whole workspace. Every nested
//     module then ALSO became a service named `filepath.Base(moduleDir)`, so
//     the cockpit's Topology tab grew 20 nodes that are Go modules rather than
//     services -- three of them (`component/{grpc,node,bus}/gen`) colliding on
//     the single id `service:gen`.
//   - COST. Each plan is its own packages.Load with NeedDeps|NeedTypes, and
//     ExtractTypes holds every one live at once to compute Implements across
//     services. Separate Loads share nothing, so each module re-materialises
//     the type universe beneath it. Measured at 48 modules: 30.8 GB peak RSS
//     and 84s, against a 16 GB CI runner -- go-tests died with SIGTERM twice,
//     mid-run, inside the model regeneration this gate shells out to. Planning
//     only the declared service: 2.4 GB and 3s.
//
// The model is not narrowed by this. The declared roots still span the whole
// workspace, so the same package and symbol nodes are emitted; what disappears
// is the phantom services and their duplicate containment edges.
func Plan(ws *Workspace) ([]ServicePlan, error) {
	declared := make(map[string]bool, len(ws.Modules))
	for _, dir := range ws.Modules {
		if _, err := os.Stat(filepath.Join(dir, ArchYAMLFilename)); err == nil {
			declared[dir] = true
		}
	}

	plans := make([]ServicePlan, 0, len(ws.Modules))
	for _, dir := range ws.Modules {
		if len(declared) > 0 && !declared[dir] {
			continue
		}
		arch, err := LoadArchYAML(dir)
		if err != nil {
			return nil, err
		}
		modPath, err := readModulePath(dir)
		if err != nil {
			return nil, fmt.Errorf("read module path for %s: %w", dir, err)
		}
		plans = append(plans, ServicePlan{
			ModuleDir:  dir,
			Arch:       arch,
			ModulePath: modPath,
		})
	}
	return plans, nil
}

// ExtractPackages populates m with Service nodes and the Package
// nodes contained in each. Cross-package import edges are added
// inside the same service; cross-service imports become DependsOn
// edges between services in addition to the underlying Imports edges.
//
// Behavior is intentionally conservative: standard library and
// third-party packages are not added as nodes -- we only model code
// the team owns. They still appear implicitly as the "absent target"
// of an import edge that the renderer filters out.
func ExtractPackages(m *model.Model, plans []ServicePlan, workspaceRoot string) error {
	// Index module paths so we can tell "is this package one of ours?"
	// by prefix-matching the package import path against any module path.
	type modOwner struct {
		service string
		modPath string
	}
	owners := make([]modOwner, 0, len(plans))
	for _, p := range plans {
		owners = append(owners, modOwner{service: p.Arch.Service, modPath: p.ModulePath})
	}
	resolveOwner := func(importPath string) (service string, ok bool) {
		// Match longest module path first to disambiguate nested modules.
		bestLen := -1
		for _, o := range owners {
			if importPath == o.modPath || strings.HasPrefix(importPath, o.modPath+"/") {
				if len(o.modPath) > bestLen {
					bestLen = len(o.modPath)
					service = o.service
				}
			}
		}
		return service, bestLen >= 0
	}

	for _, plan := range plans {
		svcID := model.ServiceID(plan.Arch.Service)
		m.Nodes = append(m.Nodes, model.Node{
			ID:   svcID,
			Kind: model.KindService,
			Name: plan.Arch.Service,
			Doc:  plan.Arch.Description,
			Attrs: pruneEmpty(map[string]string{
				"display_name": plan.Arch.DisplayName,
				"module_path":  plan.ModulePath,
			}),
		})

		cfg := &packages.Config{
			Mode: packages.NeedName |
				packages.NeedFiles |
				packages.NeedImports |
				packages.NeedDeps |
				packages.NeedModule,
			Dir:   plan.ModuleDir,
			Tests: false,
		}
		patterns := plan.Arch.Roots
		if len(patterns) == 0 {
			patterns = []string{"./..."}
		}
		pkgs, err := packages.Load(cfg, patterns...)
		if err != nil {
			return fmt.Errorf("load packages for %s: %w", plan.Arch.Service, err)
		}
		// packages.Load surfaces per-package errors via pkg.Errors.
		// We collect a representative sample but don't abort the run
		// on isolated breakage -- a service still produces useful
		// output even if one subpackage doesn't compile.
		var firstErr error
		packages.Visit(pkgs, nil, func(p *packages.Package) {
			if len(p.Errors) == 0 || firstErr != nil {
				return
			}
			firstErr = fmt.Errorf("%s: %s", p.PkgPath, p.Errors[0])
		})
		if firstErr != nil {
			fmt.Printf("memql-arch: warning: %v\n", firstErr)
		}

		seen := make(map[string]bool)
		excludes := compileExcludes(plan.Arch.Excludes, plan.ModulePath)

		// Two-pass walk: first add all Package nodes belonging to this
		// service so import edges in the second pass can refer to them
		// by ID without ordering hazards.
		var include []*packages.Package
		packages.Visit(pkgs, nil, func(p *packages.Package) {
			if seen[p.PkgPath] {
				return
			}
			seen[p.PkgPath] = true
			owner, ok := resolveOwner(p.PkgPath)
			if !ok || owner != plan.Arch.Service {
				return
			}
			if excludes(p.PkgPath) {
				return
			}
			include = append(include, p)
			pkgID := model.PackageID(p.PkgPath)
			m.Nodes = append(m.Nodes, model.Node{
				ID:     pkgID,
				Kind:   model.KindPackage,
				Name:   shortPkgName(p.PkgPath, plan.ModulePath),
				Parent: svcID,
				Attrs:  pruneEmpty(map[string]string{"import_path": p.PkgPath}),
				Source: firstSource(p, workspaceRoot),
			})
			m.Edges = append(m.Edges, model.Edge{
				From: svcID,
				To:   pkgID,
				Kind: model.EdgeContains,
			})
		})

		// Cross-service edges are aggregated so we don't emit
		// duplicate DependsOn for every import that crosses the same
		// boundary -- one edge per (src service, dst service) pair,
		// with the import count carried in Attrs for renderer
		// weighting.
		serviceDeps := make(map[string]int) // dstService -> import count

		for _, p := range include {
			srcID := model.PackageID(p.PkgPath)
			for _, imp := range p.Imports {
				dstService, ownerOk := resolveOwner(imp.PkgPath)
				if !ownerOk {
					continue // stdlib / third-party -- silently dropped
				}
				dstID := model.PackageID(imp.PkgPath)
				m.Edges = append(m.Edges, model.Edge{
					From: srcID,
					To:   dstID,
					Kind: model.EdgeImports,
				})
				if dstService != plan.Arch.Service {
					serviceDeps[dstService]++
				}
			}
		}

		// Promote explicit arch.yaml depends_on declarations -- they
		// exist for relationships the import graph can't see (e.g.
		// gRPC peer talking over the network). Counted as 0 so the
		// renderer can distinguish "imported" from "declared."
		for _, dst := range plan.Arch.DependsOn {
			if _, ok := serviceDeps[dst]; !ok {
				serviceDeps[dst] = 0
			}
		}
		for dst, count := range serviceDeps {
			m.Edges = append(m.Edges, model.Edge{
				From: svcID,
				To:   model.ServiceID(dst),
				Kind: model.EdgeDependsOn,
				Attrs: map[string]string{
					"import_count": fmt.Sprintf("%d", count),
				},
			})
		}
	}
	return nil
}

// shortPkgName trims the module-path prefix from a package's import
// path so the on-screen label is the part that actually varies. The
// root package of the module renders as "." rather than its full
// module path.
func shortPkgName(importPath, modulePath string) string {
	if importPath == modulePath {
		return "."
	}
	if strings.HasPrefix(importPath, modulePath+"/") {
		return strings.TrimPrefix(importPath, modulePath+"/")
	}
	return importPath
}

// firstSource returns a SourceRef for the first Go file in a package,
// path normalized relative to the workspace root. Used as a "click
// target" by cockpit -- highlighting a Package node and pressing
// Enter could open the file in the explorer.
func firstSource(p *packages.Package, workspaceRoot string) *model.SourceRef {
	if len(p.GoFiles) == 0 {
		return nil
	}
	rel, ok := workspaceRelative(workspaceRoot, p.GoFiles[0])
	if !ok {
		return nil
	}
	return &model.SourceRef{File: rel}
}

// compileExcludes turns a list of exclude patterns into a predicate.
// Patterns are matched as package-path prefixes after stripping the
// "./" prefix and resolving against the module path; "/..." at the
// end is treated as "this package and everything under it."
func compileExcludes(patterns []string, modulePath string) func(string) bool {
	prefixes := make([]string, 0, len(patterns))
	for _, raw := range patterns {
		p := strings.TrimPrefix(raw, "./")
		p = strings.TrimSuffix(p, "/...")
		if p == "" {
			continue
		}
		prefixes = append(prefixes, modulePath+"/"+p)
	}
	if len(prefixes) == 0 {
		return func(string) bool { return false }
	}
	return func(pkgPath string) bool {
		for _, pref := range prefixes {
			if pkgPath == pref || strings.HasPrefix(pkgPath, pref+"/") {
				return true
			}
		}
		return false
	}
}

func pruneEmpty(m map[string]string) map[string]string {
	for k, v := range m {
		if v == "" {
			delete(m, k)
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// workspaceRelative renders path relative to workspaceRoot, reporting false
// when it falls OUTSIDE the workspace.
//
// This is what makes the model reproducible across machines (memql#2844).
// filepath.Rel happily walks upwards, so a file in GOROOT or the module cache
// came out as a `../../..` chain whose LENGTH encodes how deep the checkout
// sits on that particular disk:
//
//	/home/znas/memql-projects/wt-2844  ->  ../../../../usr/local/go/src/sync/mutex.go
//	/home/znas/depthtest               ->  ../../../usr/local/go/src/sync/mutex.go
//
// Twenty-seven nodes carried such paths -- promoted methods from embedded
// stdlib and dependency types -- so the artifact differed between two
// checkouts of the SAME COMMIT on the same machine, and differed again if
// GOMODCACHE moved. On CI the GOROOT path additionally embeds the Go PATCH
// VERSION. A drift gate over that is unsatisfiable: it goes green on the
// machine that regenerated and red everywhere else.
//
// Dropped rather than symbolised. These are files the cockpit cannot open
// anyway -- they are not in the workspace it has checked out -- so a SourceRef
// pointing at them is a click target that cannot work. A node keeps its
// identity, kind, name and parent; it just has no source position, which is
// the honest answer for code that lives outside the tree being modelled.
func workspaceRelative(workspaceRoot, path string) (string, bool) {
	rel, err := filepath.Rel(workspaceRoot, path)
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}
