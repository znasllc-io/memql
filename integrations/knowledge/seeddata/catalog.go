// Package seeddata resolves immutable release knowledge, without caller content or database authority.
package seeddata

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/dsl"
	"io/fs"
	"sync"
)

// CatalogJSON compiles only the core catalog, before packs register their own content.
var CatalogJSON = sync.OnceValue(func() []byte {
	source, err := fs.ReadFile(dsl.Tree(), "knowledge/catalog/automations.memql")
	if err != nil {
		panic(err)
	}
	a, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(string(source), "knowledge/catalog/automations.memql")
	if err != nil {
		panic(err)
	}
	value, err := workflowhost.Run(context.Background(), "knowledgeSeedCatalog", nil, workflowhost.Options{
		Load: func(name string) (*automations.Automation, error) {
			if name != a.Name {
				return nil, fmt.Errorf("seed catalog cannot call %s", name)
			}
			return a, nil
		},
	})
	if err != nil {
		panic(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
})
