//go:build !darwin && !linux

package inference

import "context"

// gatherPlatform: every other GOOS. Windows and the BSDs are not worker
// platforms for this cockpit and are not inference platforms either, so
// there is nothing to probe -- Decide refuses on the floor's own verdict
// before it reads any of this.
//
// It returns a Reason rather than a bare zero value, because a
// DockerFacts full of false with nothing to explain it reads as "Docker
// was looked for and not found" to whoever prints the diagnostic.
func gatherPlatform(_ context.Context) platformFacts {
	return platformFacts{
		Docker: DockerFacts{
			Reason: "not probed: local models run on macOS (Apple Silicon) and Linux (discrete GPU)",
		},
	}
}

// availableBytes is the free-space reading the stage consults before a
// download. No platform other than macOS and Linux serves models, so
// nothing here is ever staged; zero is "not established", which refuses
// nothing, and exists so stage.go compiles on every platform.
func availableBytes(string) uint64 { return 0 }
