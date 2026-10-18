// Package inference decides what a machine needs in order to serve
// models, and says it in one sentence.
//
// It sits in front of everything else in `memql worker setup
// --inference`: the floor verdict, the runtime, the install commands and
// the refusal all come from here, and the person running the command sees
// exactly one of them. THE SENTENCES ARE THE PRODUCT. A refusal is read
// by somebody in a terminal at the moment they are blocked, and it is the
// only thing they get -- so each one names the missing thing exactly and
// names the command that fixes it, and the tests assert every one of them
// verbatim. Rewording a sentence is meant to be a decision somebody makes
// on purpose, in a diff a reviewer can read.
//
// Two rules run through the package:
//
//   - DECIDE IS A PURE FUNCTION OF Host, and that is the whole design.
//     Every platform question -- is Docker there, does it have GPU
//     passthrough, how much disk is free -- is answered by the platform
//     files into Host, so the decision itself is table-testable on
//     fixtures with no build tags and no real machine. A runtime.GOOS
//     inside Decide would put the Linux rules beyond the reach of a mac
//     runner and the mac rules beyond the reach of CI, which is where a
//     refusal that names the wrong package survives to production.
//
//   - A PLAN ALWAYS SAYS SOMETHING. There is no path on which Decide
//     returns no refusal, no runtime and no command: that combination is
//     a blank terminal in front of a blocked person, and it is the exact
//     failure this package exists to prevent. A property test walks the
//     whole cross-product to hold it.
//
// The decision NEVER installs anything and never shells out; it reports
// the commands it would run so `setup --inference` can print them and ask
// (design D2 -- no sudo, no `curl | sh`, and nothing installed that
// nobody approved).
package inference

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/hardware"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

// probeTimeout bounds one shelled-out fact. Gather runs in front of a
// person waiting at a prompt, and a wedged Docker daemon must not turn
// `setup --inference` into a command that appears to hang -- the same
// reasoning, and the same five seconds, as the models package's probes.
const probeTimeout = 5 * time.Second

// Runtime is the model runtime a plan would install.
//
// It is what this plan is ABOUT, not necessarily what is serving: when
// RuntimePresent is true the machine already has something answering and
// Install is empty, and this field then only says which runtime the
// machine's platform would have got. Native Ollama is the default on both
// platforms -- through Homebrew on Apple Silicon, from the vendor's
// release archive under the person's home on Linux (2026-09-08 record,
// D1) -- and Docker is the Linux alternative behind --runtime docker. A
// refusal names none, because there is no runtime to install.
type Runtime string

const (
	// RuntimeNone is a plan that installs nothing, because it refuses.
	RuntimeNone Runtime = ""
	// RuntimeNative is Ollama on the host as a plain process: Homebrew's
	// service on macOS, a user systemd unit on Linux.
	RuntimeNative Runtime = "native"
	// RuntimeDocker is the ollama/ollama image with GPU passthrough,
	// which Linux offers under --runtime docker.
	RuntimeDocker Runtime = "docker"
)

// GPUVendor is whose passthrough a Linux container would use.
//
// This is NOT in the design record's DockerFacts and it has to be, for
// the reason the record itself gives about naming packages: the two
// vendors need different `docker run` flags AND different refusals, and
// there is no package name that is right for both. `nvidia-container-toolkit`
// printed to somebody with a Radeon names software that will not help
// them, which the record calls out as worse than naming none.
type GPUVendor string

const (
	// GPUVendorUnknown is a machine whose GPU neither probe recognised.
	// Reachable only by hand-assembling a Host -- a Linux machine that
	// met the floor did so through one of these two probes -- but Decide
	// is total, so it gets a sentence that names both fixes rather than
	// guessing one.
	GPUVendorUnknown GPUVendor = ""
	// GPUVendorNVIDIA passes the GPU through with --gpus=all, which needs
	// the NVIDIA container toolkit.
	GPUVendorNVIDIA GPUVendor = "nvidia"
	// GPUVendorAMD passes it through with --device /dev/kfd --device
	// /dev/dri, which needs no container toolkit at all -- the device
	// nodes ARE the passthrough. There is no "ROCm container toolkit"
	// package to name, and looking for a symmetry with NVIDIA here is how
	// a refusal ends up naming one that does not exist.
	GPUVendorAMD GPUVendor = "amd"
)

