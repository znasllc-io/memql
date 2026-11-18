package calls

import (
	"bytes"
	"strings"
	"testing"
)

func TestKeyRoundTrip(t *testing.T) {
	src := bytes.NewReader([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 255})
	raw, formatted, err := NewKey(src)
	if err != nil {
		t.Fatal(err)
	}
	if formatted != FormatKey(raw) {
		t.Fatalf("NewKey formatted %q, FormatKey says %q", formatted, FormatKey(raw))
	}
	if !strings.HasPrefix(formatted, KeyPrefix) || len(formatted) != len(KeyPrefix)+28 {
		t.Fatalf("unexpected shape %q", formatted)
	}
	if formatted != strings.ToLower(formatted) {
		t.Fatalf("canonical spelling must be lowercase: %q", formatted)
	}
	got, err := ParseKey(formatted)
	if err != nil {
		t.Fatal(err)
	}
	if got != raw {
		t.Fatalf("round trip changed the key: %x != %x", got, raw)
	}
	var ones [16]byte
	for i := range ones {
		ones[i] = 0xff
	}
	for _, edge := range [][16]byte{{}, ones} {
		back, err := ParseKey(FormatKey(edge))
		if err != nil || back != edge {
			t.Fatalf("edge key %x: %x, %v", edge, back, err)
		}
	}
}

func TestNewKeyShortReadFails(t *testing.T) {
	if _, _, err := NewKey(bytes.NewReader(make([]byte, 15))); err == nil {
		t.Fatal("a short read must fail")
	}
}

func TestParseKeyIsCaseInsensitiveAndRejectsATypo(t *testing.T) {
	raw, formatted, err := NewKey(bytes.NewReader([]byte("0123456789abcdef")))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseKey(strings.ToUpper(formatted)); err != nil || got != raw {
		t.Fatalf("upper case must parse: %v", err)
	}
	// Crockford aliases: i and l read as 1, o reads as 0.
	aliased := strings.NewReplacer("1", "i", "0", "o").Replace(formatted[len(KeyPrefix):])
	if got, err := ParseKey(KeyPrefix + aliased); err != nil || got != raw {
		t.Fatalf("aliases must parse: %v", err)
	}

	// Replace every symbol position with every symbol of a different value:
	// the weighted checksum must refuse each one.
	body := formatted[len(KeyPrefix):]
	for i := 0; i < len(body); i++ {
		for _, sym := range []byte(crockford) {
			if sym == body[i] {
				continue
			}
			typo := KeyPrefix + body[:i] + string(sym) + body[i+1:]
			if _, err := ParseKey(typo); err == nil {
				t.Fatalf("typo at %d (%c -> %c) was accepted: %s", i, body[i], sym, typo)
			}
		}
	}
}

// An unweighted sum catches every substitution but not a swap of two
// neighbours; the position weights are what make this pass.
func TestParseKeyRejectsSwappedNeighbours(t *testing.T) {
	_, formatted, _ := NewKey(bytes.NewReader([]byte("0123456789abcdef")))
	body := formatted[len(KeyPrefix):]
	swapped := 0
	for i := 0; i+1 < keyDataSymbols; i++ {
		if body[i] == body[i+1] {
			continue
		}
		b := []byte(body)
		b[i], b[i+1] = b[i+1], b[i]
		swapped++
		if _, err := ParseKey(KeyPrefix + string(b)); err == nil {
			t.Fatalf("swap at %d was accepted", i)
		}
	}
	if swapped == 0 {
		t.Fatal("test key has no different neighbours")
	}
}

func TestParseKeyErrorsAreDistinct(t *testing.T) {
	_, formatted, _ := NewKey(bytes.NewReader([]byte("0123456789abcdef")))
	body := formatted[len(KeyPrefix):]
	bumped := string(crockford[(strings.IndexByte(crockford, body[27])+1)%32])
	cases := []struct{ name, in, want string }{
		{"wrong prefix", "xx_" + body, "prefix"},
		{"too short", KeyPrefix + body[:27], "length"},
		{"too long", KeyPrefix + body + "0", "length"},
		{"invalid character", KeyPrefix + "u" + body[1:], "character"},
		{"bad checksum", KeyPrefix + body[:27] + bumped, "checksum"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseKey(c.in)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error naming %q", err, c.want)
			}
		})
	}
}

func TestParseKeyRequiresZeroPaddingBits(t *testing.T) {
	// The 26th symbol carries 3 data bits and 2 padding bits. A spelling with
	// a padding bit set, even with a matching checksum, is a second spelling
	// of the same key and must be refused.
	body := FormatKey([16]byte{})[len(KeyPrefix):]
	data := []byte(body[:26])
	data[25] = crockford[1]
	sum := 0
	for i, c := range data {
		sum += strings.IndexByte(crockford, c) * (i + 1)
	}
	sum %= 1021
	forged := KeyPrefix + string(data) + string(crockford[sum>>5]) + string(crockford[sum&31])
	if _, err := ParseKey(forged); err == nil || !strings.Contains(err.Error(), "padding") {
		t.Fatalf("got %v, want a padding error", err)
	}
}

func TestDigestDiffersByPepper(t *testing.T) {
	a := Digest([]byte("pepper-a"), []byte("secret"))
	b := Digest([]byte("pepper-b"), []byte("secret"))
	if a == b {
		t.Fatal("a different pepper must give a different digest")
	}
	if a != Digest([]byte("pepper-a"), []byte("secret")) {
		t.Fatal("a digest must be deterministic")
	}
	if len(a) != 64 || a != strings.ToLower(a) {
		t.Fatalf("want 64 lowercase hex characters, got %q", a)
	}
	if a == Digest([]byte("pepper-a"), []byte("secret2")) {
		t.Fatal("a different secret must give a different digest")
	}
}
