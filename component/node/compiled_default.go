//go:build !agent && !planner && !bff && !workbench && !mcp

package node

// compiledNodeType is the node type this binary was compiled for.
// Default (no build tag) compiles as a BFF node.
var compiledNodeType = NodeTypeBFF
