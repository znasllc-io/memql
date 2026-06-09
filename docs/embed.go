package docs

import _ "embed"

var (
	//go:embed public/language/memql.md
	memqlGuide string
)

// MemqlGuide returns the embedded MemQL reference documentation.
func MemqlGuide() string {
	return memqlGuide
}
