package model

import "fmt"

// Constructors for the canonical ID format. Centralizing these keeps
// the format invariant in one place; extractors and observability
// joiners both go through these functions instead of re-encoding the
// string templates.

// ClusterID builds an ID for a Cluster node.
func ClusterID(name string) ID {
	return ID(fmt.Sprintf("cluster:%s", name))
}

// ServiceID builds an ID for a Service node.
func ServiceID(name string) ID {
	return ID(fmt.Sprintf("service:%s", name))
}

// PackageID builds an ID for a Package node from its Go import path.
func PackageID(importPath string) ID {
	return ID(fmt.Sprintf("pkg:%s", importPath))
}

// TypeID builds an ID for a struct (or other non-interface named
// type). pkgPath is the containing Go import path.
func TypeID(pkgPath, typeName string) ID {
	return ID(fmt.Sprintf("type:%s.%s", pkgPath, typeName))
}

// InterfaceID builds an ID for an interface type.
func InterfaceID(pkgPath, typeName string) ID {
	return ID(fmt.Sprintf("iface:%s.%s", pkgPath, typeName))
}

// FuncID builds an ID for a top-level function.
func FuncID(pkgPath, funcName string) ID {
	return ID(fmt.Sprintf("func:%s.%s", pkgPath, funcName))
}

// MethodID builds an ID for a method. recvType is the receiver type
// name without pointer indirection -- pointer vs value receivers are
// recorded in the Method node's Attrs, not the ID, so the same
// logical method has one identity regardless of receiver style.
func MethodID(pkgPath, recvType, methodName string) ID {
	return ID(fmt.Sprintf("method:%s.(%s).%s", pkgPath, recvType, methodName))
}

// FieldID builds an ID for a struct field.
func FieldID(pkgPath, typeName, fieldName string) ID {
	return ID(fmt.Sprintf("field:%s.%s.%s", pkgPath, typeName, fieldName))
}
