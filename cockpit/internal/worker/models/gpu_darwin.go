//go:build darwin

package models

// platformGPUs: Apple Silicon exposes no discrete device. Returning
// nothing is the honest answer to "which cards are installed", and the
// hardware package supplies the GPU entry from the chip name and the
// unified memory size instead -- the two facts that actually describe
// it.
func platformGPUs() ([]GPU, error) { return nil, nil }

// platformGPUBackend: Metal on Apple Silicon hardware, none on an Intel
// Mac. It asks the same sysctl the floor asks, so the two can never
// disagree about which kind of Mac this is.
func platformGPUBackend() string {
	silicon, err := darwinAppleSilicon()
	if err != nil || !silicon {
		return BackendNone
	}
	return BackendMetal
}
