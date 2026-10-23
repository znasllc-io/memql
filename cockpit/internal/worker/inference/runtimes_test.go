package inference

import (
	"strings"
	"testing"
)

// The table asserts the EXACT sentence on every path, for the reason
// the plan_test table gives: these sentences are the whole product for
// somebody who is blocked, and a shared constant would let a reworded
// refusal stay green.

func kokoroHost(mut func(*RuntimeHost)) RuntimeHost {
	h := RuntimeHost{
		GOOS: "linux", GOARCH: "amd64",
		Docker: DockerFacts{CLIPresent: true, Present: true, Version: "27.3.1"},
	}
	if mut != nil {
		mut(&h)
	}
	return h
}

func TestDecideRuntimeKokoro(t *testing.T) {
	for _, tc := range []struct {
		name string
		host RuntimeHost

		wantPresent bool
		wantInstall []string
		wantRefusal string
		wantNote    string
	}{
		{
			name: "linux with docker and no gpu passthrough takes the CPU image",
			host: kokoroHost(nil),
			wantInstall: []string{"docker run -d --name kokoro --restart unless-stopped " +
				"-p 127.0.0.1:8880:8880 ghcr.io/remsky/kokoro-fastapi-cpu:latest"},
		},
		{
			name: "linux with NVIDIA passthrough takes the GPU image",
			host: kokoroHost(func(h *RuntimeHost) {
				h.Docker.GPUToolkit = true
				h.Docker.GPUVendor = GPUVendorNVIDIA
			}),
			wantInstall: []string{"docker run -d --name kokoro --restart unless-stopped --gpus=all " +
				"-p 127.0.0.1:8880:8880 ghcr.io/remsky/kokoro-fastapi-gpu:latest"},
		},
		{
			// AMD sets GPUToolkit TRUE -- the /dev/kfd and /dev/dri
			// nodes are the ROCm passthrough -- so a toolkit-only test
			// hands a Radeon machine `--gpus=all` and Docker refuses
			// with `could not select device driver`. Nothing installs.
			name: "linux with AMD passthrough takes the CPU image, not --gpus=all",
			host: kokoroHost(func(h *RuntimeHost) {
				h.Docker.GPUToolkit = true
				h.Docker.GPUVendor = GPUVendorAMD
			}),
			wantInstall: []string{"docker run -d --name kokoro --restart unless-stopped " +
				"-p 127.0.0.1:8880:8880 ghcr.io/remsky/kokoro-fastapi-cpu:latest"},
		},
		{
			// And a vendor neither probe recognised: the CPU image,
			// which runs anywhere, rather than a flag for a card
			// nobody established is there.
			name: "linux with a toolkit and an unknown vendor takes the CPU image",
			host: kokoroHost(func(h *RuntimeHost) { h.Docker.GPUToolkit = true }),
			wantInstall: []string{"docker run -d --name kokoro --restart unless-stopped " +
				"-p 127.0.0.1:8880:8880 ghcr.io/remsky/kokoro-fastapi-cpu:latest"},
		},
		{
			// Docker on macOS, which D1 forbids for the MODEL runtime.
			// The note says why the two differ, because a reader who
			// knows D1 will otherwise read this as a bug.
			name: "macOS takes the CPU image and explains why Docker is offered here",
			host: kokoroHost(func(h *RuntimeHost) { h.GOOS, h.GOARCH = "darwin", "arm64" }),
			wantInstall: []string{"docker run -d --name kokoro --restart unless-stopped " +
				"-p 127.0.0.1:8880:8880 ghcr.io/remsky/kokoro-fastapi-cpu:latest"},
			wantNote: noteKokoroDockerOnMac,
		},
		{
			name:        "no docker at all",
			host:        kokoroHost(func(h *RuntimeHost) { h.Docker = DockerFacts{} }),
			wantRefusal: refusalRuntimeNoDocker,
		},
		{
			name:        "the docker command with a silent daemon, on linux",
			host:        kokoroHost(func(h *RuntimeHost) { h.Docker.Present = false }),
			wantRefusal: refusalRuntimeDockerDaemonSilent,
		},
		{
			// A DIFFERENT sentence, because the fix is different: macOS
			// has no systemctl and no docker group, and printing a Linux
			// command there sends somebody to a shell that will not have
			// it.
			name: "the docker command with a silent daemon, on macOS",
			host: kokoroHost(func(h *RuntimeHost) {
				h.GOOS, h.GOARCH = "darwin", "arm64"
				h.Docker.Present = false
			}),
			wantRefusal: refusalRuntimeDockerDesktopSilent,
		},
		{
			// PRESENCE WINS OVER EVERY REFUSAL. A machine that already
			// speaks must never be told to install Docker -- and it is
			// settled before any branch, so no refusal can erase it.
			name: "already running, with no docker at all",
			host: kokoroHost(func(h *RuntimeHost) {
				h.Docker = DockerFacts{}
				h.KokoroPresent = true
				h.KokoroDetail = "http://127.0.0.1:8880 (0.2.4)"
			}),
			wantPresent: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideRuntime(tc.host, RuntimeKokoro)

			if got.Present != tc.wantPresent {
				t.Errorf("Present = %v, want %v", got.Present, tc.wantPresent)
			}
			if got.Refusal != tc.wantRefusal {
				t.Errorf("Refusal =\n  %q\nwant\n  %q", got.Refusal, tc.wantRefusal)
			}
			if got.Note != tc.wantNote {
				t.Errorf("Note =\n  %q\nwant\n  %q", got.Note, tc.wantNote)
			}
			if len(got.Install) != len(tc.wantInstall) {
				t.Fatalf("Install = %q, want %q", got.Install, tc.wantInstall)
			}
			for i := range got.Install {
				if got.Install[i] != tc.wantInstall[i] {
					t.Errorf("Install[%d] =\n  %q\nwant\n  %q", i, got.Install[i], tc.wantInstall[i])
				}
			}
		})
	}
}

