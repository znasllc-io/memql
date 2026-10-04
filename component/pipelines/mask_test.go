package pipelines

import (
	"slices"
	"strings"
	"testing"
)

// A secret's value never reaches a check run or a log tail a person reads
// (Review Focus 5): every occurrence is masked.
func TestMaskSecretsReplacesEveryOccurrence(t *testing.T) {
	got := MaskSecrets("publishing with npm_9f2c4e1a; retry with npm_9f2c4e1a.", []string{"npm_9f2c4e1a"})
	if want := "publishing with ***; retry with ***."; got != want {
		t.Errorf("MaskSecrets = %q, want %q", got, want)
	}
}

// Masking a value shorter than four bytes would mangle ordinary output (a
// secret "1" would mask every 1) while hiding next to nothing.
func TestMaskSecretsLeavesShortValuesAlone(t *testing.T) {
	text := "abc abcd 1 2 3"
	if got := MaskSecrets(text, []string{"abc", "1", "", "   "}); got != text {
		t.Errorf("MaskSecrets = %q, want the text unchanged", got)
	}
}

// Two secrets that overlap in the text are masked as one span, so no byte of
// either survives between two masks. Replacing them one after the other
// would leave the end of the longer one readable.
func TestMaskSecretsMasksOverlappingValuesWhole(t *testing.T) {
	cases := []struct {
		text   string
		values []string
		want   string
	}{
		{"prefix-ABCDEFGH-suffix", []string{"ABCDEF", "DEFGH"}, "prefix-***-suffix"},
		{"abcdef", []string{"bcdef", "abcd"}, "***"},
		{"aaaaaa", []string{"aaaa"}, "***"},
		{"keykey and key", []string{"keyk", "ykey"}, "*** and key"},
		{"keykey", []string{"keyk", "eyke"}, "***y"},
	}
	for _, c := range cases {
		if got := MaskSecrets(c.text, c.values); got != c.want {
			t.Errorf("MaskSecrets(%q, %q) = %q, want %q", c.text, c.values, got, c.want)
		}
	}
}

// A secret stored with a trailing newline is printed without it, and a
// multi-line secret (a private key) is printed one line at a time behind a
// log prefix: each form is masked.
func TestMaskSecretsMasksTheFormsAValueIsPrintedIn(t *testing.T) {
	stored := "tok3n-v4lue\n"
	if got := MaskSecrets("Authorization: tok3n-v4lue", []string{stored}); got != "Authorization: ***" {
		t.Errorf("a value stored with a newline: %q", got)
	}
	key := "-----BEGIN KEY-----\nMIIBOgIBAAJBAKj34GkxFhD90vcN\nLYLInFEX6Ppy1tPf9Cnzj4p4WGeK\n-----END KEY-----\n"
	log := "2026-10-03T09:21:44Z MIIBOgIBAAJBAKj34GkxFhD90vcN\n2026-10-03T09:21:44Z LYLInFEX6Ppy1tPf9Cnzj4p4WGeK\n"
	got := MaskSecrets(log, []string{key})
	if strings.Contains(got, "MIIBOgIBAAJBAKj34GkxFhD90vcN") || strings.Contains(got, "LYLInFEX6Ppy1tPf9Cnzj4p4WGeK") {
		t.Errorf("a line of a multi-line secret survived: %q", got)
	}
}

// MaskForms is what MaskSecrets masks, for a caller that masks text a piece
// at a time and must know what a piece may end inside of (the substrate's log
// follower): each value as stored, without the whitespace around it, and each
// of its lines without theirs -- four bytes or more, each once. The forms are
// their own forms, so MaskSecrets over them masks exactly what it masks over
// the values.
func TestMaskFormsAreWhatMaskSecretsMasks(t *testing.T) {
	values := []string{
		"hunter2-token \n",                           // stored with whitespace around it
		"-----BEGIN-----\n    \nbody-line\n  ab  \n", // an indent-only line and a short one are no forms
		"abc",           // too short to mask
		"hunter2-token", // a form already given
	}
	want := []string{
		"hunter2-token \n",
		"hunter2-token",
		"-----BEGIN-----\n    \nbody-line\n  ab  \n",
		"-----BEGIN-----\n    \nbody-line\n  ab",
		"-----BEGIN-----",
		"body-line",
	}
	got := MaskForms(values)
	if !slices.Equal(sorted(got), sorted(want)) {
		t.Errorf("MaskForms = %q,\nwant (in any order) %q", got, want)
	}
	if again := MaskForms(got); !slices.Equal(sorted(again), sorted(got)) {
		t.Errorf("MaskForms(MaskForms(values)) = %q, want the forms again: they are their own forms", again)
	}
	text := "token hunter2-token printed; key -----BEGIN----- then body-line;    return nil; ab"
	if byForms, byValues := MaskSecrets(text, got), MaskSecrets(text, values); byForms != byValues {
		t.Errorf("MaskSecrets over the forms = %q, over the values = %q: want the same", byForms, byValues)
	}
	if got := MaskForms(nil); len(got) != 0 {
		t.Errorf("MaskForms(nil) = %q, want none", got)
	}
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

func TestMaskSecretsWithNothingToMaskReturnsTheText(t *testing.T) {
	if got := MaskSecrets("plain output", nil); got != "plain output" {
		t.Errorf("MaskSecrets(text, nil) = %q", got)
	}
	if got := MaskSecrets("", []string{"s3cret"}); got != "" {
		t.Errorf("MaskSecrets(\"\", values) = %q", got)
	}
}
