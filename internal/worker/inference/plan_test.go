package inference

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"unicode"

	"github.com/znasllc-io/memql-cockpit/internal/worker/hardware"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

// The table asserts the EXACT sentence on every path, and the literals
// live here rather than behind a shared constant on purpose: a shared
// constant would let a reworded refusal stay green, and these sentences
// are the whole product of the package. Changing one has to be a decision
// somebody makes on purpose, in a diff a reviewer can read.

const gib = 1024 * 1024 * 1024

// lookPath builds a Host.LookPath over a fixed set of binaries, so a
// fixture never depends on what the CI runner happens to have installed.
func lookPath(present ...string) func(string) (string, error) {
	set := make(map[string]bool, len(present))
	for _, p := range present {
		set[p] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return "/usr/bin/" + name, nil
		}
		return "", exec.ErrNotFound
	}
}

func metFloor(detail string) models.FloorVerdict {
	return models.FloorVerdict{Met: true, Detail: detail}
}

// nativeFacts is a Linux machine whose user can reach its GPU, laid out
// under /home/op.
func nativeFacts(vendor GPUVendor) NativeFacts {
	n := DefaultNativeFacts("/home/op", "")
	n.Vendor = vendor
	n.Devices = true
	return n
}

const (
	nativeNote     = "Ollama will run as your user, from ~/.memql, kept running by a user systemd unit. Nothing here needs root and Docker is not involved; --runtime docker chooses the container instead."
	dropDockerFlag = " Or drop --runtime docker: without it this command runs Ollama as your user, which needs none of that."
)

var nativeInstall = []string{"systemctl --user daemon-reload", "systemctl --user enable --now memql-ollama.service"}

