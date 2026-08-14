//go:build edge

package main

// The edge binary needs no init-time special casing. It is here so the build
// tag selects a main file the way every other node type does, and so a reader
// grepping for a node type finds all three of its files together.
