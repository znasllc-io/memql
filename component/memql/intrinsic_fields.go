package memql

import "strings"

type intrinsicFieldKind int

const (
	intrinsicFieldUnknown intrinsicFieldKind = iota
	intrinsicFieldConcept
	intrinsicFieldId
	intrinsicFieldType
	intrinsicFieldCreatedAt
	intrinsicFieldCreatedBy
	intrinsicFieldPartition
)

type intrinsicFieldInfo struct {
	kind      intrinsicFieldKind
	canonical string
	column    string
}

var intrinsicFieldRegistry = map[string]intrinsicFieldInfo{
	"concept": {
		kind:      intrinsicFieldConcept,
		canonical: "concept",
		column:    "concept",
	},
	"id": {
		kind:      intrinsicFieldId,
		canonical: "id",
		column:    "id",
	},
	"type": {
		kind:      intrinsicFieldType,
		canonical: "type",
		column:    "type",
	},
	"createdat": {
		kind:      intrinsicFieldCreatedAt,
		canonical: "createdAt",
		column:    `"createdAt"`,
	},
	"createdby": {
		kind:      intrinsicFieldCreatedBy,
		canonical: "createdBy",
		column:    `"createdBy"`,
	},
	"partition": {
		kind:      intrinsicFieldPartition,
		canonical: "partition",
		column:    "partition",
	},
}

func resolveIntrinsicField(name string) (intrinsicFieldInfo, bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	info, ok := intrinsicFieldRegistry[key]
	return info, ok
}

func canonicalIntrinsicFieldName(name string) (string, bool) {
	info, ok := resolveIntrinsicField(name)
	if !ok {
		return "", false
	}
	return info.canonical, true
}