// ollamaInventory is what Probe returns when Ollama answered with models.
func ollamaInventory() models.Inventory {
	return models.Inventory{
		Models: []models.Info{{ID: "llama3.1:8b", Kind: models.KindOllama, Runtime: "ollama"}},
	}
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name string
		host Host

		wantRuntime Runtime
		wantPresent bool
		wantInstall []string
		wantNote    string
		wantRefusal string
		// The native Linux stage: which archives it fetches, or that it
		// reuses an earlier unpack. Nil and false on every other plan.
		wantArchives []string
		wantReuse    bool
		// An installed runtime that is not answering (memql-cockpit#524):
		// the commands that start it, or the sentence when nothing here can.
		wantServing bool
		wantStart   []string
		wantStopped string
	}{
		{
			name: "apple silicon with ollama already installed",
			host: Host{
				GOOS: "darwin", GOARCH: "arm64",
				Floor:    metFloor("apple silicon, 32 GB, macOS 15"),
				Ollama:   ollamaInventory(),
				LookPath: lookPath("brew", "ollama"),
			},
			wantRuntime: RuntimeNative,
			wantPresent: true,
			wantServing: true,
		},
		{
			// THE HALF-SET-UP MAC (memql-cockpit#524): Homebrew installed
			// ollama and its service is stopped. Present used to mean
			// "nothing to do", and every pull then failed against a closed
			// port.
			name: "apple silicon with homebrew's ollama installed and stopped",
			host: Host{
				GOOS: "darwin", GOARCH: "arm64", UID: 501,
				Floor:    metFloor("apple silicon, 32 GB, macOS 15"),
				Native:   DarwinNativeFacts("/Users/op", ""),
				Stopped:  StoppedFacts{Brew: true, App: true},
				LookPath: lookPath("brew", "ollama"),
			},
			wantRuntime: RuntimeNative,
			wantPresent: true,
			wantStart:   []string{"brew services start ollama"},
			wantNote:    "Ollama is installed and is not answering, so it has to be started before a model can be pulled.",
		},
		{
			name: "apple silicon with ollama's own app installed and not running",
			host: Host{
				GOOS: "darwin", GOARCH: "arm64", UID: 501,
				Floor:    metFloor("apple silicon, 32 GB, macOS 15"),
				Native:   DarwinNativeFacts("/Users/op", ""),
				Stopped:  StoppedFacts{App: true},
				LookPath: lookPath("ollama"),
			},
			wantRuntime: RuntimeNative,
			wantPresent: true,
			wantStart:   []string{"open -a Ollama"},
			wantNote:    "Ollama is installed and is not answering, so it has to be started before a model can be pulled.",
		},
		{
			name: "apple silicon with memql's own agent loaded and the runtime down",
			host: Host{
				GOOS: "darwin", GOARCH: "arm64", UID: 501,
				Floor:    metFloor("apple silicon, 32 GB, macOS 15"),
				Native:   DarwinNativeFacts("/Users/op", ""),
				Stopped:  StoppedFacts{Agent: true, AgentLoaded: true, Brew: true},
				LookPath: lookPath("ollama"),
			},
			wantRuntime: RuntimeNative,
			wantPresent: true,
			wantStart:   []string{"launchctl kickstart -k gui/501/com.znasllc.memql-ollama"},
			wantNote:    "Ollama is installed and is not answering, so it has to be started before a model can be pulled.",
		},
		{
			name: "apple silicon with memql's own agent written and not loaded",
			host: Host{
				GOOS: "darwin", GOARCH: "arm64", UID: 501,
				Floor:    metFloor("apple silicon, 32 GB, macOS 15"),
				Native:   DarwinNativeFacts("/Users/op", ""),
				Stopped:  StoppedFacts{Agent: true},
				LookPath: lookPath("ollama"),
			},
			wantRuntime: RuntimeNative,
			wantPresent: true,
			wantStart:   []string{"launchctl bootstrap gui/501 /Users/op/Library/LaunchAgents/com.znasllc.memql-ollama.plist"},
			wantNote:    "Ollama is installed and is not answering, so it has to be started before a model can be pulled.",
		},
		{
			name: "apple silicon with an ollama nobody here can identify, not answering",
			host: Host{
				GOOS: "darwin", GOARCH: "arm64", UID: 501,
				Floor:    metFloor("apple silicon, 32 GB, macOS 15"),
				Native:   DarwinNativeFacts("/Users/op", ""),
				LookPath: lookPath("ollama"),
			},
			wantRuntime: RuntimeNative,
			wantPresent: true,
			wantStopped: "Ollama is installed and is not answering, and this command cannot tell how it was installed. Start it (open the Ollama app, or run ollama serve in a terminal) and run this again.",
		},
		{
			name: "apple silicon without ollama",
			host: Host{
				GOOS: "darwin", GOARCH: "arm64",
				Floor:    metFloor("apple silicon, 32 GB, macOS 15"),
				LookPath: lookPath("brew"),
			},
			wantRuntime: RuntimeNative,
			wantInstall: []string{"brew install ollama", "brew services start ollama"},
			wantNote:    "Docker is not offered on macOS: a container has no access to the GPU, so Ollama in a container would serve on the CPU and this machine would not be advertised for inference.",
		},
		{
			// No Homebrew is no longer a refusal (memql-cockpit#524):
			// Ollama's own release archive, checked against its checksum
			// file, run by a LaunchAgent as the person.
			name: "apple silicon without homebrew: ollama's own release",
			host: Host{
				GOOS: "darwin", GOARCH: "arm64", UID: 501,
				Floor:    metFloor("apple silicon, 16 GB, macOS 14"),
				Native:   DarwinNativeFacts("/Users/op", ""),
				LookPath: lookPath(),
			},
			wantRuntime:  RuntimeNative,
			wantInstall:  []string{"launchctl kickstart -k gui/501/com.znasllc.memql-ollama"},
			wantArchives: []string{"ollama-darwin.tgz"},
			wantNote:     "Homebrew is not installed, so Ollama's own macOS release will run as your user, from ~/.memql, kept running by a LaunchAgent. Nothing here needs an administrator password.",
		},
		{
			// An earlier native run unpacked the runtime and stopped
			// before it served. It is reused, even where Homebrew has
			// since appeared, rather than installing a second Ollama.
			name: "apple silicon with the native runtime already unpacked",
			host: Host{
				GOOS: "darwin", GOARCH: "arm64", UID: 501,
				Floor: metFloor("apple silicon, 16 GB, macOS 14"),
				Native: func() NativeFacts {
					n := DarwinNativeFacts("/Users/op", "")
					n.Installed = true
					return n
				}(),
				LookPath: lookPath("brew"),
			},
			wantRuntime: RuntimeNative,
			wantInstall: []string{"launchctl kickstart -k gui/501/com.znasllc.memql-ollama"},
			wantReuse:   true,
			wantNote:    "Homebrew is not installed, so Ollama's own macOS release will run as your user, from ~/.memql, kept running by a LaunchAgent. Nothing here needs an administrator password.",
		},
		{
			// The ollama binary is absent and something is nonetheless
			// answering on the port -- Ollama.app, or a tunnel. Installing
			// a second runtime would put two processes on port 11434.
			name: "apple silicon serving ollama from something not on PATH",
			host: Host{
				GOOS: "darwin", GOARCH: "arm64",
				Floor:    metFloor("apple silicon, 64 GB, macOS 15"),
				Ollama:   ollamaInventory(),
				LookPath: lookPath("brew"),
			},
			wantRuntime: RuntimeNative,
			wantPresent: true,
			wantServing: true,
		},
		{
			// THE LINUX DEFAULT (2026-09-08 record, D1): Ollama unpacked
			// under the person's home and kept up by a user unit. Docker is
			// not consulted at all -- the facts say it is absent and the
			// plan does not care.
			name: "linux with an nvidia gpu, no flag: ollama as the user",
			host: Host{
				GOOS: "linux", GOARCH: "amd64",
				Floor:    metFloor("NVIDIA GeForce RTX 4090, 24 GB VRAM"),
				Native:   nativeFacts(GPUVendorNVIDIA),
				Docker:   DockerFacts{Reason: "docker is not on PATH"},
				FreeDisk: 400 * gib,
				LookPath: lookPath("systemctl", "nvidia-smi"),
			},
			wantRuntime:  RuntimeNative,
			wantInstall:  nativeInstall,
			wantArchives: []string{"ollama-linux-amd64.tar.zst"},
			wantNote:     nativeNote,
		},
		{
			name: "linux with an amd gpu, no flag: the rocm add-on rides along",
			host: Host{
				GOOS: "linux", GOARCH: "amd64",
				Floor:    metFloor("AMD GPU 0x744c, 24 GB VRAM"),
				Native:   nativeFacts(GPUVendorAMD),
				LookPath: lookPath("systemctl"),
			},
			wantRuntime:  RuntimeNative,
			wantInstall:  nativeInstall,
			wantArchives: []string{"ollama-linux-amd64.tar.zst", "ollama-linux-amd64-rocm.tar.zst"},
			wantNote:     nativeNote,
		},
		{
			name: "linux on arm64 with an nvidia gpu",
			host: Host{
				GOOS: "linux", GOARCH: "arm64",
				Floor:    metFloor("NVIDIA GH200, 96 GB VRAM"),
				Native:   nativeFacts(GPUVendorNVIDIA),
				LookPath: lookPath("systemctl", "nvidia-smi"),
			},
			wantRuntime:  RuntimeNative,
			wantInstall:  nativeInstall,
			wantArchives: []string{"ollama-linux-arm64.tar.zst"},
			wantNote:     nativeNote,
		},
		{
			name: "linux on arm64 with an amd gpu",
			host: Host{
				GOOS: "linux", GOARCH: "arm64",
				Floor:    metFloor("AMD GPU 0x744c, 24 GB VRAM"),
				Native:   nativeFacts(GPUVendorAMD),
				LookPath: lookPath("systemctl"),
			},
			wantRefusal: "Ollama ships its ROCm libraries for x86-64 only, so an AMD GPU on this processor cannot be served natively. Install Ollama yourself from https://ollama.com/download and start it, then run this again.",
		},
		{
			name: "linux on a processor ollama does not ship for",
			host: Host{
				GOOS: "linux", GOARCH: "riscv64",
				Floor:    metFloor("NVIDIA something, 16 GB VRAM"),
				Native:   nativeFacts(GPUVendorNVIDIA),
				LookPath: lookPath("systemctl", "nvidia-smi"),
			},
			wantRefusal: "Ollama ships no Linux build for this processor, so there is nothing to unpack. Install Ollama yourself from https://ollama.com/download and start it, then run this again.",
		},
		{
			name: "linux without a systemd user manager",
			host: Host{
				GOOS: "linux", GOARCH: "amd64",
				Floor:    metFloor("NVIDIA GeForce RTX 4090, 24 GB VRAM"),
				Native:   nativeFacts(GPUVendorNVIDIA),
				LookPath: lookPath("nvidia-smi"),
			},
			wantRefusal: "The systemd user manager is not available on this machine (no systemctl on PATH), and it is what keeps Ollama running across logins and reboots. Install Ollama yourself from https://ollama.com/download and start it, or run this again with --runtime docker.",
		},
		{
			// The platform file's own sentence, verbatim: it saw the miss.
			name: "linux whose user cannot open the gpu's device nodes",
			host: Host{
				GOOS: "linux", GOARCH: "amd64",
				Floor: metFloor("AMD GPU 0x744c, 24 GB VRAM"),
				Native: NativeFacts{
					Vendor:        GPUVendorAMD,
					DevicesReason: "This machine has an AMD GPU and your user cannot open /dev/kfd and a /dev/dri render node. Add yourself to the render and video groups with sudo usermod -aG render,video $USER, log back in, and run this again.",
				},
				LookPath: lookPath("systemctl"),
			},
			wantRefusal: "This machine has an AMD GPU and your user cannot open /dev/kfd and a /dev/dri render node. Add yourself to the render and video groups with sudo usermod -aG render,video $USER, log back in, and run this again.",
		},
		{
			// Reachable only by hand: a Linux machine that met the floor was
			// seen by one of the two probes. Decide is total anyway.
			name: "linux native with no gpu the probes recognised",
			host: Host{
				GOOS: "linux", GOARCH: "amd64",
				Floor:    metFloor("some GPU, 16 GB VRAM"),
				Native:   DefaultNativeFacts("/home/op", ""),
				LookPath: lookPath("systemctl"),
			},
			wantRefusal: "No GPU this command can drive was found for the native runtime: neither nvidia-smi nor the amdgpu driver answered. Install the vendor's driver and run this again.",
		},
		{
			// An earlier run unpacked the runtime and stopped before the
			// unit was enabled. Nothing is fetched twice; the unit is
			// written and the commands run.
			name: "linux with the runtime unpacked by an interrupted earlier run",
			host: Host{
				GOOS: "linux", GOARCH: "amd64",
				Floor: metFloor("NVIDIA GeForce RTX 4090, 24 GB VRAM"),
				Native: func() NativeFacts {
					n := nativeFacts(GPUVendorNVIDIA)
					n.Installed = true
					return n
				}(),
				LookPath: lookPath("systemctl", "nvidia-smi"),
			},
			wantRuntime: RuntimeNative,
			wantInstall: nativeInstall,
			wantReuse:   true,
			wantNote:    nativeNote,
		},
		{
			// --runtime docker: the original Linux path, unchanged.
			name: "linux asked for docker, with the nvidia container toolkit",
			host: Host{
				GOOS: "linux", GOARCH: "amd64",
				Runtime: RuntimeDocker,
				Floor:   metFloor("NVIDIA GeForce RTX 4090, 24 GB VRAM"),
				Docker: DockerFacts{
					CLIPresent: true, Present: true, Version: "29.6.1",
					GPUVendor: GPUVendorNVIDIA, GPUToolkit: true,
				},
				FreeDisk: 400 * gib,
				LookPath: lookPath("docker", "nvidia-smi", "nvidia-ctk"),
			},
			wantRuntime: RuntimeDocker,
			wantInstall: []string{"docker run -d --name ollama --restart unless-stopped --gpus=all -v ollama:/root/.ollama -p 127.0.0.1:11434:11434 ollama/ollama"},
		},
		{
			name: "linux asked for docker, with the rocm device nodes",
			host: Host{
				GOOS: "linux", GOARCH: "amd64",
				Runtime: RuntimeDocker,
				Floor:   metFloor("AMD GPU 0x744c, 24 GB VRAM"),
				Docker: DockerFacts{
					CLIPresent: true, Present: true, Version: "29.6.1",
					GPUVendor: GPUVendorAMD, GPUToolkit: true,
				},
				FreeDisk: 400 * gib,
				LookPath: lookPath("docker"),
			},
			wantRuntime: RuntimeDocker,
			wantInstall: []string{"docker run -d --name ollama --restart unless-stopped --device /dev/kfd --device /dev/dri -v ollama:/root/.ollama -p 127.0.0.1:11434:11434 ollama/ollama:rocm"},
		},
		{
			// Every Docker refusal names the way out, because each is a
			// root install the default path does not need.
			name: "linux asked for docker, without the nvidia container toolkit",
			host: Host{
				GOOS: "linux", GOARCH: "amd64",
				Runtime: RuntimeDocker,
				Floor:   metFloor("NVIDIA GeForce RTX 4090, 24 GB VRAM"),
				Docker: DockerFacts{
					CLIPresent: true, Present: true, Version: "29.6.1",
					GPUVendor: GPUVendorNVIDIA, GPUToolkit: false,
				},
				LookPath: lookPath("docker", "nvidia-smi"),
			},
			wantRefusal: "Docker is installed but cannot pass this machine's NVIDIA GPU into a container. Install the nvidia-container-toolkit package (see https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html), run sudo nvidia-ctk runtime configure --runtime=docker and sudo systemctl restart docker, then run this again." + dropDockerFlag,
		},
		{
			name: "linux asked for docker, without the rocm device nodes",
			host: Host{
				GOOS: "linux", GOARCH: "amd64",
				Runtime: RuntimeDocker,
				Floor:   metFloor("AMD GPU 0x744c, 24 GB VRAM"),
				Docker: DockerFacts{
					CLIPresent: true, Present: true, Version: "29.6.1",
					GPUVendor: GPUVendorAMD, GPUToolkit: false,
				},
				LookPath: lookPath("docker"),
			},
			wantRefusal: "Docker is installed but cannot pass this machine's AMD GPU into a container, because /dev/kfd is missing. Install the amdgpu-dkms driver (see https://rocm.docs.amd.com/projects/install-on-linux/en/latest/install/quick-start.html) and run this again." + dropDockerFlag,
		},
		{
			name: "linux asked for docker, with a GPU neither probe recognised",
			host: Host{
				GOOS: "linux", GOARCH: "amd64",
				Runtime: RuntimeDocker,
				Floor:   metFloor("some GPU, 16 GB VRAM"),
				Docker: DockerFacts{
					CLIPresent: true, Present: true, Version: "29.6.1",
					GPUVendor: GPUVendorUnknown, GPUToolkit: false,
				},
				LookPath: lookPath("docker"),
			},
			wantRefusal: "Docker is installed and no GPU passthrough could be established for it. Install the nvidia-container-toolkit package for an NVIDIA GPU, or the amdgpu-dkms driver for an AMD GPU, then run this again." + dropDockerFlag,
		},
		{
			name: "linux asked for docker, without docker",
			host: Host{
				GOOS: "linux", GOARCH: "amd64",
				Runtime:  RuntimeDocker,
				Floor:    metFloor("NVIDIA GeForce RTX 4090, 24 GB VRAM"),
				Docker:   DockerFacts{Reason: "docker is not on PATH"},
				LookPath: lookPath("nvidia-smi"),
			},
			wantRefusal: "Docker is not installed, and this machine runs its model runtime in a container. Install Docker Engine from https://docs.docker.com/engine/install/ and run this again." + dropDockerFlag,
		},
		{
			name: "linux asked for docker, with the docker command and a daemon that did not answer",
			host: Host{
				GOOS: "linux", GOARCH: "amd64",
				Runtime: RuntimeDocker,
				Floor:   metFloor("NVIDIA GeForce RTX 4090, 24 GB VRAM"),
				Docker: DockerFacts{
					CLIPresent: true,
					Reason:     "permission denied while trying to connect to the Docker daemon socket",
				},
				LookPath: lookPath("docker", "nvidia-smi"),
			},
			wantRefusal: "The docker command is installed and the Docker daemon did not answer. Start it with sudo systemctl start docker, or add your user to the docker group with sudo usermod -aG docker $USER and log back in, then run this again." + dropDockerFlag,
		},
		{
			// The runtime check runs before anything else on both Linux
			// paths: a machine already serving needs nothing installed, so
			// a missing Docker, or a missing systemd, is not a reason to
			// refuse it.
			name: "linux already serving ollama without docker",
			host: Host{
				GOOS: "linux", GOARCH: "amd64",
				Floor:    metFloor("NVIDIA GeForce RTX 4090, 24 GB VRAM"),
				Ollama:   ollamaInventory(),
				Docker:   DockerFacts{Reason: "docker is not on PATH"},
				LookPath: lookPath(),
			},
			wantRuntime: RuntimeNative,
			wantPresent: true,
			wantServing: true,
		},
		{
			name: "linux asked for docker while already serving",
			host: Host{
				GOOS: "linux", GOARCH: "amd64",
				Runtime:  RuntimeDocker,
				Floor:    metFloor("NVIDIA GeForce RTX 4090, 24 GB VRAM"),
				Ollama:   ollamaInventory(),
				Docker:   DockerFacts{Reason: "docker is not on PATH"},
				LookPath: lookPath(),
			},
			wantRuntime: RuntimeDocker,
			wantPresent: true,
			wantServing: true,
		},
		{
			name: "below the macOS memory floor",
			host: Host{
				GOOS: "darwin", GOARCH: "arm64",
				Floor: models.FloorVerdict{
					Reason: "this machine has 8 GB of unified memory; the floor is 16 GB.",
					Detail: "apple silicon, 8 GB",
				},
				LookPath: lookPath("brew"),
			},
			wantRefusal: "this machine has 8 GB of unified memory; the floor is 16 GB.",
		},
		{
			name: "below the Linux VRAM floor",
			host: Host{
				GOOS: "linux", GOARCH: "amd64",
				Floor: models.FloorVerdict{
					Reason: "this machine has no discrete GPU. CPU-only inference is not supported; it remains a full worker for everything else.",
				},
				Docker:   DockerFacts{CLIPresent: true, Present: true, Version: "29.6.1"},
				LookPath: lookPath("docker"),
			},
			wantRefusal: "this machine has no discrete GPU. CPU-only inference is not supported; it remains a full worker for everything else.",
		},
		{
			// A below-floor machine that already has the runtime is a
			// different message from one that does not, so the presence
			// survives the refusal rather than being reset by it.
			name: "below the floor with ollama already installed",
			host: Host{
				GOOS: "darwin", GOARCH: "arm64",
				Floor: models.FloorVerdict{
					Reason: "an Intel Mac is not supported as an inference machine. It remains a full worker for everything else.",
					Detail: "intel",
				},
				Ollama:   ollamaInventory(),
				LookPath: lookPath("brew", "ollama"),
			},
			wantPresent: true,
			wantServing: true,
			wantRefusal: "an Intel Mac is not supported as an inference machine. It remains a full worker for everything else.",
		},
		{
			name: "windows, refused by the floor before the platform rule",
			host: Host{
				GOOS: "windows", GOARCH: "amd64",
				Floor: models.FloorVerdict{
					Reason: "windows is not a supported inference platform; local models run on macOS (Apple Silicon) and Linux (discrete GPU).",
					Detail: "windows",
				},
				LookPath: lookPath(),
			},
			wantRefusal: "windows is not a supported inference platform; local models run on macOS (Apple Silicon) and Linux (discrete GPU).",
		},
		{
			// Decide is total: a Host assembled by hand can name a
			// platform with a met floor, and it still gets a sentence
			// rather than a plan that installs nothing and says nothing.
			name: "an unsupported platform whose floor somehow passed",
			host: Host{
				GOOS: "freebsd", GOARCH: "amd64",
				Floor:    metFloor("some GPU"),
				LookPath: lookPath(),
			},
			wantRefusal: "freebsd is not a supported platform for local models. They run on macOS with Apple Silicon and on Linux with a discrete GPU.",
		},
		{
			name: "linux with memql's own unit written and the runtime down",
			host: Host{
				GOOS: "linux", GOARCH: "amd64",
				Floor:    metFloor("NVIDIA GeForce RTX 4090, 24 GB VRAM"),
				Native:   nativeFacts(GPUVendorNVIDIA),
				Stopped:  StoppedFacts{Unit: true},
				LookPath: lookPath("systemctl", "ollama"),
			},
			wantRuntime: RuntimeNative,
			wantPresent: true,
			wantStart:   []string{"systemctl --user start memql-ollama.service"},
			wantNote:    "Ollama is installed and is not answering, so it has to be started before a model can be pulled.",
		},
		{
			// Ollama's own install.sh puts a SYSTEM unit on the machine,
			// and this command never runs sudo: it names the command.
			name: "linux with ollama's system service stopped",
			host: Host{
				GOOS: "linux", GOARCH: "amd64",
				Floor:    metFloor("NVIDIA GeForce RTX 4090, 24 GB VRAM"),
				Native:   nativeFacts(GPUVendorNVIDIA),
				Stopped:  StoppedFacts{SystemUnit: true},
				LookPath: lookPath("systemctl", "ollama"),
			},
			wantRuntime: RuntimeNative,
			wantPresent: true,
			wantStopped: "Ollama is installed as a system service and is not running. Start it with sudo systemctl start ollama, then run this again.",
		},
		{
			name: "linux under --runtime docker with the ollama container stopped",
			host: Host{
				GOOS: "linux", GOARCH: "amd64",
				Runtime:  RuntimeDocker,
				Floor:    metFloor("NVIDIA GeForce RTX 4090, 24 GB VRAM"),
				Docker:   DockerFacts{CLIPresent: true, Present: true, GPUVendor: GPUVendorNVIDIA, GPUToolkit: true},
				Stopped:  StoppedFacts{Container: true},
				LookPath: lookPath("docker", "ollama"),
			},
			wantRuntime: RuntimeDocker,
			wantPresent: true,
			wantStart:   []string{"docker start ollama"},
			wantNote:    "Ollama is installed and is not answering, so it has to be started before a model can be pulled.",
		},
		{
			// A Host that names no platform. Gather cannot build one, and
			// the sentence exists so a hand-built Host never produces a
			// refusal that opens with a space and reads as truncated.
			name: "a host that names no platform at all",
			host: Host{
				Floor:    metFloor("fixture"),
				LookPath: lookPath(),
			},
			wantRefusal: "This machine's platform could not be established, so it is not offered for inference.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Decide(tt.host)
			if got.Refusal != tt.wantRefusal {
				t.Errorf("Refusal =\n  %q\nwant\n  %q", got.Refusal, tt.wantRefusal)
			}
			if got.Note != tt.wantNote {
				t.Errorf("Note =\n  %q\nwant\n  %q", got.Note, tt.wantNote)
			}
			if got.Runtime != tt.wantRuntime {
				t.Errorf("Runtime = %q, want %q", got.Runtime, tt.wantRuntime)
			}
			if got.RuntimePresent != tt.wantPresent {
				t.Errorf("RuntimePresent = %v, want %v", got.RuntimePresent, tt.wantPresent)
			}
			if len(got.Install) != len(tt.wantInstall) {
				t.Fatalf("Install = %q, want %q", got.Install, tt.wantInstall)
			}
			for i := range got.Install {
				if got.Install[i] != tt.wantInstall[i] {
					t.Errorf("Install[%d] =\n  %q\nwant\n  %q", i, got.Install[i], tt.wantInstall[i])
				}
			}
			var archives []string
			reuse := false
			if got.Stage != nil {
				reuse = got.Stage.Reuse
				for _, a := range got.Stage.Archives {
					archives = append(archives, a.Name)
				}
			}
			if strings.Join(archives, ",") != strings.Join(tt.wantArchives, ",") {
				t.Errorf("Stage archives = %q, want %q", archives, tt.wantArchives)
			}
			if reuse != tt.wantReuse {
				t.Errorf("Stage.Reuse = %v, want %v", reuse, tt.wantReuse)
			}
			if got.Stage != nil && got.Refusal != "" {
				t.Error("a refusal must not carry a stage")
			}
			if got.Serving != tt.wantServing {
				t.Errorf("Serving = %v, want %v", got.Serving, tt.wantServing)
			}
			if strings.Join(got.Start, "\n") != strings.Join(tt.wantStart, "\n") {
				t.Errorf("Start = %q, want %q", got.Start, tt.wantStart)
			}
			if got.Stopped != tt.wantStopped {
				t.Errorf("Stopped =\n  %q\nwant\n  %q", got.Stopped, tt.wantStopped)
			}
			// Every case in this table leaves Host.Hardware zero, so
			// every one classes `unsupported` and takes the smallest
			// set. The assertion is here rather than in
			// recommend_test.go because what it actually pins is that
			// NO BRANCH of Decide blanks the set -- including the
			// refusals, which is the half a table of happy cases would
			// never reach.
			if len(got.DefaultModels) != 2 ||
				got.DefaultModels[0] != "qwen3.5:4b" ||
				got.DefaultModels[1] != "qwen3-embedding:0.6b" {
				t.Errorf("DefaultModels = %q, want the smallest recommended set", got.DefaultModels)
			}
			if got.MachineClass != hardware.ClassUnsupported {
				t.Errorf("MachineClass = %q, want %q for a host with no hardware inventory",
					got.MachineClass, hardware.ClassUnsupported)
			}
		})
	}
}

