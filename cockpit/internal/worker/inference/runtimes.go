package inference

import (
	"fmt"
	"sort"
	"strings"
)

// The speech and image runtimes (memql-cockpit#400, engine record D7).
//
// Same shape as Decide and for the same reasons: a PURE FUNCTION of
// facts already gathered, returning commands or a refusal, printing
// nothing and installing nothing. The caller prints, asks, and only then
// runs -- so every sentence here is asserted verbatim in a test and
// rewording one is a decision in a diff a reviewer can read.
//
// THE ENGINE NEVER INSTALLS SOFTWARE ON A PERSON'S MACHINE (record D7),
// which is why this exists on this side at all: the catalog can say "this
// profile needs the Kokoro runtime", and only the cockpit can offer to
// put it there.

// The runtime names this command installs. A CLOSED SET: `--runtime`
// already means docker|native alongside `--inference`, and admitting an
// open vocabulary here would make a typo look like a runtime nobody has
// heard of rather than a mistake.
const (
	// RuntimeKokoro is the speech runtime. A separate SERVICE rather
	// than a model in Ollama: Ollama serves no text-to-speech at all, so
	// a machine that speaks has a second thing running on it.
	RuntimeKokoro = "kokoro"
	// RuntimeImage is Ollama's own image generation. NOT a service to
	// install -- it is a capability of a runtime this machine may
	// already have, so the "install" is a model pull and the refusals
	// are about the runtime being absent rather than the software.
	RuntimeImage = "image"
)

// InstallableRuntimes lists them for the usage text and the refusal
// that has to name the alternatives.
func InstallableRuntimes() []string {
	out := []string{RuntimeKokoro, RuntimeImage}
	sort.Strings(out)
	return out
}

// RuntimePlan is what `setup --runtime <name>` would do.
type RuntimePlan struct {
	// Name is the runtime this plan is about.
	Name string
	// Present reports that the runtime already ANSWERED a probe. When
	// true, Install is empty and the command has nothing to do -- which
	// is the whole of "idempotent" here: a second run installs nothing
	// and says so in the present tense, rather than printing the
	// commands again at a machine that reads it as having lost the
	// runtime.
	Present bool
	// Detail is what was observed when Present ("http://127.0.0.1:8880
	// (0.2.4)"), for the sentence that says so.
	Detail string
	// Install is the exact commands, in order, empty when the runtime is
	// present or the plan refuses.
	Install []string
	// Refusal is the whole message when this machine cannot have this
	// runtime: complete sentences, naming what is missing and what fixes
	// it.
	Refusal string
	// Note is what the person should know that is neither a command nor
	// a refusal.
	Note string
}

// RuntimeHost is what DecideRuntime is allowed to know. A separate
// struct from Host because the questions are different -- Docker's GPU
// passthrough decides nothing here -- and because a function that took
// the whole of Host would invite reading a field that does not apply.
type RuntimeHost struct {
	GOOS, GOARCH string
	// Docker is the daemon's state. Only CLIPresent and Present are
	// read: an 82M speech model does not need a GPU passed through.
	Docker DockerFacts
	// KokoroPresent and KokoroDetail are the speech runtime's probe.
	KokoroPresent bool
	KokoroDetail  string
	// OllamaPresent reports that something answered the model runtime's
	// probe, and OllamaBase is where.
	OllamaPresent bool
	OllamaBase    string
	// ImageCapableModel is the id of a model this machine's runtime
	// reports image generation for, empty when there is none. Read from
	// the runtime's own capability list rather than from a model name or
	// the host platform -- see the models package's probe.
	ImageCapableModel string
}

// The sentences.
const (
	// installKokoroCPU / installKokoroGPU run the reference deployment.
	//
	// DOCKER ON macOS TOO, which is a deliberate divergence from D1's
	// "no Docker on macOS" and not an oversight. D1 forbids a container
	// there because it cannot reach the Mac's GPU, so an LLM would serve
	// from the CPU -- exactly what the hardware floor exists to prevent.
	// An 82M-parameter speech model is not that call: it is faster than
	// real time on a CPU, so the reason does not apply and the refusal
	// it would justify blocks a runtime that works.
	//
	// -p 127.0.0.1:8880:8880 rather than the vendor's -p 8880:8880, for
	// the reason the Ollama lines already give: Docker publishes a port
	// by writing its own DNAT and FORWARD rules, which a host firewall
	// managed through ufw does not sit in front of, and this service has
	// no authentication of any kind. Nothing off-box needs the port.
	//
	// --restart unless-stopped, because a runtime that does not come
	// back after a reboot leaves a machine advertising audioout it
	// cannot serve, and the call fails on somebody else's prompt.
	installKokoroCPU = "docker run -d --name kokoro --restart unless-stopped " +
		"-p 127.0.0.1:8880:8880 ghcr.io/remsky/kokoro-fastapi-cpu:latest"
	installKokoroGPU = "docker run -d --name kokoro --restart unless-stopped --gpus=all " +
		"-p 127.0.0.1:8880:8880 ghcr.io/remsky/kokoro-fastapi-gpu:latest"

	noteKokoroDockerOnMac = "Docker is offered here where it is not for the model runtime, and the" +
		" difference is the model: an 82M-parameter speech model runs faster than real time on a" +
		" CPU, so a container that cannot reach this Mac's GPU costs nothing. A language model in" +
		" the same container would serve from the CPU, which is what the hardware floor exists to" +
		" prevent."

	refusalRuntimeNoDocker = "Docker is not installed, and this machine runs the speech runtime in a" +
		" container. Install Docker Engine from https://docs.docker.com/engine/install/ and run this again."

	refusalRuntimeDockerDaemonSilent = "The docker command is installed and the Docker daemon did not" +
		" answer. Start it with sudo systemctl start docker, or add your user to the docker group with" +
		" sudo usermod -aG docker $USER and log back in, then run this again."

	refusalRuntimeDockerDesktopSilent = "The docker command is installed and the Docker daemon did not" +
		" answer. Start Docker Desktop and run this again."

	refusalImageNoOllama = "This machine has no model runtime, and image generation is a capability of" +
		" one rather than a separate service. Run memql worker setup --inference first, then run this again."
)

