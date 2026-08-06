package baseparser

// idents.go holds the canonical identifier predicates. Before this
// package shipped, three near-identical implementations of
// `[A-Za-z_][A-Za-z0-9_]*` lived in mutation_templates.go,
// language/parser/parser.go, and (historically) shape_parser.go.
// Hoisted here as the single source of truth.

// IsSimpleIdentifier reports whether s is a non-empty
// `[A-Za-z_][A-Za-z0-9_]*` identifier. The ASCII-only definition
// matches what every memQL parser expected at its call sites; the
// language doesn't allow unicode in identifiers today.
func IsSimpleIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 0 {
			if !IsIdentifierStart(c) {
				return false
			}
			continue
		}
		if !IsIdentifierByte(c) {
			return false
		}
	}
	return true
}

// IsIdentifierStart reports whether b is a valid leading byte for an
// ASCII identifier: A-Z, a-z, or '_'.
func IsIdentifierStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}

// IsIdentifierByte reports whether b is a valid continuation byte for
// an ASCII identifier: A-Z, a-z, 0-9, or '_'.
func IsIdentifierByte(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') ||
		b == '_'
}
