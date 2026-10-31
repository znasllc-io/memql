package gamespack

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"sort"
	"testing"
)

// The vectors are part of the runtime contract: a runtime computes the
// signature itself and must reach these exact strings.
func TestGoalSignatureVectors(t *testing.T) {
	cases := []struct {
		goal string
		keys []string
		want string
	}{
		{"Light up the area!", []string{"spot"}, "d22aafce7ea66a16b435a1e6b8d178a8cdcbddaf5f254dc5677096ac76b5c0bd"},
		{"light up   the area", []string{"spot"}, "d22aafce7ea66a16b435a1e6b8d178a8cdcbddaf5f254dc5677096ac76b5c0bd"},
		{"Light up the area", []string{"spot", "count"}, "8d4463cfb1db4bf8b0f154d2ba3692bb4b2607dc4c9081bc8925aa8766db0b62"},
		{"Light up the area", nil, "eb75279beebedb94ae46116ef860a3d2ff48ff45b7806fce07a465d838c2fa2a"},
		{"Go to the den.", []string{" ", ""}, "6fff361fbd783f6a11ef41e5bcf9d79e5259623e70a430eb79ba06034eea11b5"},
		// Punctuation inside a word is dropped, not turned into a space
		// ("Don't" is "dont", "fox's" is "foxs"), a colon is dropped with it,
		// and the keys sort before they join ("b", "a" is "a,b"), so the
		// preimage is "dont stop build the foxs den|a,b". A runtime that
		// replaces punctuation with a space, or joins the keys as given,
		// reaches a different string.
		{"Don't stop: build the fox's den!", []string{"b", "a"}, "57c03bb17ab6febbdff6e8e0167f00a5ebc1dafee50d6d77e1632144febebfd7"},
		// Text outside ASCII is classified by Unicode category, not by script. A
		// letter of any script (here e-acute, u-umlaut and the Cyrillic word) and a
		// decimal digit are kept; every other character is dropped, with no
		// replacement and without ending a run of spaces: the right single
		// quotation mark (U+2019, twice), the em dash (U+2014) and the hyphen-minus
		// (U+002D) alike. So the two quotation marks leave "dont" and "caf\u00e9s",
		// the hyphen leaves "\u00fcbercozy", and the two spaces round the dash fold
		// into one. The preimage, hashed as UTF-8, is
		// "dont stop build the caf\u00e9s \u00fcbercozy 2nd floor \u0434\u043e\u043c|spot".
		// A runtime that keeps only ASCII letters, turns punctuation or the hyphen
		// into a space, folds spaces before it drops punctuation, or normalizes the
		// text to NFD or NFKD first reaches a different string.
		{"Don\u2019t stop \u2014 build the caf\u00e9\u2019s \u00fcber-cozy 2nd floor, \u0434\u043e\u043c!", []string{"spot"}, "4cd6e0c44687e08cfd04ea977e9d74da2e7922782ffb6dfb9062c425b52bd949"},
		// What row 7 cannot pin. A capital Greek word ending in sigma is lower-cased
		// code point by code point, so its last letter is sigma and not final sigma
		// (a whole-string toLowerCase gives the final form). The no-break space
		// (U+00A0) is a space, and the vulgar fraction one half (U+00BD, a number but
		// not a decimal digit) is dropped between two of them without ending the run,
		// so the three leave one space. The Arabic-Indic digit two (U+0662) is a
		// decimal digit of another script and stays. The mathematical bold capital A
		// (U+1D400) is a letter above U+FFFF with no lower-case form and stays whole.
		// The keys show the key rules: the key padded with no-break spaces is trimmed
		// to "a", the ideographic space (U+3000) is blank and dropped, the second "a"
		// is not de-duplicated, "B" sorts before "a" (byte order, not collation), and
		// the fullwidth A (U+FF21) sorts before the Deseret letter U+10400 (code point
		// order; a UTF-16 sort puts them the other way round). The preimage is
		// "walk \u03bf\u03b4\u03bf\u03c3 \u0662 laps then sit \U0001d400|B,a,a,b,\uff21,\U00010400".
		{
			"Walk \u039f\u0394\u039f\u03a3\u00a0\u00bd\u00a0\u0662 laps, then sit \U0001d400",
			[]string{"b", "\u00a0a\u00a0", "B", "a", "\u3000", "\uff21", "\U00010400"},
			"56673f937921f7cb90f7e3b0660ac365e9f769f9098dc465e13b565af1145fbe",
		},
	}
	for _, c := range cases {
		if got := GoalSignature(c.goal, c.keys); got != c.want {
			t.Errorf("GoalSignature(%q, %v) = %s, want %s", c.goal, c.keys, got, c.want)
		}
	}
}