// DockerFacts is what the Linux platform file observed about Docker.
//
// Present and CLIPresent are separate because "docker is not installed"
// and "the daemon did not answer" are different problems with different
// fixes, and a machine where the user is simply not in the docker group
// is a common enough state that telling them to install Docker again
// would send them somewhere they have already been.
type DockerFacts struct {
	// CLIPresent reports that the docker command is on PATH, whether or
	// not anything is listening behind it.
	CLIPresent bool
	// Present reports that the daemon ANSWERED. It is what a plan needs:
	// a `docker run` against a daemon that is down installs nothing.
	Present bool
	// Version is the daemon's version string, for the diagnostic.
	Version string
	// GPUVendor is which vendor's passthrough applies on this machine.
	GPUVendor GPUVendor
	// GPUToolkit reports that the passthrough is actually usable -- the
	// NVIDIA container toolkit installed, or the ROCm device nodes
	// present. False with a known vendor is a refusal that names the
	// vendor's own fix.
	GPUToolkit bool
	// Reason is what the probe saw when Present is false, or why it did
	// not look. It is a DIAGNOSTIC and never a refusal sentence: the
	// refusals here are constant strings so they can be asserted
	// verbatim, and a caller that wants docker's own words prints this
	// beside one.
	Reason string
}

// Host is everything Decide is allowed to know. Assembled by Gather on a
// real machine, and by hand in the tests.
type Host struct {
	// GOOS and GOARCH are the machine's, not the build's. They are
	// fields rather than runtime constants so that every platform's rules
	// run on every CI runner.
	GOOS, GOARCH string
	// Floor is the hardware verdict from internal/worker/models. Decide
	// does not re-derive any part of it.
	Floor models.FloorVerdict
	// Ollama is the Probe result, which may be empty. It is probed even
	// below the floor, for the reason `memql worker models` gives: "you
	// have models and this machine cannot serve them" and "you have no
	// models" send a person to entirely different places.
	Ollama models.Inventory
	// OllamaServing reports that something ANSWERED the Ollama probe,
	// with or without models pulled. It earns its place beside Ollama for
	// the case the Inventory cannot express: an empty Models list means
	// "no Ollama" and "an Ollama with nothing pulled yet" equally, and
	// the second is exactly what a half-finished setup leaves behind,
	// where printing the docker run line again produces a container name
	// conflict rather than a pull.
	OllamaServing bool
	// Docker is the Linux runtime's facts. On macOS it is deliberately
	// unprobed: Docker is not a runtime option there, and probing it
	// would invite a plan that used it.
	Docker DockerFacts
	// Native is what running Ollama as this user would need on Linux,
	// and where it would go. Empty on macOS, where native means Homebrew.
	Native NativeFacts
	// Runtime is the runtime the PERSON asked for with --runtime, or ""
	// for the platform's own choice. Decide honours it where the platform
	// can serve it and refuses where it cannot; it never widens what the
	// platform serves.
	Runtime Runtime
	// DockerFreeDisk is bytes available where the ollama/ollama image
	// keeps its models, read only on Linux. FreeDisk is the native
	// runtime's figure; the command swaps this one in under
	// --runtime docker, because the two are different volumes.
	DockerFreeDisk uint64
	// Hardware is this machine's presence-facts inventory, from
	// internal/worker/hardware. Decide reads it for ONE thing -- the
	// machine class, which chooses the recommended set -- and it is a
	// field rather than a probe inside Decide for the reason every
	// other fact here is: the class table has to be exercisable on a
	// runner that is not the machine being classed.
	Hardware hardware.Inventory
	// FreeDisk is bytes available on the volume the runtime keeps models
	// on. Decide DOES NOT read it, and that is deliberate rather than an
	// oversight: refusing on disk needs the size of the models actually
	// requested (design section 5, "refused with both numbers"), which
	// only the pull path knows. Gathering it here means the pull does not
	// have to re-derive which volume that is.
	FreeDisk uint64
	// LookPath answers whether a binary is on PATH. A seam so a fixture
	// never depends on what the CI runner has installed; nil falls back
	// to exec.LookPath, which Gather always overrides.
	LookPath func(string) (string, error)
}