// TestDecideAlwaysSaysSomething. The one property that must hold on every
// path: a plan that neither refuses, nor reports a runtime, nor prints a
// command leaves the person with a blank terminal at the exact moment
// they are blocked -- the failure this package exists to prevent, and the
// one a table of happy cases would never catch.
func TestDecideAlwaysSaysSomething(t *testing.T) {
	goos := []string{"darwin", "linux", "windows", "freebsd", ""}
	arches := []string{"amd64", "arm64", "riscv64"}
	vendors := []GPUVendor{GPUVendorUnknown, GPUVendorNVIDIA, GPUVendorAMD}
	paths := [][]string{{}, {"brew"}, {"docker"}, {"brew", "ollama"}, {"docker", "nvidia-smi"}, {"systemctl"}, {"systemctl", "nvidia-smi"}}
	prefs := []Runtime{RuntimeNone, RuntimeNative, RuntimeDocker}

	for _, goosName := range goos {
		for _, arch := range arches {
			for _, met := range []bool{true, false} {
				for _, vendor := range vendors {
					for _, toolkit := range []bool{true, false} {
						for _, devices := range []bool{true, false} {
							for _, cli := range []bool{true, false} {
								for _, daemon := range []bool{true, false} {
									for _, pref := range prefs {
										for _, p := range paths {
											native := DefaultNativeFacts("/home/op", "")
											native.Vendor = vendor
											native.Devices = devices
											h := Host{
												GOOS: goosName, GOARCH: arch,
												Runtime:  pref,
												Floor:    models.FloorVerdict{Met: met, Reason: "a floor sentence."},
												Docker:   DockerFacts{CLIPresent: cli, Present: cli && daemon, GPUVendor: vendor, GPUToolkit: toolkit},
												Native:   native,
												LookPath: lookPath(p...),
											}
											if met {
												h.Floor.Reason = ""
											}
											plan := Decide(h)
											if plan.Refusal == "" && !plan.RuntimePresent && len(plan.Install) == 0 {
												t.Fatalf("silent plan for %+v", h)
											}
											if plan.Stage != nil && plan.Refusal != "" {
												t.Fatalf("a refusal with a stage for %+v", h)
											}
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
}

// TestSentencesAreReadableInATerminal guards the house rules on every
// sentence this package can emit: they are read by a person in a terminal
// at the moment they are blocked, and it is the only thing they get.
func TestSentencesAreReadableInATerminal(t *testing.T) {
	sentences := []string{
		noteDockerNotOfferedOnMac,
		noteNativeOnMac,
		noteRuntimeStopped,
		stoppedUnknown,
		stoppedSystemUnit,
		refusalNoDocker,
		refusalDockerDaemonSilent,
		refusalNoNVIDIAToolkit,
		refusalNoROCmDevices,
		refusalNoGPUPassthrough,
		noteDropDockerFlag,
		noteNativeOnLinux,
		refusalNoSystemdUser,
		refusalNoNativeArchive,
		refusalNoROCmOnArm,
		refusalNVIDIADriverNotLoaded,
		refusalNVIDIADevicesNotAccessible,
		refusalAMDDevicesNotAccessible,
		refusalNativeGPUUnknown,
		unsupportedPlatform("freebsd"),
		unsupportedPlatform(""),
	}
	banned := []string{"unfortunately", "sorry", "please note", "oops", "failed to"}

	for _, s := range sentences {
		if s == "" {
			t.Error("an empty sentence is a blank terminal")
			continue
		}
		if strings.ContainsRune(s, '!') {
			t.Errorf("no exclamation: %q", s)
		}
		if strings.Contains(s, "  ") {
			t.Errorf("one space after a full stop: %q", s)
		}
		if !strings.HasSuffix(s, ".") {
			t.Errorf("a sentence ends in a full stop: %q", s)
		}
		for _, r := range s {
			if r > unicode.MaxASCII {
				t.Errorf("ASCII only, found %q in %q", r, s)
				break
			}
		}
		lower := strings.ToLower(s)
		for _, b := range banned {
			if strings.Contains(lower, b) {
				t.Errorf("%q reads as an apology or a log line: %q", b, s)
			}
		}
	}
}

// TestGatherReadsThisMachine is the only test that touches the platform
// files. It asserts the facts that hold on every runner rather than any
// that depend on what is installed, because the point is that Gather
// returns a Host at all -- a Gather that panicked on a machine without
// Docker would be found here and nowhere else.
func TestGatherReadsThisMachine(t *testing.T) {
	h, err := Gather(context.Background(), &models.Discoverer{
		OllamaBaseURL: "http://127.0.0.1:1", // nothing listens there
		FloorFn:       func() models.FloorVerdict { return models.FloorVerdict{Met: true, Detail: "fixture"} },
	})
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if h.GOOS == "" || h.GOARCH == "" {
		t.Errorf("Gather must name the platform, got %q/%q", h.GOOS, h.GOARCH)
	}
	if h.LookPath == nil {
		t.Error("Gather must supply LookPath, or Decide falls back to the process PATH silently")
	}
	if h.OllamaServing {
		t.Error("nothing listens on port 1; OllamaServing must be false")
	}
	if !h.Floor.Met {
		t.Errorf("Gather must carry the Discoverer's floor verdict through, got %+v", h.Floor)
	}
}

// TestGatherCancelledContext. Gather is the interactive half of `setup
// --inference` and it shells out; a person who pressed Ctrl-C gets the
// cancellation back rather than a Host assembled from probes that were
// never allowed to run.
func TestGatherCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Gather(ctx, &models.Discoverer{OllamaBaseURL: "http://127.0.0.1:1"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestOllamaServingWithNoModelsPulled pins the coupling this package has
// to the models package's probe notes, and it is the reason the test
// stands up a real HTTP server instead of a fixture Inventory.
//
// An Ollama that answers with an EMPTY model list is indistinguishable
// from an absent one in Inventory.Models, and it is the exact state a
// half-finished setup leaves behind. Reading it as "absent" would print
// the docker run line for a container that already exists, and the person
// would get a name conflict instead of a pull. The note is the only
// exported signal that tells the two apart, so this test drives the real
// Discoverer and fails here if its wording ever changes -- rather than on
// somebody's machine.
func TestOllamaServingWithNoModelsPulled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	inv := (&models.Discoverer{
		OllamaBaseURL: srv.URL,
		FloorFn:       func() models.FloorVerdict { return models.FloorVerdict{Met: true} },
	}).Probe(context.Background(), models.Request{})

	if len(inv.Models) != 0 {
		t.Fatalf("fixture served no models, got %+v", inv.Models)
	}
	if !ollamaAnswered(inv) {
		t.Fatalf("an Ollama with nothing pulled must read as serving; notes were %q", inv.ProbeNotes)
	}
}

// TestOllamaAnsweredIsFalseWhenNothingListens is the other half: a
// refused connection must not read as a runtime, or every machine
// without Ollama would be told it already has one.
func TestOllamaAnsweredIsFalseWhenNothingListens(t *testing.T) {
	inv := (&models.Discoverer{
		OllamaBaseURL: "http://127.0.0.1:1",
		FloorFn:       func() models.FloorVerdict { return models.FloorVerdict{Met: true} },
	}).Probe(context.Background(), models.Request{})

	if ollamaAnswered(inv) {
		t.Fatalf("nothing listens on port 1; notes were %q", inv.ProbeNotes)
	}
}
