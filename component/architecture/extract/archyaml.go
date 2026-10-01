package extract

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ArchYAMLFilename is the conventional name for the per-module
// architecture declaration. Looked up at the module root by the
// workspace walker.
const ArchYAMLFilename = "arch.yaml"

// ArchYAML is the on-disk schema for arch.yaml. It tells the
// extractor how to label a module in the model and which packages
// inside it to include. Missing fields default sensibly so a service
// can adopt the framework by dropping in an almost-empty file.
//
//	# arch.yaml
//	service: cognition
//	display_name: "Cognition Node"
//	description: "Reasoning + planning pipeline."
//	roots:
//	  - "./..."             # default if absent
//	excludes:
//	  - "./component/legacy/..."
//	depends_on:
//	  - bff                 # explicit cross-service edges; auto-derived imports are merged in
//	entrypoints:
//	  - func: "main.main"   # used by the sequence-diagram extractor (later milestone)
//	    label: "boot"
//	roles:
//	  - name: agent         # one service per node role the module builds
//	    mesh: true
//	    description: "Task execution."
//
// The schema is forgiving on extra fields so future extractors can
// add their own keys without breaking older versions; the YAML
// decoder is configured non-strict for that reason.
type ArchYAML struct {
	Service     string       `yaml:"service"`
	DisplayName string       `yaml:"display_name,omitempty"`
	Description string       `yaml:"description,omitempty"`
	Roots       []string     `yaml:"roots,omitempty"`
	Excludes    []string     `yaml:"excludes,omitempty"`
	DependsOn   []string     `yaml:"depends_on,omitempty"`
	Entrypoints []Entrypoint `yaml:"entrypoints,omitempty"`
	Roles       []ArchRole   `yaml:"roles,omitempty"`
}

// ArchRole is one node role a module builds: a binary compiled from the
// module under its own build tag. Each becomes a Service node of its own
// (service:<name>) under the cluster, depending on the module's service,
// so a live node type can name its code-side identity (the
// v1:cluster:nodeType.codeReference join) and the platform graph's role
// nodes share their ids with the architecture model (memql#5727).
//
// This package cannot know the role set -- it is base tier and imports
// nothing above it -- so the module declares it here. MemQL's own list is
// held to component/node/roles.go by that package's roles_test.go.
type ArchRole struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	Mesh        bool   `yaml:"mesh"`
}

// Entrypoint identifies a function the sequence-diagram extractor
// should walk forward from. Func is the fully-qualified name
// (pkg.Func or pkg.(*Recv).Method).
type Entrypoint struct {
	Func  string `yaml:"func"`
	Label string `yaml:"label,omitempty"`
}

// LoadArchYAML reads and parses arch.yaml from the given module
// directory. Returns a default ArchYAML with Service set to the
// directory base name when the file is absent -- adoption is opt-in
// at the level of "what extra metadata do I want," not "do I want
// to be analyzed at all."
func LoadArchYAML(moduleDir string) (*ArchYAML, error) {
	path := filepath.Join(moduleDir, ArchYAMLFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return defaultArchYAML(moduleDir), nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var a ArchYAML
	if err := yaml.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if a.Service == "" {
		a.Service = filepath.Base(moduleDir)
	}
	if len(a.Roots) == 0 {
		a.Roots = []string{"./..."}
	}
	return &a, nil
}

func defaultArchYAML(moduleDir string) *ArchYAML {
	return &ArchYAML{
		Service: filepath.Base(moduleDir),
		Roots:   []string{"./..."},
	}
}
