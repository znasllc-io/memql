package tools

import (
	"fmt"
	"os"
	"path/filepath"
)

// Admission measures the filesystem that will hold this checkout, not a
// possibly stale Fleet inventory or the system temporary directory. The caller
// holds the host capacity lock. External programs and a separate Docker volume
// remain outside this headroom check; nothing here deletes their data.
func checkPipelineDisk(workspace string, minimumMiB int) error {
	if minimumMiB == 0 {
		minimumMiB = DefaultPipelineMinFreeDiskMiB
	}
	if minimumMiB < 1024 || minimumMiB > 1048576 {
		return fmt.Errorf("Build capacity cannot be checked: pipelines.min_free_disk_mib is invalid. Check this machine's pipeline policy; no pipeline command started.")
	}
	path, err := pipelineFilesystemDirectory(workspace)
	if err != nil {
		return fmt.Errorf("Build capacity cannot be checked for %s: %w. Check this machine's workspace access; no pipeline command started.", workspace, err)
	}
	available, err := pipelineAvailableDiskBytes(path)
	if err != nil {
		return fmt.Errorf("Build capacity cannot be checked for %s: %w. Check this machine's storage; no pipeline command started.", path, err)
	}
	if available < uint64(minimumMiB)<<20 {
		return fmt.Errorf("Build waiting for disk space: %d MiB available at %s; %d MiB required by pipelines.min_free_disk_mib. Free space or move the pipeline workspace to a larger disk; no pipeline command started.", available>>20, path, minimumMiB)
	}
	return nil
}

// Before the first checkout the configured root may not exist. Only walk past
// absent paths, never past inaccessible paths or a dangling symlink: otherwise
// a different parent's free space could incorrectly admit the real target.
func pipelineFilesystemDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("workspace path must be absolute")
	}
	path = filepath.Clean(path)
	for {
		info, err := os.Stat(path)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("workspace path is not a directory")
			}
			return path, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		if _, linkErr := os.Lstat(path); linkErr == nil || !os.IsNotExist(linkErr) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		path = parent
	}
}
