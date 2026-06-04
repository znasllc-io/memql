package main

import _ "embed"

// embeddedVersionFile holds the fallback VERSION contents for environments where
// the VERSION file is not present on disk.
//
//go:embed VERSION
var embeddedVersionFile []byte

// ci-routing verification (memql#856) — remove after verification
