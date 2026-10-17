package inference

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// The Linux facts: what running Ollama as this user needs and where it
// goes (the default, 2026-09-08 record D1), plus Docker and its GPU
// passthrough for --runtime docker, and free disk on each one's volume.
func gatherPlatform(ctx context.Context) platformFacts {
	docker := linuxDockerFacts(ctx)
	native := linuxNativeFacts()
	return platformFacts{
		Docker:         docker,
		Native:         native,
		FreeDisk:       availableBytes(native.ModelsDir),
		DockerFreeDisk: availableBytes(dockerModelsDir(ctx, docker)),
	}
}

// linuxNativeFacts is what running Ollama as this user needs: the
// vendor, whether this user can open its device nodes, whether an earlier
// run already unpacked the runtime, and the layout under this home.
//
// The device check is the native path's whole GPU question. There is no
// toolkit to install and no daemon to configure: the same nodes
// nvidia-smi opened to pass the hardware floor are the ones Ollama's
// bundled CUDA opens, and for ROCm the render node is the passthrough
// exactly as it is for the container. A user who cannot open them gets a
// runtime that quietly serves from the CPU, so the plan refuses first.
func linuxNativeFacts() NativeFacts {
	home, _ := os.UserHomeDir()
	n := DefaultNativeFacts(home, os.Getenv("OLLAMA_MODELS"))
	n.Installed = exists(filepath.Join(n.RuntimeDir, "bin", "ollama"))
	n.FreeDisk = availableBytes(n.RuntimeDir)
	switch {
	case linuxHasNVIDIA():
		n.Vendor = GPUVendorNVIDIA
		switch {
		case !exists("/dev/nvidiactl"):
			n.DevicesReason = refusalNVIDIADriverNotLoaded
		case !openable("/dev/nvidiactl"):
			n.DevicesReason = refusalNVIDIADevicesNotAccessible
		default:
			n.Devices = true
		}
	case linuxHasAMDGPU():
		n.Vendor = GPUVendorAMD
		if openable("/dev/kfd") && anyOpenable("/dev/dri/renderD*") {
			n.Devices = true
		} else {
			n.DevicesReason = refusalAMDDevicesNotAccessible
		}
	default:
		n.DevicesReason = refusalNativeGPUUnknown
	}
	return n
}

// openable reports that this user may open the node read-write, which is
// what a driver needs of it. A stat says the node exists; only an open
// says the render group was granted.
func openable(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	return f.Close() == nil
}

func anyOpenable(pattern string) bool {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return false
	}
	for _, m := range matches {
		if openable(m) {
			return true
		}
	}
	return false
}

// linuxDockerFacts separates "the command is missing" from "the daemon
// did not answer", because they are different problems and each has its
// own sentence. A machine whose user is simply not in the docker group
// reads as the second, and telling that person to install Docker sends
// them somewhere they have already been.
func linuxDockerFacts(ctx context.Context) DockerFacts {
	if _, err := exec.LookPath("docker"); err != nil {
		return DockerFacts{Reason: "docker is not on PATH"}
	}
	facts := DockerFacts{CLIPresent: true}

	// `docker version --format {{.Server.Version}}` rather than
	// `docker info`: it is the cheapest call that actually contacts the
	// daemon, so a CLI installed in front of a dead socket fails here
	// rather than three steps later inside `docker run`.
	version, err := runProbe(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	if err != nil {
		facts.Reason = firstLine(err.Error())
		return facts
	}
	facts.Present = true
	facts.Version = version
	facts.GPUVendor, facts.GPUToolkit = linuxGPUPassthrough()
	return facts
}

// linuxGPUPassthrough names the vendor whose passthrough applies and says
// whether it is actually usable.
//
// The vendor probes MIRROR the floor's, deliberately: floor_linux.go
// finds a GPU through nvidia-smi or the amdgpu driver's sysfs node, so a
// machine that met the floor was seen by one of these two. Naming a
// vendor the floor did not see would produce a refusal about hardware
// this machine does not have.
//
// NVIDIA is checked first. A machine with both cards is rare and the
// NVIDIA path is the one the container toolkit exists for; the AMD route
// on such a machine still works, it is just not what this offers.
func linuxGPUPassthrough() (GPUVendor, bool) {
	if linuxHasNVIDIA() {
		return GPUVendorNVIDIA, linuxHasNVIDIAToolkit()
	}
	if linuxHasAMDGPU() {
		// The device nodes ARE the passthrough for ROCm -- there is no
		// container toolkit to install, so this asks for the nodes
		// themselves rather than for a package that does not exist.
		return GPUVendorAMD, exists("/dev/kfd") && exists("/dev/dri")
	}
	return GPUVendorUnknown, false
}

func linuxHasNVIDIA() bool {
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		return true
	}
	// The driver's control node, for a machine that has the kernel module
	// without the CUDA utilities package.
	return exists("/dev/nvidiactl")
}

// linuxHasNVIDIAToolkit asks for the toolkit by its BINARIES rather than
// by querying the package manager, so the answer is the same on Debian,
// RHEL and anything installed from the tarball.
//
// Three names because the package has shipped different ones over its
// life and all three are installed by the current one (confirmed on
// Ubuntu 24.04 on 2026-09-07: nvidia-container-toolkit 1.19.1-1 provides
// nvidia-ctk, nvidia-container-runtime and nvidia-container-cli). The
// package is NOT nvidia-docker2 any more; that name was retired and a
// refusal naming it would send somebody to an apt package that no longer
// resolves.
func linuxHasNVIDIAToolkit() bool {
	for _, bin := range []string{"nvidia-ctk", "nvidia-container-runtime-hook", "nvidia-container-cli"} {
		if _, err := exec.LookPath(bin); err == nil {
			return true
		}
	}
	return false
}

// linuxHasAMDGPU reads the same sysfs node floor_linux.go reads.
// mem_info_vram_total exists only for discrete cards, so an integrated
// Radeon does not answer here -- which is correct, since it would not
// have met the floor either.
func linuxHasAMDGPU() bool {
	cards, err := filepath.Glob("/sys/class/drm/card[0-9]*/device/mem_info_vram_total")
	return err == nil && len(cards) > 0
}

// dockerModelsDir is the volume the `ollama` docker volume lives on.
//
// The models are gigabytes each and they land under Docker's data root,
// not under the home directory -- a machine with a small root filesystem
// and a large /home has plenty of the wrong free space, and reporting
// that number would let a pull start that cannot finish.
func dockerModelsDir(ctx context.Context, docker DockerFacts) string {
	if docker.Present {
		if root, err := runProbe(ctx, "docker", "info", "--format", "{{.DockerRootDir}}"); err == nil && root != "" {
			return root
		}
	}
	return "/var/lib/docker"
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// availableBytes is what a non-root process may still write on that
// volume -- Bavail, not Bfree, which counts the reserved blocks this
// process cannot have. It is duplicated in plan_darwin.go rather than
// shared: unix.Statfs_t is a different struct on each platform, so there
// is no signature that compiles on both without a fourth file.
//
// Zero means "not established". Nothing refuses on it; the pull path
// reports it beside the model's own size (design section 5).
func availableBytes(path string) uint64 {
	dir := nearestExistingDir(path)
	if dir == "" {
		return 0
	}
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0
	}
	return st.Bavail * uint64(st.Bsize)
}
