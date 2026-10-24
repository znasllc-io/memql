//go:build linux

package models

// platformGPUs is the floor's own probe, unchanged.
func platformGPUs() ([]GPU, error) { return linuxGPUs() }

// platformGPUBackend reports which vendor's stack a call would go
// through. It re-runs the two probes rather than caching a verdict from
// linuxGPUs, because the two answer different questions: linuxGPUs
// merges both vendors into one list so the floor can pick the largest
// card, and the backend has to know WHICH probe found it.
//
// NVIDIA wins a tie. A machine with both is being driven by the card
// the CUDA stack can reach, and naming the AMD one would send an
// operator to install a driver for a device nothing is using.
func platformGPUBackend() string {
	if len(nvidiaGPUs()) > 0 {
		return BackendCUDA
	}
	if amd, err := amdGPUs(); err == nil && len(amd) > 0 {
		return BackendROCm
	}
	return BackendNone
}
