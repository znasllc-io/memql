package memql

import (
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func applyPlanProjection(bundle *memqlv1.GraphBundle, plan *QueryPlan) {
	if bundle == nil || plan == nil {
		return
	}
	if len(plan.Fields) == 0 && len(plan.ConceptFields) == 0 && plan.Metadata.IncludeAll {
		return
	}
	applyProjection(bundle, plan.Fields, plan.ConceptFields, plan.Metadata, plan.PayloadSelect)
}

func applyProjection(bundle *memqlv1.GraphBundle, global []FieldReference, concept map[string][]FieldReference, meta metadataSelection, hasGlobal bool) {
	if bundle == nil {
		return
	}

	for i := range bundle.Nodes {
		node := bundle.Nodes[i]
		conceptName := ""
		if node != nil && node.Concept != "" {
			conceptName = node.Concept
		}
		fields, project := effectiveProjectionFields(conceptName, global, concept, hasGlobal)
		projectNode(node, fields, project, meta)
	}
}

func projectNode(node *memqlv1.MemoryNode, fields []FieldReference, projectPayload bool, meta metadataSelection) {
	if node == nil {
		return
	}

	projectMetadata(node, meta)

	if !projectPayload {
		return
	}

	if len(fields) == 0 {
		node.Payload = emptyStruct()
		return
	}

	selected := selectPayload(structToMap(node.Payload), fields)
	node.Payload = structFromMap(selected)
}

func selectPayload(original map[string]any, refs []FieldReference) map[string]any {
	if len(refs) == 0 {
		if original == nil {
			return map[string]any{}
		}
		return cloneMap(original)
	}

	payloadRefs := make([]FieldReference, 0, len(refs))
	for _, ref := range refs {
		if len(ref.Parts) == 0 {
			continue
		}
		if !strings.EqualFold(ref.Parts[0], "payload") {
			continue
		}
		if len(ref.Parts) == 1 {
			return cloneMap(original)
		}
		payloadRefs = append(payloadRefs, ref)
	}

	if len(payloadRefs) == 0 {
		return map[string]any{}
	}

	result := make(map[string]any)
	for _, ref := range payloadRefs {
		projectPayloadPath(result, original, ref.Parts[1:])
	}
	if len(result) == 0 {
		return map[string]any{}
	}
	return result
}

func projectPayloadPath(dest map[string]any, current map[string]any, parts []string) {
	if len(parts) == 0 || current == nil {
		return
	}

	segment := parts[0]
	if segment == "*" {
		for key, value := range current {
			dest[key] = cloneValue(value)
		}
		return
	}

	value, ok := current[segment]
	if !ok {
		return
	}

	if len(parts) == 1 {
		dest[segment] = cloneValue(value)
		return
	}

	childMap, ok := value.(map[string]any)
	if !ok {
		return
	}

	nextDest, _ := dest[segment].(map[string]any)
	if nextDest == nil {
		nextDest = make(map[string]any)
	}
	projectPayloadPath(nextDest, childMap, parts[1:])
	if len(nextDest) > 0 {
		dest[segment] = nextDest
	}
}

func cloneMap(input map[string]any) map[string]any {
	if input == nil {
		return map[string]any{}
	}
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = cloneValue(value)
	}
	return output
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMap(typed)
	case []any:
		result := make([]any, len(typed))
		for i, item := range typed {
			result[i] = cloneValue(item)
		}
		return result
	default:
		return typed
	}
}

func projectMetadata(node *memqlv1.MemoryNode, meta metadataSelection) {
	if node == nil || meta.IncludeAll {
		return
	}

	keep := make(map[string]struct{}, len(meta.Fields)+1)
	keep["id"] = struct{}{}
	for name := range meta.Fields {
		keep[name] = struct{}{}
	}

	if _, ok := keep["concept"]; !ok {
		node.Concept = ""
	}
	if _, ok := keep["type"]; !ok {
		node.Type = ""
	}
	if _, ok := keep["createdAt"]; !ok {
		node.CreatedAt = timestamppb.New(time.Now().UTC())
	}
	if _, ok := keep["createdBy"]; !ok {
		node.CreatedBy = ""
	}
	if _, ok := keep["schema"]; !ok {
		node.Schema = emptyStruct()
	}
}

func structToMap(data *structpb.Struct) map[string]any {
	if data == nil {
		return nil
	}
	return data.AsMap()
}

func structFromMap(data map[string]any) *structpb.Struct {
	if data == nil {
		data = map[string]any{}
	}
	st, err := structpb.NewStruct(data)
	if err != nil {
		return emptyStruct()
	}
	return st
}

func emptyStruct() *structpb.Struct {
	return &structpb.Struct{}
}
