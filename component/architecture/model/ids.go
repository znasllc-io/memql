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

// AutomationID identifies a DSL automation by its registered name.
func AutomationID(name string) ID { return ID("automation:" + name) }

// The constructors below name the platform graph's nodes (platform.go,
// memql#5727) that have no id in the code map. A role is a Service
// (ServiceID), a package is PackageID and an automation AutomationID, so
// those three join the architecture model, component/observe and
// v1:cluster:nodeType.codeReference on the same strings; everything else the
// deployment and the DSL declare gets a prefix of its own here.

// DeploymentID identifies a Kubernetes Deployment by name.
func DeploymentID(name string) ID { return ID("deployment:" + name) }

// K8sServiceID identifies a Kubernetes Service by name. Not "service:", which
// is the code map's service (and so a role).
func K8sServiceID(name string) ID { return ID("k8sservice:" + name) }

// DatabaseID identifies a database the deployment connects its nodes to.
func DatabaseID(name string) ID { return ID("database:" + name) }

// GRPCServiceID identifies a protobuf service by its full name
// (memql.v1.MemqlService).
func GRPCServiceID(fullName string) ID { return ID("grpc:" + fullName) }

// RPCID identifies one method of a protobuf service.
func RPCID(serviceFullName, method string) ID {
	return ID("rpc:" + serviceFullName + "/" + method)
}

// HostID identifies a front-door host. The domain is a placeholder
// ("api.<domain>"): the graph describes the shape, not an installation.
func HostID(host string) ID { return ID("host:" + host) }

// PathID identifies one routed path under a front-door host.
func PathID(host, path string) ID { return ID("path:" + host + path) }

// RoutingRuleID identifies one mesh event-routing rule by its action (block
// or forward) and topic pattern.
func RoutingRuleID(action, pattern string) ID {
	return ID("routing:" + action + ":" + pattern)
}

// NamespaceID identifies a DSL namespace.
func NamespaceID(namespace string) ID { return ID("namespace:" + namespace) }

// ConceptID identifies a DSL concept by its canonical id (v1:cluster:node).
func ConceptID(canonical string) ID { return ID("concept:" + canonical) }

// OSAppID identifies a MemQL OS app from its navigation contract.
func OSAppID(app string) ID { return ID("osapp:" + app) }

// OSSectionID identifies one section of a MemQL OS app.
func OSSectionID(app, section string) ID { return ID("ossection:" + app + "/" + section) }