// RuntimePresentFor reports whether a MODEL runtime answered on this
// machine, by the same three signals Decide reads.
//
// Exported for the runtime-install command, which needs the fact
// without needing a Plan: `setup --runtime image` refuses when there is
// no model runtime, because image generation is a capability of one
// rather than a service of its own -- and re-deriving that from a
// second set of probes would be a second answer to a question Decide
// already settles.
func (h Host) RuntimePresentFor() bool {
	return h.OllamaServing || ollamaAnswered(h.Ollama) || h.lookPath("ollama")
}

func (h Host) lookPath(name string) bool {
	look := h.LookPath
	if look == nil {
		look = exec.LookPath
	}
	_, err := look(name)
	return err == nil
}

// Plan is the answer, and the sentence that goes with it.
type Plan struct {
	// Runtime is what would be installed. RuntimeNone on a refusal.
	Runtime Runtime
	// RuntimePresent reports that this machine already serves models, so
	// Install is empty. It is filled in BEFORE any refusal and survives
	// one, because "you have Ollama and this machine still cannot serve
	// from it" and "install this first" are different messages and the
	// caller needs to tell them apart.
	RuntimePresent bool
	// Install is the exact commands that would run, in order, empty when
	// the runtime is present or the plan refuses. They are printed and
	// consented to before anything runs (design D2).
	Install []string
	// Refusal is non-empty when this machine cannot be an inference
	// machine. It is the whole message: complete sentences, naming what
	// is missing and the command that fixes it.
	Refusal string
	// Note is what the person should know that is neither a command nor a
	// refusal. Today that is only the macOS answer to "why not Docker?",
	// which is asked exactly once -- when the plan is about to install
	// something -- and is noise on a machine that already works.
	Note string
	// DefaultModels is the set `setup --inference` pulls when no --model
	// is given. Populated on every plan including a refusal, so its
	// meaning does not depend on which branch produced the plan.
	//
	// It is a SET rather than a pair since the open-weight defaults
	// (engine memql#5137): what a machine should hold depends on how
	// much of it there is. See RecommendedSet.
	DefaultModels []string
	// MachineClass is the class DefaultModels was chosen for --
	// "16", "24", "32", "64", "128", or hardware.ClassUnsupported.
	//
	// It is on the plan rather than re-derived by the caller because
	// the preamble prints the class and the set on adjacent lines, and
	// two computations of the same class can disagree while a person is
	// looking at both answers at once.
	MachineClass string
	// Stage is the non-command half of a native Linux install -- the
	// archive, its checksum, the unit file -- run after the same consent
	// as Install and before its first command. Nil on every other plan.
	Stage *Stage
}

