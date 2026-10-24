package models

import (
	"fmt"
	"runtime"
)

// The hardware floor (spec D10, published in the engine's
// docs/public/operate/local-models.md).
//
// A machine below the floor is NOT OFFERED as an inference machine. It
// remains a full worker for everything else -- shell, filesystem, HTTP,
// computer use, local apps -- and nothing about it is degraded.
//
// THE CHECK RUNS ON THE MACHINE, and that placement is the whole point:
// only this machine can see its own GPU. A central check would be
// guessing from a hostname.
const (
	// FloorMacMemoryBytes is the unified-memory minimum on macOS.
	FloorMacMemoryBytes uint64 = 16 * 1024 * 1024 * 1024
	// FloorMacOSMajor is the oldest macOS major version supported.
	FloorMacOSMajor = 13
	// FloorLinuxVRAMBytes is the discrete-GPU VRAM minimum on Linux.
	FloorLinuxVRAMBytes uint64 = 8 * 1024 * 1024 * 1024
)

// FloorVerdict is the answer, and when the answer is no, the sentence
// that says which of five plausible reasons it actually is.
//
// "My laptop is not in the model list" has five causes -- below the
// floor, no runtime installed, not in models.allow, not signed in, simply
// asleep -- and from the portal they are indistinguishable, because all
// five render as an absence. Reason is what makes this one legible.
type FloorVerdict struct {
	Met bool
	// Reason is empty when Met. Otherwise it is a complete sentence
	// naming the miss, written for an operator rather than a log parser.
	Reason string
	// Detail is what was actually observed, for the diagnostic. It may
	// be empty when nothing could be observed at all.
	Detail string
}

// Floor evaluates this machine against the floor.
func Floor() FloorVerdict { return platformFloor() }

// -----------------------------------------------------------------------------
// The evaluations. These carry no build tag on purpose: the platform files
// supply only the probes, so the RULES are exercised on every CI runner
// rather than on whichever one happens to match.
// -----------------------------------------------------------------------------

// darwinProbe is what the macOS evaluation needs to see. Each returns an
// error when it cannot establish the fact, which is not the same as
// establishing a bad one -- and both fail closed, for the reason the
// package doc gives.
type darwinProbe struct {
	AppleSilicon func() (bool, error)
	MemoryBytes  func() (uint64, error)
	OSMajor      func() (int, error)
}

func evalDarwinFloor(p darwinProbe) FloorVerdict {
	silicon, err := p.AppleSilicon()
	if err != nil {
		return FloorVerdict{Reason: "this machine's CPU architecture could not be established, so it is not offered for inference."}
	}
	if !silicon {
		return FloorVerdict{
			Reason: "an Intel Mac is not supported as an inference machine. It remains a full worker for everything else.",
			Detail: "intel",
		}
	}

	mem, memErr := p.MemoryBytes()
	if memErr != nil {
		return FloorVerdict{Reason: "this machine's installed memory could not be established, so it is not offered for inference.", Detail: "apple silicon"}
	}
	if mem < FloorMacMemoryBytes {
		return FloorVerdict{
			Reason: fmt.Sprintf("this machine has %s of unified memory; the floor is %s.", humanBytes(mem), humanBytes(FloorMacMemoryBytes)),
			Detail: fmt.Sprintf("apple silicon, %s", humanBytes(mem)),
		}
	}

	major, osErr := p.OSMajor()
	if osErr != nil {
		return FloorVerdict{Reason: "this machine's macOS version could not be established, so it is not offered for inference.", Detail: fmt.Sprintf("apple silicon, %s", humanBytes(mem))}
	}
	if major < FloorMacOSMajor {
		return FloorVerdict{
			Reason: fmt.Sprintf("this machine runs macOS %d; the floor is macOS %d.", major, FloorMacOSMajor),
			Detail: fmt.Sprintf("apple silicon, %s, macOS %d", humanBytes(mem), major),
		}
	}
	return FloorVerdict{Met: true, Detail: fmt.Sprintf("apple silicon, %s, macOS %d", humanBytes(mem), major)}
}

// GPU is one graphics device the Linux probe found.
type GPU struct {
	Name string
	// VRAMBytes is dedicated video memory. Zero means the probe saw the
	// device but not its memory, which does not meet the floor.
	VRAMBytes uint64
}

// linuxProbe is what the Linux evaluation needs to see.
type linuxProbe struct {
	Arch func() string
	GPUs func() ([]GPU, error)
}

func evalLinuxFloor(p linuxProbe) FloorVerdict {
	if arch := p.Arch(); arch != "amd64" {
		return FloorVerdict{
			Reason: fmt.Sprintf("Linux inference machines must be x86_64; this one is %s.", arch),
			Detail: arch,
		}
	}
	gpus, err := p.GPUs()
	if err != nil {
		return FloorVerdict{Reason: "no GPU could be detected on this machine, so it is not offered for inference. CPU-only inference is not supported."}
	}
	if len(gpus) == 0 {
		return FloorVerdict{Reason: "this machine has no discrete GPU. CPU-only inference is not supported; it remains a full worker for everything else."}
	}
	best := gpus[0]
	for _, g := range gpus {
		if g.VRAMBytes > best.VRAMBytes {
			best = g
		}
	}
	if best.VRAMBytes < FloorLinuxVRAMBytes {
		if best.VRAMBytes == 0 {
			return FloorVerdict{
				Reason: fmt.Sprintf("this machine's GPU (%s) reported no VRAM figure, so the floor of %s could not be confirmed.", best.Name, humanBytes(FloorLinuxVRAMBytes)),
				Detail: best.Name,
			}
		}
		return FloorVerdict{
			Reason: fmt.Sprintf("this machine's largest GPU (%s) has %s of VRAM; the floor is %s.", best.Name, humanBytes(best.VRAMBytes), humanBytes(FloorLinuxVRAMBytes)),
			Detail: fmt.Sprintf("%s, %s", best.Name, humanBytes(best.VRAMBytes)),
		}
	}
	return FloorVerdict{Met: true, Detail: fmt.Sprintf("%s, %s VRAM", best.Name, humanBytes(best.VRAMBytes))}
}

// evalUnsupportedFloor is every other GOOS. Windows and the BSDs are not
// worker platforms for this cockpit at all, so there is no floor to
// evaluate rather than a floor they fail.
func evalUnsupportedFloor(goos string) FloorVerdict {
	return FloorVerdict{
		Reason: fmt.Sprintf("%s is not a supported inference platform; local models run on macOS (Apple Silicon) and Linux (discrete GPU).", goos),
		Detail: goos,
	}
}

// humanBytes renders a byte count the way an operator would say it. It
// rounds to whole GB, because every threshold in this file is stated in
// whole GB and "15.9 GB" against a 16 GB floor reads as a rounding bug
// rather than a genuine miss.
func humanBytes(b uint64) string {
	const gb = 1024 * 1024 * 1024
	if b >= gb {
		return fmt.Sprintf("%d GB", b/gb)
	}
	const mb = 1024 * 1024
	return fmt.Sprintf("%d MB", b/mb)
}

// goarch is a seam only so the Linux evaluation can be driven from a test
// on any runner.
func goarch() string { return runtime.GOARCH }
