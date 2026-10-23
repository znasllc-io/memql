//go:build !darwin || !computeruse

package main

import "github.com/znasllc-io/memql-cockpit/internal/worker"

func runWorkerCommand(args []string) { worker.HandleCommand(args) }
