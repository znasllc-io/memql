package annotations

// RefusalCode is one of the codes Check and CheckAll refuse with, and what it
// means in a sentence or two. The generated attribute matrix lists them from
// here, so the page explains each code in the same words as the registry that
// answers with it.
type RefusalCode struct {
	Code    string
	Meaning string
}

// RefusalCodes returns every refusal code, in the order check.go declares
// them (TestRefusalCodesListEveryCode reads that file, so a new code cannot be
// declared without a meaning). The result is a fresh slice the caller may
// keep.
func RefusalCodes() []RefusalCode {
	return []RefusalCode{
		{CodeUnknown, "No construct or field accepts the name. The refusal suggests the nearest name the construct does accept, and lists them all."},
		{CodeRetired, "The name is retired where it was written. The refusal carries the migration hint, which says what to write instead."},
		{CodeMisplaced, "Other constructs or fields accept the name, but not this one. The refusal names where it is accepted and shows it written there."},
		{CodeForm, "The construct accepts the annotation, but not written this way: a string where it takes a number, or arguments on a flag. The refusal says what it takes and shows its example."},
		{CodeKey, "A keyword argument the annotation does not have, or a key written in the wrong shape: a flag given a value, or a valued key written bare. The refusal lists the keys and shows the example."},
		{CodeRepeated, "An annotation that is not repeatable is written twice on one declaration, so a reader cannot tell which one takes effect. Keep one."},
	}
}
