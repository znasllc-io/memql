package num

import _ "embed"

// Source is the implementation shipped with standalone, reproducible renderers.
// Embedding the actual file keeps exported artifacts on the same numeric rules.
//
//go:embed num.go
var Source string