func TestGoalSignatureIgnoresInputValuesAndKeyOrder(t *testing.T) {
	a := GoalSignature("Build a den", inputKeys(map[string]any{"near": "home", "size": 3}))
	b := GoalSignature("build a den", inputKeys(map[string]any{"size": 9, "near": "base"}))
	if a != b {
		t.Fatalf("the signature must depend on input KEYS only: %s != %s", a, b)
	}
	if c := GoalSignature("build a den", []string{"near"}); c == a {
		t.Fatal("a different key set must give a different signature")
	}
}

// A key is trimmed before it enters the signature and a blank one is dropped,
// so the spelling of the list a caller holds does not matter.
func TestGoalSignatureTrimsKeysAndDropsBlankOnes(t *testing.T) {
	want := GoalSignature("Build a den", []string{"count", "spot"})
	for _, keys := range [][]string{
		{"spot", "count"},
		{" spot ", "\tcount\n"},
		{"spot", "", "count", "   "},
	} {
		if got := GoalSignature("Build a den", keys); got != want {
			t.Errorf("GoalSignature with keys %q = %s, want the signature of count,spot: %s", keys, got, want)
		}
	}
}

func TestInputKeysNamesEveryKeyOfTheObject(t *testing.T) {
	got := inputKeys(map[string]any{"near": "home", "size": 3, "spot": nil})
	sort.Strings(got)
	if want := []string{"near", "size", "spot"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("inputKeys = %v, want %v", got, want)
	}
	if got := inputKeys(nil); len(got) != 0 {
		t.Fatalf("inputKeys(nil) = %v, want none", got)
	}
}

// sha256Hex is the hex sha256 of a preimage written out in a test.
func sha256Hex(preimage string) string {
	sum := sha256.Sum256([]byte(preimage))
	return hex.EncodeToString(sum[:])
}

// The key rules one at a time, each against the preimage written out and hashed
// here, so that a failure of the vectors can be traced to the rule that broke.
func TestGoalSignatureKeyRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []string
		want string // the keys as they enter the preimage
	}{
		{"sorted in byte order, upper case first", []string{"b", "B", "a"}, "B,a,b"},
		{"equal keys are kept", []string{"a", "a"}, "a,a"},
		{"trimmed of any Unicode space", []string{"\u00a0a\u3000", "b"}, "a,b"},
		{"a key blank after trimming is dropped", []string{"\u3000", " ", ""}, ""},
		{"code point order, not UTF-16 order", []string{"\U00010400", "\uff21"}, "\uff21,\U00010400"},
	} {
		if got, want := GoalSignature("goal", tc.keys), sha256Hex("goal|"+tc.want); got != want {
			t.Errorf("%s: GoalSignature(goal, %q) is not the signature of the preimage %q", tc.name, tc.keys, "goal|"+tc.want)
		}
	}
}

// The doc comment's rules for the text, one claim at a time: each input against
// the text the rules leave of it. The vectors pin whole goals; this says which
// rule a failure belongs to, and it is the list a runtime's own tests can copy.
func TestGoalSignatureNormalizationClasses(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"a dropped dash between spaces does not end the run", "a - b", "a b"},
		{"a dropped dash inside a word joins it", "a-b", "ab"},
		{"an apostrophe is dropped, not spaced", "Don't", "dont"},
		{"the right single quotation mark and the dashes are dropped", "a\u2019b \u2014 c\u2013d", "ab cd"},
		{"a no-break space is a space", "a\u00a0b", "a b"},
		{"the next-line control (U+0085) is a space", "a\u0085b", "a b"},
		{"spaces of different kinds fold to one", "a\u3000\u2003 b", "a b"},
		{"spaces and dropped characters round a word leave only the word", "  \u2014 a !\u3000", "a"},
		{"the byte-order mark is dropped, not a space", "a\ufeffb", "ab"},
		{"the zero-width space is dropped, not a space", "a\u200bb", "ab"},
		{"capital letters of other scripts are lower-cased", "\u00c9cole \u0414\u041e\u041c", "\u00e9cole \u0434\u043e\u043c"},
		{"a capital sigma is sigma, also at the end of a word", "\u03a3\u03a3 \u0391\u03a3", "\u03c3\u03c3 \u03b1\u03c3"},
		{"a capital I with a dot above is a plain i", "\u0130stanbul", "istanbul"},
		{"decimal digits of another script are kept", "\u0662\u0663", "\u0662\u0663"},
		{"numbers that are not decimal digits are dropped", "\u00bd \u00b2 \u2167 \u2460", ""},
		{"a letter and its combining mark are two characters, and the mark is dropped", "cafe\u0301", "cafe"},
		{"a precomposed letter is kept", "caf\u00e9", "caf\u00e9"},
		{"letters above U+FFFF are kept, lower-cased where they have a lower case", "\U0001d400\U00010400", "\U0001d400\U00010428"},
		{"an emoji is dropped", "fun \U0001f600 time", "fun time"},
	} {
		if got, want := GoalSignature(tc.in, nil), sha256Hex(tc.want+"|"); got != want {
			t.Errorf("%s: GoalSignature(%q) is not the signature of the text %q", tc.name, tc.in, tc.want)
		}
	}
}