// The sentences. Constants rather than fmt calls at the point of use, so
// the whole vocabulary of this package is readable in one screen -- and
// so the style guard in the tests can walk them.
const (
	noteDockerNotOfferedOnMac = "Docker is not offered on macOS: a container has no access to the GPU, so Ollama in a container would serve on the CPU and this machine would not be advertised for inference."

	refusalNoHomebrew = "Homebrew is not installed, and it is the only macOS path this command can take. Install Homebrew from https://brew.sh and run this again, or install Ollama yourself from https://ollama.com/download."

	refusalNoDocker = "Docker is not installed, and this machine runs its model runtime in a container. Install Docker Engine from https://docs.docker.com/engine/install/ and run this again."

	refusalDockerDaemonSilent = "The docker command is installed and the Docker daemon did not answer. Start it with sudo systemctl start docker, or add your user to the docker group with sudo usermod -aG docker $USER and log back in, then run this again."

	refusalNoNVIDIAToolkit = "Docker is installed but cannot pass this machine's NVIDIA GPU into a container. Install the nvidia-container-toolkit package (see https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html), run sudo nvidia-ctk runtime configure --runtime=docker and sudo systemctl restart docker, then run this again."

	refusalNoROCmDevices = "Docker is installed but cannot pass this machine's AMD GPU into a container, because /dev/kfd is missing. Install the amdgpu-dkms driver (see https://rocm.docs.amd.com/projects/install-on-linux/en/latest/install/quick-start.html) and run this again."

	refusalNoGPUPassthrough = "Docker is installed and no GPU passthrough could be established for it. Install the nvidia-container-toolkit package for an NVIDIA GPU, or the amdgpu-dkms driver for an AMD GPU, then run this again."

	// noteDropDockerFlag rides every Docker refusal on Linux, because
	// every one of them is now reachable only under --runtime docker and
	// the way out is the platform's own default.
	noteDropDockerFlag = "Or drop --runtime docker: without it this command runs Ollama as your user, which needs none of that."

	// The native Linux sentences (2026-09-08 record, D1).
	noteNativeOnLinux = "Ollama will run as your user, from ~/.memql, kept running by a user systemd unit. Nothing here needs root and Docker is not involved; --runtime docker chooses the container instead."

	refusalNoSystemdUser = "The systemd user manager is not available on this machine (no systemctl on PATH), and it is what keeps Ollama running across logins and reboots. Install Ollama yourself from https://ollama.com/download and start it, or run this again with --runtime docker."

	refusalNoNativeArchive = "Ollama ships no Linux build for this processor, so there is nothing to unpack. Install Ollama yourself from https://ollama.com/download and start it, then run this again."

	refusalNoROCmOnArm = "Ollama ships its ROCm libraries for x86-64 only, so an AMD GPU on this processor cannot be served natively. Install Ollama yourself from https://ollama.com/download and start it, then run this again."

	refusalNVIDIADriverNotLoaded = "This machine has an NVIDIA GPU and its driver is not loaded: /dev/nvidiactl is missing. Install the NVIDIA driver for your distribution, reboot, and run this again."

	refusalNVIDIADevicesNotAccessible = "This machine has an NVIDIA GPU and your user cannot open /dev/nvidiactl. Check the device's permissions (ls -l /dev/nvidiactl), log back in, and run this again."

	refusalAMDDevicesNotAccessible = "This machine has an AMD GPU and your user cannot open /dev/kfd and a /dev/dri render node. Add yourself to the render and video groups with sudo usermod -aG render,video $USER, log back in, and run this again."

	refusalNativeGPUUnknown = "No GPU this command can drive was found for the native runtime: neither nvidia-smi nor the amdgpu driver answered. Install the vendor's driver and run this again."
)

// The native install's commands. Both pass through the same splitter as
// every other line, and both are safe to run twice.
const (
	installSystemdReload = "systemctl --user daemon-reload"
	installSystemdEnable = "systemctl --user enable --now " + OllamaUnitName
)

// refusalUnknownPlatform is the sentence for a Host that names no
// platform at all. Gather cannot produce one -- runtime.GOOS is never
// empty -- but the fallback exists because the alternative is a refusal
// that begins with a space and reads as a truncated message, and a person
// who gets that spends their time doubting the terminal rather than the
// machine. Its wording mirrors the floor's for a fact it could not
// establish, so the two packages sound like one.
const refusalUnknownPlatform = "This machine's platform could not be established, so it is not offered for inference."

func unsupportedPlatform(goos string) string {
	if strings.TrimSpace(goos) == "" {
		return refusalUnknownPlatform
	}
	return fmt.Sprintf("%s is not a supported platform for local models. They run on macOS with Apple Silicon and on Linux with a discrete GPU.", goos)
}

