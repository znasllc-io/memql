//go:build !darwin && !linux

package models

// platformGPUs / platformGPUBackend: every other GOOS is not a worker
// platform for this cockpit, so nothing is probed and nothing is
// claimed.
func platformGPUs() ([]GPU, error) { return nil, nil }

func platformGPUBackend() string { return BackendNone }
