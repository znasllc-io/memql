package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/znasllc-io/memql/component/architecture/model"
)

// osNavigation is the OS navigation contract the engine embeds
// (component/memql/work_navigation.go), generated from the OS's own app
// manifests. Read as a file rather than through component/memql, which keeps
// its decoded form unexported.
var osNavigation = filepath.Join("component", "memql", "os_navigation.json")

type navApp struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Requires string `json:"requires"`
	Sections []struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		Requires       string `json:"requires"`
		SourceRequires string `json:"sourceRequires"`
	} `json:"sections"`
	Records []struct {
		Section string   `json:"section"`
		IDField string   `json:"idField"`
		Query   string   `json:"query"`
		Labels  []string `json:"labels"`
	} `json:"records"`
}

// osPass records every OS app and its sections, with the capability each
// requires and, for a section that lists records, the query it lists them by.
func osPass(b *builder, root string) error {
	raw, err := os.ReadFile(filepath.Join(root, osNavigation))
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var apps []navApp
	if err := dec.Decode(&apps); err != nil {
		return fmt.Errorf("%s: %w", osNavigation, err)
	}
	if len(apps) == 0 {
		return fmt.Errorf("%s lists no apps", osNavigation)
	}
	for _, a := range apps {
		b.node(model.OSAppID(a.ID), model.PlatformOSApp, a.Name, map[string]string{"requires": a.Requires})
		queries := map[string][]string{}
		for _, r := range a.Records {
			queries[r.Section] = append(queries[r.Section], r.Query)
		}
		for _, s := range a.Sections {
			attrs := map[string]string{
				"requires": s.Requires,
				"records":  strings.Join(queries[s.ID], ","),
			}
			// Moved sections retain both original grants. Keep the extra
			// app requirement in the graph instead of flattening it away.
			if s.SourceRequires != "" {
				attrs["sourceRequires"] = s.SourceRequires
			}
			b.node(model.OSSectionID(a.ID, s.ID), model.PlatformOSSection, s.Name, attrs)
			b.edge(model.OSAppID(a.ID), model.OSSectionID(a.ID, s.ID), model.PlatformContains, nil)
		}
	}
	return nil
}