// DecideRuntime answers what installing this runtime would take.
func DecideRuntime(h RuntimeHost, name string) RuntimePlan {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case RuntimeKokoro:
		return decideKokoro(h)
	case RuntimeImage:
		return decideImage(h)
	default:
		return RuntimePlan{
			Name: name,
			Refusal: fmt.Sprintf("There is no runtime called %q. This command installs %s.",
				name, strings.Join(InstallableRuntimes(), " and ")),
		}
	}
}

func decideKokoro(h RuntimeHost) RuntimePlan {
	p := RuntimePlan{Name: RuntimeKokoro}

	// Presence is settled FIRST, so a refusal cannot erase it. A machine
	// that already speaks must never be told to install Docker.
	if h.KokoroPresent {
		p.Present = true
		p.Detail = h.KokoroDetail
		return p
	}

	if !h.Docker.CLIPresent {
		p.Refusal = refusalRuntimeNoDocker
		return p
	}
	if !h.Docker.Present {
		// Two spellings, because the fix is genuinely different: on
		// macOS there is no systemctl and no docker group, and printing
		// a Linux command there sends somebody to a shell that will not
		// have it.
		if h.GOOS == "darwin" {
			p.Refusal = refusalRuntimeDockerDesktopSilent
		} else {
			p.Refusal = refusalRuntimeDockerDaemonSilent
		}
		return p
	}

	// The GPU image needs NVIDIA SPECIFICALLY, and reading GPUToolkit
	// alone gets that wrong on exactly one machine: an AMD box.
	// DockerFacts sets GPUToolkit for AMD whenever /dev/kfd and
	// /dev/dri exist -- the device nodes ARE the ROCm passthrough --
	// so a toolkit-only test hands a Radeon machine `--gpus=all`,
	// which needs the NVIDIA container runtime and makes Docker refuse
	// with `could not select device driver "" with capabilities:
	// [[gpu]]`. Nothing installs, and the operator is looking at an
	// NVIDIA flag on a machine that has no NVIDIA card.
	//
	// The answer for AMD is the CPU image rather than a ROCm one:
	// kokoro-fastapi publishes no ROCm build, and this package's own
	// note already argues that a CPU is fine for an 82M speech model.
	// Naming a `-rocm` tag by symmetry with the model runtime would
	// print an image nobody can pull -- the same mistake the inference
	// record calls out about reaching for a "ROCm container toolkit".
	if h.GOOS == "linux" && h.Docker.GPUToolkit && h.Docker.GPUVendor == GPUVendorNVIDIA {
		p.Install = []string{installKokoroGPU}
	} else {
		p.Install = []string{installKokoroCPU}
	}
	if h.GOOS == "darwin" {
		p.Note = noteKokoroDockerOnMac
	}
	return p
}

func decideImage(h RuntimeHost) RuntimePlan {
	p := RuntimePlan{Name: RuntimeImage}

	if !h.OllamaPresent {
		p.Refusal = refusalImageNoOllama
		return p
	}
	if h.ImageCapableModel != "" {
		p.Present = true
		p.Detail = h.ImageCapableModel
		return p
	}

	// NO COMMAND, and a refusal that states what was OBSERVED rather
	// than a vendor fact.
	//
	// Which platforms Ollama offers image generation on is something
	// this cockpit cannot verify and would be wrong about within a
	// release -- so the sentence says what the runtime in front of us
	// reported, and names the pull that would change it. A refusal that
	// asserted "image generation is macOS only" would be a claim from a
	// changelog, and the person reading it on the platform that just
	// gained support has no way to argue with it.
	p.Refusal = fmt.Sprintf(
		"The model runtime at %s reports no image generation capability for any model it has."+
			" Pull an image model with memql worker models --pull <id> and run this again;"+
			" this machine advertises image generation only once its runtime answers for one.",
		orLocal(h.OllamaBase))
	return p
}

func orLocal(base string) string {
	if strings.TrimSpace(base) == "" {
		return "this machine"
	}
	return base
}
