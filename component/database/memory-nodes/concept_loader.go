package memoryNodes

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"strings"

	"github.com/visionarys-io/memql/component/database"
	"github.com/visionarys-io/memql/core/env"
)

var conceptFS = database.Concepts

const conceptsRoot = "."

var (
	// Concept directory segments are camelCase identifiers: lowercase
	// start (so `v1`, `v2` ... still resolve as version dirs via the
	// separate version pattern below), then any mix of letters and
	// digits. No separators (no `_`, no `-`, no `.`). Allows
	// `audioOverride` and `accessRequest`; rejects `audio-override`
	// and `Audio_Override`.
	conceptDirSegmentPattern = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*$`)
	conceptVersionPattern    = regexp.MustCompile(`^v[0-9]+$`)
)

// newConceptFromDir constructs a Concept by loading concept.memql from the provided directory path.
func newConceptFromDir(dir string) (*Concept, error) {
	if dir == "" {
		return nil, fmt.Errorf("concept schema directory path is required")
	}

	cleanDir := strings.TrimSuffix(dir, "/")

	memqlPath := path.Join(cleanDir, "concept.memql")
	memqlBytes, err := fs.ReadFile(conceptFS, memqlPath)
	if err != nil {
		return nil, fmt.Errorf("concept directory %s is missing concept.memql", cleanDir)
	}

	return ParseConceptMemQL(memqlBytes, cleanDir)
}

// LoadConcepts loads concepts into the global singleton registry,
// filtering by the compiled node type's @visibility annotation. Concepts
// with @visibility("*") or no @visibility load on all nodes. Concepts with
// @visibility("cognition") only load when nodeType is "cognition".
func LoadConcepts(logger *slog.Logger, nodeType string) error {
	return loadDefaultRegistry(logger, nodeType)
}

func loadDefaultRegistry(logger *slog.Logger, nodeType string) error {
	concepts, err := loadAllConcepts(logger)
	if err != nil {
		return err
	}

	// Filter by @visibility annotation. Concepts whose visibility
	// doesn't include the compiled node type are excluded from the
	// registry. No annotation = not visible (opt-in model).
	if nodeType != "" {
		filtered := make(map[string]*Concept, len(concepts))
		for name, concept := range concepts {
			if concept.Visibility.IsVisibleTo(nodeType) {
				filtered[name] = concept
			} else {
				if logger != nil {
					logger.Debug("concept filtered by @visibility",
						"concept", name,
						"nodeType", nodeType,
					)
				}
			}
		}
		concepts = filtered
	}

	ReplaceAll(concepts)
	return nil
}

// loadAllConcepts loads concepts from all version directories (v1, v2, etc.).
func loadAllConcepts(logger *slog.Logger) (map[string]*Concept, error) {
	// Discover all version directories at the root
	versionDirs, err := discoverVersionDirectories(conceptFS, conceptsRoot)
	if err != nil {
		return nil, fmt.Errorf("discover version directories: %w", err)
	}

	if len(versionDirs) == 0 {
		if logger != nil {
			logger.Warn("no version directories found", "component", "memoryNodes")
		}
		return make(map[string]*Concept), nil
	}

	// Collect all paths from all version directories
	var allPaths []string
	for _, versionDir := range versionDirs {
		walker, err := env.NewWalker(conceptFS, versionDir, env.ForbidShared())
		if err != nil {
			if logger != nil {
				logger.Warn("failed to create walker for version directory",
					"component", "memoryNodes",
					"version", versionDir,
					"error", err)
			}
			continue
		}
		paths := walker.AllPaths()
		allPaths = append(allPaths, paths...)

		if logger != nil {
			logger.Info("loading concept folders",
				"component", "memoryNodes",
				"version", versionDir,
				"directories", walker.Directories(),
				"paths", paths,
			)
		}
	}

	return walkConceptDirectories(allPaths)
}

// discoverVersionDirectories finds all version directories (v1, v2, etc.) at the root.
func discoverVersionDirectories(fsys fs.FS, root string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		return nil, err
	}

	var versions []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		// Skip files starting with _ (documentation/reference files)
		if strings.HasPrefix(name, "_") {
			continue
		}
		// Only include directories matching v<number> pattern
		if conceptVersionPattern.MatchString(name) {
			versions = append(versions, name)
		}
	}

	return versions, nil
}

func walkConceptDirectories(paths []string) (map[string]*Concept, error) {
	loaded := make(map[string]*Concept)
	origins := make(map[string]string)

	// Load salt configuration for content-addressed IDs
	envOpts, err := LoadDefaultEnvOptions()
	if err != nil {
		return nil, fmt.Errorf("load content ID configuration: %w", err)
	}

	for _, basePath := range paths {
		err := fs.WalkDir(conceptFS, basePath, func(dirPath string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !d.IsDir() {
				return nil
			}
			if dirPath == basePath {
				return nil
			}

			memqlPath := path.Join(dirPath, "concept.memql")
			if _, openErr := conceptFS.Open(memqlPath); openErr != nil {
				if errors.Is(openErr, fs.ErrNotExist) {
					return nil
				}
				return fmt.Errorf("open concept.memql for %s: %w", dirPath, openErr)
			}

			concept, loadErr := newConceptFromDir(dirPath)
			if loadErr != nil {
				return fmt.Errorf("load concept at %q: %w", dirPath, loadErr)
			}

			if previous, exists := origins[concept.Name]; exists {
				return fmt.Errorf("duplicate concept %q detected in %s and %s", concept.Name, previous, dirPath)
			}

			// Apply content ID salt to concept
			concept.SetContentIdSalt(envOpts.ContentIdSalt)

			loaded[concept.Name] = concept
			origins[concept.Name] = dirPath

			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}

	return loaded, nil
}

func ensureReservedFieldsNotDeclared(conceptName string, schemaBytes []byte) error {
	var definition struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []any                      `json:"required"`
	}
	if err := json.Unmarshal(schemaBytes, &definition); err != nil {
		return fmt.Errorf("parse definition schema for %s: %w", conceptName, err)
	}

	for name := range definition.Properties {
		if IsReservedPayloadField(name) {
			return fmt.Errorf("concept %s definition schema declares reserved property %q", conceptName, name)
		}
	}

	for _, entry := range definition.Required {
		if field, ok := entry.(string); ok && IsReservedPayloadField(field) {
			return fmt.Errorf("concept %s definition schema marks reserved property %q as required", conceptName, field)
		}
	}

	return nil
}

func deriveConceptName(dir string) (string, error) {
	cleanDir := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(dir), "./"), "/")
	if cleanDir == "" {
		return "", fmt.Errorf("concept schema directory path %q is invalid", dir)
	}

	trimmed := cleanDir
	if conceptsRoot != "." {
		if !strings.HasPrefix(cleanDir, conceptsRoot) {
			return "", fmt.Errorf("concept path %q must reside under %s/", dir, conceptsRoot)
		}
		trimmed = strings.TrimPrefix(cleanDir, conceptsRoot)
		trimmed = strings.TrimPrefix(trimmed, "/")
		if trimmed == "" {
			return "", fmt.Errorf("concept path %q must include a subdirectory under %s", dir, conceptsRoot)
		}
	}

	segments := strings.Split(trimmed, "/")
	if len(segments) < 3 {
		return "", fmt.Errorf("concept path %q must include version, environment, and concept directories", dir)
	}

	validated := make([]string, 0, len(segments))
	for idx, segment := range segments {
		if err := validateConceptDirSegment(segment); err != nil {
			return "", fmt.Errorf("concept path %q segment %q: %w", dir, segment, err)
		}
		if idx == 0 && !conceptVersionPattern.MatchString(segment) {
			return "", fmt.Errorf("concept path %q version directory %q must match v<integer>", dir, segment)
		}
		validated = append(validated, segment)
	}

	return strings.Join(validated, ":"), nil
}

func validateConceptDirSegment(segment string) error {
	trimmed := strings.TrimSpace(segment)
	if trimmed == "" {
		return fmt.Errorf("segment is empty")
	}
	if !conceptDirSegmentPattern.MatchString(trimmed) {
		return fmt.Errorf("must be a single lowercase alphanumeric word (no separators)")
	}
	return nil
}

// ConceptNameFromPath derives the canonical concept identifier from a directory path under concepts/.
func ConceptNameFromPath(dir string) (string, error) {
	return deriveConceptName(dir)
}
