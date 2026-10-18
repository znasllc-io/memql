package inference

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// The macOS facts: where the native runtime would go when Homebrew is not
// there to install it, free disk where the models live, how an installed
// runtime that is not answering would be started -- and deliberately
// NOTHING about Docker.
//
// Design D1 rules Docker out here, and this file honours that by not
// looking rather than by looking and ignoring the answer. A Docker
// Desktop found on a Mac would be a fact sitting in Host with nothing
// allowed to read it, and the next person to touch decideDarwin would
// find it and wire it up -- producing a container with no access to the
// GPU, serving on the CPU, which is exactly what the hardware floor
// exists to prevent.
func gatherPlatform(_ context.Context) platformFacts {
	home, _ := os.UserHomeDir()
	n := DarwinNativeFacts(home, os.Getenv("OLLAMA_MODELS"))
	n.Installed = exists(filepath.Join(n.RuntimeDir, "ollama"))
	n.FreeDisk = availableBytes(n.RuntimeDir)
	models := ollamaModelsDir()
	if n.Installed {
		// The runtime an earlier native setup unpacked keeps its models
		// where its LaunchAgent says, not where Homebrew's would.
		models = n.ModelsDir
	}
	return platformFacts{
		Docker: DockerFacts{
			Reason: "not probed on macOS: a container has no access to the GPU, so Docker is not a runtime option here",
		},
		Native:    n,
		FreeDisk:  availableBytes(models),
		ModelsDir: models,
		stopped:   darwinStopped,
	}
}

// darwinStopped is how an installed Ollama that is not answering would be
// started on this Mac: MemQL's own agent first (loaded or not), then
// Homebrew's service when the binary on PATH is Homebrew's, then Ollama's
// own app.
func darwinStopped(ctx context.Context, h Host) StoppedFacts {
	var s StoppedFacts
	if exists(h.Native.UnitPath) {
		s.Agent = true
		s.AgentLoaded = exec.CommandContext(ctx, "launchctl", "print",
			fmt.Sprintf("gui/%d/%s", h.UID, OllamaAgentLabel)).Run() == nil
	}
	if path, err := h.LookPath("ollama"); err == nil {
		if resolved, err := filepath.EvalSymlinks(path); err == nil && strings.Contains(resolved, "/Cellar/ollama/") {
			s.Brew = true
		}
	}
	home, _ := os.UserHomeDir()
	s.App = exists("/Applications/Ollama.app") || exists(filepath.Join(home, "Applications", "Ollama.app"))
	return s
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ollamaModelsDir is where native Ollama keeps model blobs. OLLAMA_MODELS
// overrides it, which an operator with a small internal disk and a large
// external one will have set -- and reading the home directory anyway
// would report free space on a volume the models never touch.
func ollamaModelsDir() string {
	if dir := os.Getenv("OLLAMA_MODELS"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ollama", "models")
}

// availableBytes is what a non-root process may still write on that
// volume -- Bavail, not Bfree, which counts the reserved blocks this
// process cannot have. It is duplicated in plan_linux.go rather than
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