// The install commands, verbatim from the vendors, with two deliberate
// divergences noted where they are made.
//
// Ollama's own docs/docker.mdx and the ollama/ollama Docker Hub page
// (both read 2026-09-07) give:
//
//	docker run -d --gpus=all -v ollama:/root/.ollama -p 11434:11434 --name ollama ollama/ollama
//	docker run -d --device /dev/kfd --device /dev/dri -v ollama:/root/.ollama -p 11434:11434 --name ollama ollama/ollama:rocm
const (
	// installBrewOllama installs the CLI and the server. The Homebrew
	// FORMULA rather than the ollama-app cask (both at 0.33.3 on
	// 2026-09-07, neither deprecated): the cask is a desktop app that
	// needs a logged-in GUI session, and the machine this runs on is one
	// whose worker is a LaunchAgent.
	installBrewOllama = "brew install ollama"
	// installBrewOllamaStart is the second half, and it is not optional.
	// The Linux line starts the runtime (-d --restart unless-stopped);
	// without this one the macOS path installs a binary, exits happy, and
	// the very next step pulls against a socket nothing is listening on
	// -- an install that "succeeded" and a runtime that is not there.
	installBrewOllamaStart = "brew services start ollama"

	// installDockerNVIDIA and installDockerAMD diverge from the vendor
	// lines in exactly two ways, both on purpose:
	//
	//   -p 127.0.0.1:11434:11434 rather than -p 11434:11434. Ollama has
	//   no authentication of any kind, so the vendor's line publishes an
	//   open model server to the whole LAN. Docker publishes a port by
	//   writing its own DNAT and FORWARD rules, which a host firewall
	//   managed through ufw does not sit in front of -- so an operator who
	//   believes they are firewalled is not, and binding the loopback is
	//   what actually keeps it local. Nothing off-box needs the port: the
	//   cluster reaches this machine over the worker's outbound stream.
	//
	//   --restart unless-stopped. A runtime that does not come back after
	//   a reboot leaves a machine advertising models it cannot serve, and
	//   the calls fail on somebody else's prompt.
	installDockerNVIDIA = "docker run -d --name ollama --restart unless-stopped --gpus=all -v ollama:/root/.ollama -p 127.0.0.1:11434:11434 ollama/ollama"
	installDockerAMD    = "docker run -d --name ollama --restart unless-stopped --device /dev/kfd --device /dev/dri -v ollama:/root/.ollama -p 127.0.0.1:11434:11434 ollama/ollama:rocm"
)

// Decide answers what this machine needs. Pure: same Host, same Plan.
func Decide(h Host) Plan {
	// The class, and the set that follows from it, are settled BEFORE
	// any branch, for the same reason RuntimePresent is: a refusal must
	// not erase them. "This machine cannot serve models" and "and the
	// set it would have pulled is this" are both facts a person reading
	// a refusal wants, and a refusal that blanked the Models line would
	// read as a command that had not decided anything yet.
	class := hardware.Class(h.Hardware)
	p := Plan{MachineClass: class, DefaultModels: RecommendedSet(class)}

	// Presence is settled before any branch, so a refusal cannot erase
	// it. ANY of the three signals counts, and erring toward "present" is
	// the safe direction: the worst case is telling somebody to start
	// what they already have, where the other way round puts a second
	// runtime on port 11434 racing the first.
	//
	// The inventory is read as well as the flag, even though Gather sets
	// them together, because the two can disagree in a Host assembled by
	// hand -- and a plan that printed `docker run` at a machine whose own
	// inventory lists three pulled models would be refuted by the very
	// field it ignored.
	p.RuntimePresent = h.RuntimePresentFor()

	// The floor's own sentence, verbatim.
	//
	// The design record and the plan both say to use the floor's Detail
	// here; that is a slip in the record and this uses Reason. Detail is
	// what was OBSERVED ("intel", "apple silicon, 8 GB") and is empty on
	// the verdicts where a probe could establish nothing at all -- so a
	// refusal built from it is a blank terminal on exactly the paths
	// nobody can debug. Reason is the field the floor documents as "a
	// complete sentence naming the miss, written for an operator". It is
	// copied rather than rewritten because a second sentence for the same
	// fact drifts from the first, and then two places disagree about why
	// a machine is not in the model list.
	if !h.Floor.Met {
		p.Refusal = h.Floor.Reason
		if p.Refusal == "" {
			p.Refusal = unsupportedPlatform(h.GOOS)
		}
		return p
	}

	switch h.GOOS {
	case "darwin":
		return decideDarwin(h, p)
	case "linux":
		return decideLinux(h, p)
	default:
		p.Refusal = unsupportedPlatform(h.GOOS)
		return p
	}
}