func TestDecideRuntimeImage(t *testing.T) {
	t.Run("no model runtime at all", func(t *testing.T) {
		got := DecideRuntime(RuntimeHost{GOOS: "darwin"}, RuntimeImage)
		if got.Refusal != refusalImageNoOllama {
			t.Fatalf("Refusal =\n  %q\nwant\n  %q", got.Refusal, refusalImageNoOllama)
		}
		if len(got.Install) != 0 {
			t.Fatalf("a refusal carried commands: %q", got.Install)
		}
	})

	t.Run("a runtime that reports an image model is already present", func(t *testing.T) {
		got := DecideRuntime(RuntimeHost{
			GOOS: "darwin", OllamaPresent: true,
			OllamaBase: "http://127.0.0.1:11434", ImageCapableModel: "x/z-image-turbo",
		}, RuntimeImage)
		if !got.Present {
			t.Fatalf("Present = false, got %+v", got)
		}
		if got.Detail != "x/z-image-turbo" {
			t.Fatalf("Detail = %q", got.Detail)
		}
	})

	// THE REFUSAL STATES WHAT WAS OBSERVED, never a vendor fact. Which
	// platforms Ollama offers image generation on is something this
	// cockpit cannot verify and would be wrong about within a release,
	// and a person reading "macOS only" on the platform that just
	// gained support has no way to argue with it.
	t.Run("a runtime with no image-capable model names what it saw", func(t *testing.T) {
		got := DecideRuntime(RuntimeHost{
			GOOS: "linux", OllamaPresent: true, OllamaBase: "http://127.0.0.1:11434",
		}, RuntimeImage)

		if got.Present {
			t.Fatal("Present = true with no image-capable model")
		}
		want := "The model runtime at http://127.0.0.1:11434 reports no image generation capability" +
			" for any model it has. Pull an image model with memql worker models --pull <id> and run" +
			" this again; this machine advertises image generation only once its runtime answers for one."
		if got.Refusal != want {
			t.Errorf("Refusal =\n  %q\nwant\n  %q", got.Refusal, want)
		}
		for _, forbidden := range []string{"macOS only", "not available on", "only offered on"} {
			if strings.Contains(got.Refusal, forbidden) {
				t.Errorf("the refusal asserted a vendor fact this cockpit cannot verify: %q", got.Refusal)
			}
		}
	})
}

