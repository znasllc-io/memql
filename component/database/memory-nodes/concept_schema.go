package memoryNodes

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

var compiledSchemaCache sync.Map

func compileSchema(cacheKey string, raw []byte) (*jsonschema.Schema, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("schema payload is empty")
	}
	if cached, ok := compiledSchemaCache.Load(cacheKey); ok {
		if schema, valid := cached.(*jsonschema.Schema); valid && schema != nil {
			return schema, nil
		}
	}

	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2019

	reader := bytes.NewReader(raw)
	if err := compiler.AddResource(cacheKey, reader); err != nil {
		return nil, fmt.Errorf("register schema resource: %w", err)
	}

	schema, err := compiler.Compile(cacheKey)
	if err != nil {
		return nil, fmt.Errorf("compile schema: %w", err)
	}

	compiledSchemaCache.Store(cacheKey, schema)
	return schema, nil
}
