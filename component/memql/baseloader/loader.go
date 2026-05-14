// Package baseloader holds the generic walk-and-register pipeline
// every unified loader uses: read all .memql files in the DSL tree,
// extract per-keyword slices, parse each slice via the per-construct
// parser, and register the result through a supplied callback.
//
// Per-construct loaders supply just three pieces:
//   - the keyword to slice on ("shape", "tool", "spec", ...)
//   - the parser fn turning raw text into *T (or []*T for slice-form)
//   - the register fn that hands the parsed entry to its registry
//
// The walk + slice + parse + log skeleton lives here.
package baseloader

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/visionarys-io/memql/component/memql/dslfs"
	memqldsl "github.com/visionarys-io/memql/dsl"
)

// RawFile is the (path, content) pair every loader iterates over.
type RawFile struct {
	Path    string
	Content string
}

// ReadAll walks dsl.Tree() and returns the (path, content) pair for
// every .memql file. Walk failures are logged and yield nil.
func ReadAll(logger *slog.Logger) []RawFile {
	tree := memqldsl.Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		if logger != nil {
			logger.Warn("baseloader: walk failed", "error", err)
		}
		return nil
	}
	var out []RawFile
	for _, p := range paths {
		f, openErr := tree.Open(p)
		if openErr != nil {
			continue
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			continue
		}
		out = append(out, RawFile{Path: p, Content: string(raw)})
	}
	return out
}

// Slice is the per-construct extraction result. baseloader doesn't
// know how slicing works (that's keyword-specific in the engine);
// callers supply Extract.
type Slice struct {
	Name   string
	Source string
}

// LoadOne walks every .memql file in the DSL tree, extracts slices
// for keyword via extract, parses each via parse, and registers each
// non-error result via register. Returns the count of registrations
// that succeeded. Per-slice errors (parse or register) are logged at
// debug and skipped; the loader pushes through to keep loader-init
// resilient against a single bad file.
//
//	component  -- log scope ("memql.unifiedShapeLoader" etc.)
//	keyword    -- the construct keyword passed to extract
//	extract    -- supplied by the caller (ExtractKeywordSlices)
//	parse      -- per-construct parser
//	register   -- registry.Upsert / .Add / .set / etc.
func LoadOne[T any](
	logger *slog.Logger,
	component string,
	keyword string,
	files []RawFile,
	extract func(content, keyword string) []Slice,
	parse func(origin string, raw []byte) (*T, error),
	register func(item *T) error,
) (int, error) {
	if extract == nil {
		return 0, fmt.Errorf("baseloader: extract fn is nil")
	}
	if parse == nil {
		return 0, fmt.Errorf("baseloader: parse fn is nil")
	}
	if register == nil {
		return 0, fmt.Errorf("baseloader: register fn is nil")
	}

	total := 0
	for _, raw := range files {
		for _, slice := range extract(raw.Content, keyword) {
			origin := "unified:" + raw.Path + ":" + slice.Name
			item, err := parse(origin, []byte(slice.Source))
			if err != nil {
				if logger != nil {
					logger.Debug(component+": parse failed",
						"file", raw.Path, keyword, slice.Name, "error", err)
				}
				continue
			}
			if err := register(item); err != nil {
				if logger != nil {
					logger.Debug(component+": register failed",
						"file", raw.Path, keyword, slice.Name, "error", err)
				}
				continue
			}
			total++
		}
	}
	if logger != nil {
		logger.Info(component+": registered",
			"component", component, "keyword", keyword, "count", total)
	}
	return total, nil
}

// LoadMany is the slice-form variant: parse returns []*T instead of
// *T. Used by parsers that yield multiple entries per slice
// (provider definitions bundle a vendor base + per-model entries).
func LoadMany[T any](
	logger *slog.Logger,
	component string,
	keyword string,
	files []RawFile,
	extract func(content, keyword string) []Slice,
	parse func(origin string, raw []byte) ([]*T, error),
	register func(item *T) error,
) (int, error) {
	if extract == nil {
		return 0, fmt.Errorf("baseloader: extract fn is nil")
	}
	if parse == nil {
		return 0, fmt.Errorf("baseloader: parse fn is nil")
	}
	if register == nil {
		return 0, fmt.Errorf("baseloader: register fn is nil")
	}

	total := 0
	for _, raw := range files {
		for _, slice := range extract(raw.Content, keyword) {
			origin := "unified:" + raw.Path + ":" + slice.Name
			items, err := parse(origin, []byte(slice.Source))
			if err != nil {
				if logger != nil {
					logger.Debug(component+": parse failed",
						"file", raw.Path, keyword, slice.Name, "error", err)
				}
				continue
			}
			for _, item := range items {
				if err := register(item); err != nil {
					if logger != nil {
						logger.Debug(component+": register failed",
							"file", raw.Path, keyword, slice.Name, "error", err)
					}
					continue
				}
				total++
			}
		}
	}
	if logger != nil {
		logger.Info(component+": registered",
			"component", component, "keyword", keyword, "count", total)
	}
	return total, nil
}