// decideDarwin: native Ollama, never Docker (design D1).
func decideDarwin(h Host, p Plan) Plan {
	p.Runtime = RuntimeNative
	if p.RuntimePresent {
		return p
	}
	if !h.lookPath("brew") {
		p.Runtime = RuntimeNone
		p.Refusal = refusalNoHomebrew
		return p
	}
	p.Install = []string{installBrewOllama, installBrewOllamaStart}
	// The note rides the install and only the install. "Why not Docker?"
	// is a question somebody asks when they are being told to install
	// something else; on a machine that already serves it is noise in
	// front of an answer they did not ask for.
	p.Note = noteDockerNotOfferedOnMac
	return p
}

// decideLinux: Ollama as the person's own user by default (2026-09-08
// record, D1), the ollama/ollama container under --runtime docker
// (2026-09-06 record, D1).
//
// The runtime check comes FIRST on both paths: a machine already serving
// needs nothing installed, and refusing it for a missing Docker or a
// missing systemd would block a working inference machine over a runtime
// it is not using.
func decideLinux(h Host, p Plan) Plan {
	if h.Runtime == RuntimeDocker {
		return decideLinuxDocker(h, p)
	}
	return decideLinuxNative(h, p)
}

// decideLinuxNative: Ollama unpacked under the person's home and kept up
// by a user unit. Nothing here needs root, which is the whole reason it
// is the default: the container path below needs a root-installed
// toolkit and a Docker restart on every fresh machine, and this command
// runs no sudo.
func decideLinuxNative(h Host, p Plan) Plan {
	p.Runtime = RuntimeNative
	if p.RuntimePresent {
		return p
	}
	n := h.Native
	if !h.lookPath("systemctl") {
		p.Runtime = RuntimeNone
		p.Refusal = refusalNoSystemdUser
		return p
	}
	base := NativeArchiveName(h.GOARCH)
	if base == "" {
		p.Runtime = RuntimeNone
		p.Refusal = refusalNoNativeArchive
		return p
	}
	if !n.Devices {
		p.Runtime = RuntimeNone
		p.Refusal = n.DevicesReason
		if p.Refusal == "" {
			p.Refusal = refusalNativeGPUUnknown
		}
		return p
	}
	stage := &Stage{
		RuntimeDir: n.RuntimeDir,
		ModelsDir:  n.ModelsDir,
		LogPath:    n.LogPath,
		UnitPath:   n.UnitPath,
		Listen:     nativeListen,
		Parallel:   DefaultParallel(p.MachineClass),
	}
	switch {
	case n.Installed:
		stage.Reuse = true
	case n.Vendor == GPUVendorAMD:
		rocm := NativeROCmArchiveName(h.GOARCH)
		if rocm == "" {
			p.Runtime = RuntimeNone
			p.Refusal = refusalNoROCmOnArm
			return p
		}
		stage.Archives = []Archive{{Name: base}, {Name: rocm}}
	default:
		stage.Archives = []Archive{{Name: base}}
	}
	p.Stage = stage
	p.Install = []string{installSystemdReload, installSystemdEnable}
	p.Note = noteNativeOnLinux
	return p
}

// decideLinuxDocker: the ollama/ollama image with GPU passthrough, the
// original Linux path, now behind --runtime docker. Every refusal it can
// produce names the flag's way out, because each one is a root install
// the default path does not need.
func decideLinuxDocker(h Host, p Plan) Plan {
	p.Runtime = RuntimeDocker
	if p.RuntimePresent {
		return p
	}
	switch {
	case !h.Docker.CLIPresent:
		p.Runtime = RuntimeNone
		p.Refusal = refusalNoDocker + " " + noteDropDockerFlag
		return p
	case !h.Docker.Present:
		p.Runtime = RuntimeNone
		p.Refusal = refusalDockerDaemonSilent + " " + noteDropDockerFlag
		return p
	}
	if !h.Docker.GPUToolkit {
		p.Runtime = RuntimeNone
		switch h.Docker.GPUVendor {
		case GPUVendorNVIDIA:
			p.Refusal = refusalNoNVIDIAToolkit + " " + noteDropDockerFlag
		case GPUVendorAMD:
			p.Refusal = refusalNoROCmDevices + " " + noteDropDockerFlag
		default:
			p.Refusal = refusalNoGPUPassthrough + " " + noteDropDockerFlag
		}
		return p
	}
	if h.Docker.GPUVendor == GPUVendorAMD {
		p.Install = []string{installDockerAMD}
		return p
	}
	p.Install = []string{installDockerNVIDIA}
	return p
}

