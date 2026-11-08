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
//
// running is how many other steps hold a slot on this machine
// (pipelines.max_concurrent_steps). Each is presumed to still need the whole
// minimum, whatever it has written so far: nothing measures what a running
// step will write next, so a step is admitted only when the disk has the
// minimum for it and for each of them. That over-reserves by what the running
// steps have already written, and it is what keeps two steps admitted on the
// same free space from both running out of it.
func checkPipelineDisk(workspace string, minimumMiB int, running int) error {
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
	running = max(running, 0)
	required := uint64(minimumMiB) * uint64(1+running)
	if available >= required<<20 {
		return nil
	}
	if running == 0 {
		return fmt.Errorf("Build waiting for disk space: %d MiB available at %s; %d MiB required by pipelines.min_free_disk_mib. Free space or move the pipeline workspace to a larger disk; no pipeline command started.", available>>20, path, minimumMiB)
	}
	return fmt.Errorf("Build waiting for disk space: %d MiB available at %s; %d MiB required, pipelines.min_free_disk_mib for this step and for each of the %d already running here. Free space, move the pipeline workspace to a larger disk or lower pipelines.max_concurrent_steps; no pipeline command started.", available>>20, path, required, running)
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
