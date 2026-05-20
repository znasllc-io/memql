package docs

import _ "embed"

var (
	//go:embed core/memql.md
	memqlGuide string
)

// MemqlGuide returns the embedded MemQL reference documentation.
func MemqlGuide() string {
	return memqlGuide
}
