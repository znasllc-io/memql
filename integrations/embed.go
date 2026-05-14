package integrations

import "embed"

// Files exposes embedded integration configuration documents.
// This can be used for default configurations or templates.
//
//go:embed *.json
var Files embed.FS