// Gather assembles a Host from this machine.
//
// It uses Probe rather than Discover, so a machine below the floor still
// reports the runtime it has -- Decide refuses on the floor either way,
// and the caller's message is better for knowing whether Ollama is
// already there.
func Gather(ctx context.Context, d *models.Discoverer) (Host, error) {
	if err := ctx.Err(); err != nil {
		return Host{}, err
	}

	// An empty Request: every model comes back Allowed=false, which is
	// fine because nothing here reads Allowed. Whether a model may be
	// SERVED is policy.yaml's answer, and this function is asking only
	// what exists.
	inv := d.Probe(ctx, models.Request{})

	h := Host{
		GOOS:          runtime.GOOS,
		GOARCH:        runtime.GOARCH,
		Floor:         inv.Floor,
		Ollama:        inv,
		OllamaServing: ollamaAnswered(inv),
		LookPath:      exec.LookPath,
	}
	facts := gatherPlatform(ctx)
	h.Docker = facts.Docker
	h.Native = facts.Native
	h.FreeDisk = facts.FreeDisk
	h.DockerFreeDisk = facts.DockerFreeDisk
	h.Hardware = hardware.Local(ctx)

	// Checked again on the way out: the probes above shell out and dial,
	// and a Host assembled from calls a cancelled context aborted would
	// report "no Docker, no Ollama" and get a refusal that names the
	// wrong thing entirely.
	if err := ctx.Err(); err != nil {
		return Host{}, err
	}
	return h, nil
}

// platformFacts is what the platform files answer. Everything that shells
// out lives behind them, so plan_test.go needs no build tags.
type platformFacts struct {
	Docker DockerFacts
	Native NativeFacts
	// FreeDisk is where the platform's DEFAULT runtime keeps models;
	// DockerFreeDisk is the container's volume, Linux only.
	FreeDisk       uint64
	DockerFreeDisk uint64
}

// ollamaRunningNoModels is the fragment of the models package's probe note
// that distinguishes an Ollama answering with an empty model list from one
// that is not there at all. Inventory has no other exported signal for it,
// and the alternative -- re-deriving OLLAMA_HOST's three documented
// spellings here -- would be a second resolver that can disagree with the
// first. TestOllamaServingWithNoModelsPulled drives the real Discoverer
// against a fake Ollama so a rewording fails there rather than in front of
// somebody halfway through a setup.
const ollamaRunningNoModels = "but has no models pulled"

func ollamaAnswered(inv models.Inventory) bool {
	for _, m := range inv.Models {
		if m.Kind == models.KindOllama {
			return true
		}
	}
	for _, note := range inv.ProbeNotes {
		if strings.Contains(note, ollamaRunningNoModels) {
			return true
		}
	}
	return false
}

// nearestExistingDir walks up to the first directory that exists.
//
// A statfs of a path that is not there fails, and the volume this asks
// about is usually one whose leaf has not been created yet -- ~/.ollama
// before the first pull, /var/lib/docker before Docker is installed. The
// answer for the parent is the answer for the child: they are the same
// filesystem the moment the leaf appears.
func nearestExistingDir(path string) string {
	for path != "" {
		if fi, err := os.Stat(path); err == nil && fi.IsDir() {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return ""
		}
		path = parent
	}
	return ""
}

// runProbe executes a short command for a fact. Absence and failure are
// both ordinary here -- a machine without Docker is the common case
// across a fleet, not a fault -- so the error is returned for the caller
// to turn into a Reason rather than logged.
func runProbe(ctx context.Context, name string, args ...string) (string, error) {
	bin, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", errors.New(firstLine(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// firstLine keeps a probe's error to one line. Docker's daemon errors run
// to three or four, and the extra ones are advice this package has
// already written better.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}
