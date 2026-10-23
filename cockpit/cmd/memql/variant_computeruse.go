//go:build computeruse

package main

// buildVariant for the CGO + RobotGo build: adds the workerComputer.*
// control surface (screenshot / mouse / keyboard / window). Same
// command name as headless -- see variant_headless.go.
const buildVariant = "computeruse"
