package main

import (
	"runtime/debug"
	"strings"
)

// Semver build metadata identifies the executable that registered, even when
// several development builds share a release number. It does not affect version
// precedence, and reaches --version, local status, and Fleet through one value.
func stampedVersion(version, revision string, dirty bool) string {
	if revision == "" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, setting := range info.Settings {
				if setting.Key == "vcs.revision" {
					revision = setting.Value
				}
				if setting.Key == "vcs.modified" {
					dirty = setting.Value == "true"
				}
			}
		}
	}
	return versionWithRevision(version, revision, dirty)
}

func versionWithRevision(version, revision string, dirty bool) string {
	if len(revision) > 12 {
		revision = revision[:12]
	}
	separator := "+"
	if strings.Contains(version, "+") {
		separator = "."
	}
	if revision == "" {
		return version + separator + "unknown"
	}
	version += separator + "g" + revision
	if dirty {
		version += ".dirty"
	}
	return version
}
