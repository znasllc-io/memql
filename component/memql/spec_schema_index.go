package memql

import (
	"encoding/json"
	"fmt"
	"strings"

	concept "github.com/visionarys-io/memql/component/database/memory-nodes"
)

type schemaIndex struct {
	concepts map[string]*schemaNode
}

type schemaNode struct {
	properties      map[string]*schemaNode
	items           *schemaNode
	allowAdditional bool
	additional      *schemaNode
	anyOf           []*schemaNode
	allOf           []*schemaNode
	oneOf           []*schemaNode
}

func buildSchemaIndex(reg concept.Registry) (*schemaIndex, error) {
	if reg == nil {
		return nil, fmt.Errorf("concept registry is required")
	}
	list := reg.List()
	nodes := make(map[string]*schemaNode, len(list))
	for _, c := range list {
		if c == nil {
			continue
		}
		raw, err := c.DefinitionSchema()
		if err != nil {
			return nil, fmt.Errorf("concept %s definition schema: %w", c.Name, err)
		}
		node, err := parseSchemaNode(raw)
		if err != nil {
			return nil, fmt.Errorf("concept %s schema: %w", c.Name, err)
		}
		nodes[strings.TrimSpace(c.Name)] = node
	}
	return &schemaIndex{concepts: nodes}, nil
}

func parseSchemaNode(raw json.RawMessage) (*schemaNode, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("schema payload is empty")
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return schemaNodeFromMap(doc)
}

func schemaNodeFromMap(doc map[string]any) (*schemaNode, error) {
	node := &schemaNode{}

	if props, ok := doc["properties"].(map[string]any); ok {
		node.properties = make(map[string]*schemaNode, len(props))
		for key, value := range props {
			childMap, ok := value.(map[string]any)
			if !ok {
				node.properties[key] = &schemaNode{allowAdditional: true}
				continue
			}
			child, err := schemaNodeFromMap(childMap)
			if err != nil {
				return nil, err
			}
			node.properties[key] = child
		}
	}

	if items, ok := doc["items"].(map[string]any); ok {
		child, err := schemaNodeFromMap(items)
		if err != nil {
			return nil, err
		}
		node.items = child
	}

	switch add := doc["additionalProperties"].(type) {
	case bool:
		node.allowAdditional = add
	case map[string]any:
		child, err := schemaNodeFromMap(add)
		if err != nil {
			return nil, err
		}
		node.additional = child
	}

	node.anyOf = append(node.anyOf, parseSchemaNodeArray(doc["anyOf"])...)
	node.allOf = append(node.allOf, parseSchemaNodeArray(doc["allOf"])...)
	node.oneOf = append(node.oneOf, parseSchemaNodeArray(doc["oneOf"])...)

	return node, nil
}

func parseSchemaNodeArray(value any) []*schemaNode {
	array, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]*schemaNode, 0, len(array))
	for _, entry := range array {
		if entryMap, ok := entry.(map[string]any); ok {
			if child, err := schemaNodeFromMap(entryMap); err == nil {
				result = append(result, child)
			}
		}
	}
	return result
}