// An unknown name is refused and NAMES THE ALTERNATIVES, so a typo
// reads as a typo rather than as a runtime nobody has heard of.
func TestDecideRuntimeUnknownName(t *testing.T) {
	got := DecideRuntime(RuntimeHost{}, "whisper")
	want := `There is no runtime called "whisper". This command installs image and kokoro.`
	if got.Refusal != want {
		t.Fatalf("Refusal =\n  %q\nwant\n  %q", got.Refusal, want)
	}
}

// A PLAN ALWAYS SAYS SOMETHING. The same property Decide holds, and for
// the same reason: a plan that neither refuses, nor reports the runtime
// present, nor prints a command is a blank terminal in front of a
// blocked person.
func TestDecideRuntimeAlwaysSaysSomething(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows", ""} {
		for _, name := range append(InstallableRuntimes(), "nonsense") {
			for _, cli := range []bool{true, false} {
				for _, daemon := range []bool{true, false} {
					for _, toolkit := range []bool{true, false} {
						for _, kokoro := range []bool{true, false} {
							for _, ollama := range []bool{true, false} {
								for _, image := range []string{"", "x/z-image-turbo"} {
									h := RuntimeHost{
										GOOS:              goos,
										Docker:            DockerFacts{CLIPresent: cli, Present: daemon, GPUToolkit: toolkit},
										KokoroPresent:     kokoro,
										OllamaPresent:     ollama,
										ImageCapableModel: image,
									}
									p := DecideRuntime(h, name)
									if p.Refusal == "" && !p.Present && len(p.Install) == 0 {
										t.Fatalf("blank plan for %s on %s: %+v", name, goos, h)
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

// NOTHING HERE RUNS SUDO, and nothing here PRINTS one either -- unlike
// the model runtime's refusals, which name `sudo systemctl start
// docker` for a person to run. The distinction is carried by the copy
// in inference_cmd, so a refusal that gained a sudo line without the
// caller learning about it would print an unlabelled root command.
//
// It walks the whole cross-product rather than InstallableRuntimes()
// alone: `image` never populates Install on any host, so a loop over
// the two names would run zero iterations for half of them and report
// a pass it never earned.
func TestNoInstallCommandRunsSudo(t *testing.T) {
	checked := 0
	for _, name := range InstallableRuntimes() {
		for _, goos := range []string{"darwin", "linux"} {
			for _, vendor := range []GPUVendor{GPUVendorNVIDIA, GPUVendorAMD, GPUVendorUnknown} {
				for _, toolkit := range []bool{true, false} {
					p := DecideRuntime(RuntimeHost{
						GOOS: goos,
						Docker: DockerFacts{
							CLIPresent: true, Present: true,
							GPUVendor: vendor, GPUToolkit: toolkit,
						},
					}, name)
					for _, cmd := range p.Install {
						checked++
						if strings.Contains(cmd, "sudo") {
							t.Fatalf("%s on %s would run sudo: %q", name, goos, cmd)
						}
					}
				}
			}
		}
	}
	// `image` contributes none, so this counts the kokoro paths only --
	// stated so the day image gains an install command, this number
	// moves and somebody looks.
	if checked == 0 {
		t.Fatal("no install command was examined; the loop covered nothing")
	}
}

// The published port is bound to the LOOPBACK. This service has no
// authentication of any kind, and Docker publishes a port by writing
// its own DNAT and FORWARD rules that a ufw-managed host firewall does
// not sit in front of -- so an operator who believes they are
// firewalled is not.
func TestKokoroPublishesOnLoopbackOnly(t *testing.T) {
	for _, toolkit := range []bool{true, false} {
		p := DecideRuntime(kokoroHost(func(h *RuntimeHost) { h.Docker.GPUToolkit = toolkit }), RuntimeKokoro)
		for _, cmd := range p.Install {
			if !strings.Contains(cmd, "-p 127.0.0.1:8880:8880") {
				t.Fatalf("the speech runtime was published beyond the loopback: %q", cmd)
			}
		}
	}
}

// And it comes back after a reboot. A runtime that does not leaves a
// machine advertising audioout it cannot serve, and the call fails on
// somebody else's prompt.
func TestKokoroRestartsUnlessStopped(t *testing.T) {
	p := DecideRuntime(kokoroHost(nil), RuntimeKokoro)
	for _, cmd := range p.Install {
		if !strings.Contains(cmd, "--restart unless-stopped") {
			t.Fatalf("the speech runtime would not survive a reboot: %q", cmd)
		}
	}
}
