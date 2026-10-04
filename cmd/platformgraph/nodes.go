package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/architecture/extract"
	"github.com/znasllc-io/memql/component/architecture/model"
	"github.com/znasllc-io/memql/component/node"
)

// listedPackage is the slice of `go list -json` the passes read. Dir and
// GoFiles locate the sources the services pass parses; they are never written
// into the graph, which carries no path outside the repository.
type listedPackage struct {
	ImportPath string
	Name       string
	Dir        string
	GoFiles    []string
	Standard   bool
}

// roleBuild is one role and the repository packages its binary links.
type roleBuild struct {
	role node.NodeRole
	pkgs []listedPackage
}

// buildEnv pins what decides a package set to the image's build: linux/amd64
// with cgo off, and GOFLAGS cleared so a caller's `-tags` cannot leak into the
// per-role lists (the Makefile's GOFLAGS=-v reaches here too). Without the pin
// the graph would differ between a developer's darwin laptop and the CI
// runner, and the drift gate would be red for whoever did not regenerate.
func buildEnv() []string {
	env := make([]string, 0, len(os.Environ())+4)
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "GOFLAGS="), strings.HasPrefix(kv, "GOOS="),
			strings.HasPrefix(kv, "GOARCH="), strings.HasPrefix(kv, "CGO_ENABLED="):
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GOFLAGS=", "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
}

// modulePath reads the root module's path from go.mod.
func modulePath(root string) (string, error) {
	f, err := os.Open(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", errors.New("go.mod declares no module")
}

// listRole lists every repository package the root binary links under tag.
func listRole(root, module, tag string) ([]listedPackage, error) {
	cmd := exec.Command("go", "list", "-deps", "-tags", tag, "-json=ImportPath,Name,Dir,GoFiles,Standard", ".")
	cmd.Dir = root
	cmd.Env = buildEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -tags %s: %v\n%s", tag, err, stderr.String())
	}
	var pkgs []listedPackage
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listedPackage
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode go list -tags %s: %w", tag, err)
		}
		if p.Standard || !(p.ImportPath == module || strings.HasPrefix(p.ImportPath, module+"/")) {
			continue
		}
		pkgs = append(pkgs, p)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("go list -tags %s: no repository packages", tag)
	}
	return pkgs, nil
}

// packageLabel is an import path relative to the module: what a diagram prints.
func packageLabel(importPath, module string) string {
	if importPath == module {
		return "."
	}
	return strings.TrimPrefix(importPath, module+"/")
}

var engineNodeTypesRe = regexp.MustCompile(`(?m)^ENGINE_NODE_TYPES=\(([^)]*)\)`)

// verifyRoleLists refuses to describe a platform whose hand-kept role lists
// disagree with component/node/roles.go: the app/build_<type>.go files, the
// ENGINE_NODE_TYPES shell list (in order) and the root arch.yaml's roles (name,
// mesh and description, in order). component/node/roles_test.go asserts the
// same; this is the copy that runs wherever the platform graph's gate does --
// which is every lane, including a docs- or manifest-only PR that never reaches
// component/node's tests.
func verifyRoleLists(root string) error {
	roles := node.Roles()
	var names []string
	for _, r := range roles {
		names = append(names, string(r.Type))
	}

	matches, err := filepath.Glob(filepath.Join(root, "app", "build_*.go"))
	if err != nil {
		return err
	}
	var files []string
	for _, m := range matches {
		base := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "build_"), ".go")
		if base != "default" && !strings.HasSuffix(base, "_test") {
			files = append(files, base)
		}
	}
	sortedNames := append([]string(nil), names...)
	sort.Strings(sortedNames)
	sort.Strings(files)
	if strings.Join(files, ",") != strings.Join(sortedNames, ",") {
		return fmt.Errorf("app/build_<type>.go (%s) != component/node roles (%s)", strings.Join(files, ","), strings.Join(sortedNames, ","))
	}

	raw, err := os.ReadFile(filepath.Join(root, "scripts", "lib", "engine_build_args.sh"))
	if err != nil {
		return err
	}
	m := engineNodeTypesRe.FindStringSubmatch(string(raw))
	if m == nil {
		return errors.New("scripts/lib/engine_build_args.sh has no literal ENGINE_NODE_TYPES=( ... )")
	}
	if got := strings.Join(strings.Fields(m[1]), " "); got != strings.Join(names, " ") {
		return fmt.Errorf("ENGINE_NODE_TYPES (%s) != component/node roles in order (%s)", got, strings.Join(names, " "))
	}

	arch, err := extract.LoadArchYAML(root)
	if err != nil {
		return err
	}
	if len(arch.Roles) != len(roles) {
		return fmt.Errorf("arch.yaml declares %d roles, component/node %d", len(arch.Roles), len(roles))
	}
	for i, r := range roles {
		a := arch.Roles[i]
		if a.Name != string(r.Type) || a.Mesh != r.Mesh || a.Description != r.Description {
			return fmt.Errorf("arch.yaml role %d {%s mesh=%t %q} != component/node {%s mesh=%t %q}",
				i, a.Name, a.Mesh, a.Description, r.Type, r.Mesh, r.Description)
		}
	}
	return nil
}

// nodesPass adds one node per role and one per package some role links, with
// an includes edge from each role to each package its binary links. It returns
// the per-role package lists for the services pass.
func nodesPass(b *builder, root string) ([]roleBuild, error) {
	module, err := modulePath(root)
	if err != nil {
		return nil, err
	}
	if err := verifyRoleLists(root); err != nil {
		return nil, err
	}
	var builds []roleBuild
	for _, r := range node.Roles() {
		b.node(model.ServiceID(string(r.Type)), model.PlatformRole, string(r.Type), map[string]string{
			"description": r.Description,
			"mesh":        fmt.Sprintf("%t", r.Mesh),
		})
		pkgs, err := listRole(root, module, string(r.Type))
		if err != nil {
			return nil, err
		}
		for _, p := range pkgs {
			b.node(model.PackageID(p.ImportPath), model.PlatformPackage, packageLabel(p.ImportPath, module), nil)
			b.edge(model.ServiceID(string(r.Type)), model.PackageID(p.ImportPath), model.PlatformIncludes, nil)
		}
		builds = append(builds, roleBuild{role: r, pkgs: pkgs})
	}
	return builds, nil
}
