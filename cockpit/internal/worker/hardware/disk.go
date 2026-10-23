//go:build darwin || linux

package hardware

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// modelVolumeFree is space on the volume the RUNTIME keeps models on,
// which is the number a pull is refused against.
//
// It is deliberately not the root volume and not the home directory,
// which are the two a caller would otherwise assume: on a Linux machine
// running Ollama in a container the models live in a docker volume
// under /var/lib/docker, and on macOS they live in ~/.ollama. Measuring
// the wrong one produces a refusal quoting a number from a disk nobody
// is filling.
//
// Statfs uses Bavail rather than Bfree: the reserved blocks Bfree
// includes are not available to this process, and a pull refused on
// them would be refused on space that was never there.
func modelVolumeFree() uint64 {
	var st unix.Statfs_t
	if err := unix.Statfs(modelVolume(), &st); err != nil {
		return 0
	}
	return uint64(st.Bavail) * uint64(st.Bsize)
}

// modelVolume finds a path on the right volume. It walks up to the
// first directory that EXISTS, because statfs on a path that is not
// there fails, and a machine that has never run Ollama has no
// ~/.ollama -- the state a fresh setup is in when it most needs the
// number.
func modelVolume() string {
	if v := os.Getenv("OLLAMA_MODELS"); v != "" {
		return existingAncestor(v)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return os.TempDir()
	}
	return existingAncestor(filepath.Join(home, ".ollama"))
}

func existingAncestor(path string) string {
	for {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return os.TempDir()
		}
		path = parent
	}
}
