package pipelines

import (
	"fmt"
	"go/build"
	"io"
	"io/fs"
	"path"
	"strings"
)

// packageInputs is source-derived compiler input information. It is read only
// for ancestors of an explicitly declared changed test input, once per package
// in an analysis. No go command, host file, environment or dependency is read.
type packageInputs struct {
	buildFiles map[string]bool
	runtime    []string
	test       []string
	err        error
}

func fullSelectionPath(p ChangedPathFacts) bool {
	return p.Vendored || p.ConfiguredFull || p.Path == ManifestPath ||
		p.BaseName == "go.mod" || p.BaseName == "go.sum" ||
		p.BaseName == "go.work" || p.BaseName == "go.work.sum"
}

func (g *Graph) classifyTestInput(p *ChangedPathFacts, cache map[string]packageInputs) {
	if fullSelectionPath(*p) {
		return // A declaration cannot turn a compiler/policy input into a test input.
	}
	if len(p.owners) == 0 || !regularTreeFile(g.source, p.Path) {
		p.TestInputUncertain = true
		return
	}
	runtimeInput := strings.HasSuffix(p.Path, ".go")
	// An ancestor may embed a descendant package's files. Include every such
	// owner, and stop at the module boundary which Go embeds cannot cross.
	for dir := path.Dir(p.Path); ; dir = path.Dir(dir) {
		if owner, ok := g.byDir[dir]; ok {
			inputs, cached := cache[dir]
			if !cached {
				inputs = g.readPackageInputs(dir)
				cache[dir] = inputs
			}
			if inputs.err != nil {
				p.TestInputUncertain = true
				return
			}
			rel := p.Path
			if dir != "." {
				rel = strings.TrimPrefix(p.Path, dir+"/")
			}
			if inputs.buildFiles[rel] || matchesEmbed(inputs.runtime, rel) {
				runtimeInput = true
				p.owners = append(p.owners, owner)
			}
			if matchesEmbed(inputs.test, rel) {
				p.owners = append(p.owners, owner)
			}
		}
		if _, moduleRoot := g.modules[dir]; moduleRoot || dir == "." {
			break
		}
	}
	p.TestInputEligible = !runtimeInput
}

func regularTreeFile(fsys fs.FS, name string) bool {
	if fsys == nil {
		return false
	}
	entries, err := fs.ReadDir(fsys, path.Dir(name))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.Name() == path.Base(name) && entry.Type().IsRegular() {
			f, err := fsys.Open(name)
			if err != nil {
				return false
			}
			return f.Close() == nil
		}
	}
	return false
}

func (g *Graph) readPackageInputs(dir string) packageInputs {
	result := packageInputs{buildFiles: map[string]bool{}}
	// Use Go's own directive parser, including quoted/all: patterns and test
	// embeds. Each file is read separately so mutually exclusive tagged files
	// with different package names still form a conservative union. UseAllFiles
	// includes every target/tag; custom filesystem hooks prevent go/build from
	// delegating to a toolchain or consulting the operator's filesystem.
	for _, file := range g.goFiles[dir] {
		ctxt := sourceBuildContext(g.source, path.Base(file))
		pkg, err := ctxt.ImportDir(path.Join("/", dir), build.IgnoreVendor)
		if err != nil {
			result.err = fmt.Errorf("read build inputs for %s: %w", file, err)
			return result
		}
		for _, names := range [][]string{pkg.GoFiles, pkg.CgoFiles, pkg.CFiles, pkg.CXXFiles,
			pkg.MFiles, pkg.HFiles, pkg.FFiles, pkg.SFiles, pkg.SwigFiles,
			pkg.SwigCXXFiles, pkg.SysoFiles, pkg.IgnoredOtherFiles} {
			for _, name := range names {
				result.buildFiles[name] = true
			}
		}
		result.runtime = append(result.runtime, pkg.EmbedPatterns...)
		result.test = append(result.test, pkg.TestEmbedPatterns...)
		result.test = append(result.test, pkg.XTestEmbedPatterns...)
	}
	for _, patterns := range [][]string{result.runtime, result.test} {
		for _, pattern := range patterns {
			pattern = strings.TrimPrefix(pattern, "all:")
			_, err := path.Match(pattern, "")
			if err != nil || pattern == "." || !fs.ValidPath(pattern) {
				result.err = fmt.Errorf("invalid embed pattern in %s", dir)
				return result
			}
		}
	}
	return result
}

// A matched directory embeds its descendants. This deliberately also matches
// hidden files and nested package directories: extra owners widen selection.
// The module boundary is enforced by classifyTestInput. No actual EmbedFiles,
// TestEmbedFiles or XTestEmbedFiles match may be lost to a target/tag filter.
func matchesEmbed(patterns []string, name string) bool {
	for _, pattern := range patterns {
		pattern = strings.TrimPrefix(pattern, "all:")
		for p := name; p != "."; p = path.Dir(p) {
			if matched, _ := path.Match(pattern, p); matched {
				return true
			}
		}
	}
	return false
}

func sourceBuildContext(fsys fs.FS, goFile string) build.Context {
	nameOf := func(name string) (string, error) {
		if name == "/" {
			return ".", nil
		}
		name = strings.TrimPrefix(name, "/")
		if !fs.ValidPath(name) {
			return "", fs.ErrInvalid
		}
		return name, nil
	}
	return build.Context{
		GOOS: "linux", GOARCH: "amd64", Compiler: "gc", Dir: "/",
		UseAllFiles: true, CgoEnabled: true,
		JoinPath: path.Join, IsAbsPath: path.IsAbs,
		SplitPathList: func(string) []string { return nil },
		HasSubdir: func(root, dir string) (string, bool) {
			prefix := strings.TrimSuffix(root, "/") + "/"
			return strings.TrimPrefix(dir, prefix), strings.HasPrefix(dir, prefix)
		},
		IsDir: func(name string) bool {
			name, err := nameOf(name)
			if err != nil {
				return false
			}
			info, err := fs.Stat(fsys, name)
			return err == nil && info.IsDir()
		},
		OpenFile: func(name string) (io.ReadCloser, error) {
			name, err := nameOf(name)
			if err != nil {
				return nil, err
			}
			return fsys.Open(name)
		},
		ReadDir: func(name string) ([]fs.FileInfo, error) {
			name, err := nameOf(name)
			if err != nil {
				return nil, err
			}
			entries, err := fs.ReadDir(fsys, name)
			if err != nil {
				return nil, err
			}
			var out []fs.FileInfo
			for _, entry := range entries {
				if !entry.Type().IsRegular() || (strings.HasSuffix(entry.Name(), ".go") && entry.Name() != goFile) {
					continue
				}
				info, err := entry.Info()
				if err != nil {
					return nil, err
				}
				out = append(out, info)
			}
			return out, nil
		},
	}
}
