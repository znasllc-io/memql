//go:build !cognition && !agent && !planner && !bff && !voice && !workbench

package node

// compiledNodeType is the node type this binary was compiled for.
// Default (no build tag) compiles as a BFF node.
var compiledNodeType = NodeTypeBFF
