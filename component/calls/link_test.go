package calls

import (
	"bytes"
	"regexp"
	"testing"
)

func TestNewTokenIsUnpaddedLowercaseBase32(t *testing.T) {
	tok, err := NewToken(bytes.NewReader(bytes.Repeat([]byte{0xab}, 16)), 16)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[a-z2-7]{26}$`).MatchString(tok) {
		t.Fatalf("unexpected token %q", tok)
	}
	long, err := NewToken(bytes.NewReader(bytes.Repeat([]byte{0x01}, 32)), 32)
	if err != nil || len(long) != 52 {
		t.Fatalf("32 bytes must give 52 symbols, got %q, %v", long, err)
	}
}

func TestNewTokenRefusesBadInput(t *testing.T) {
	if _, err := NewToken(bytes.NewReader(make([]byte, 8)), 16); err == nil {
		t.Fatal("a short read must fail")
	}
	for _, n := range []int{0, -1} {
		if _, err := NewToken(bytes.NewReader(make([]byte, 16)), n); err == nil {
			t.Fatalf("n=%d must fail", n)
		}
	}
}
