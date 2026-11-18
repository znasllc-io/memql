package calls

import (
	"bytes"
	"regexp"
	"strings"
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

// TestLinksCarryTheirTokensInTheFragment: both links name the deployable's host and
// carry their secrets after the '#', which a browser never sends to a server.
func TestLinksCarryTheirTokensInTheFragment(t *testing.T) {
	if got, want := GuestLink("calls.example.test", "guesttoken"), "https://calls.example.test/#j=guesttoken"; got != want {
		t.Fatalf("guest link %q, want %q", got, want)
	}
	if got, want := HostLink("calls.example.test", "guesttoken", "hosttoken"), "https://calls.example.test/#j=guesttoken&h=hosttoken"; got != want {
		t.Fatalf("host link %q, want %q", got, want)
	}
}

// TestEveryIssuedTokenIsWellFormed: a session token and a call link are NewToken over
// TokenBytes, and TokenWellFormed accepts each of them whatever the draw, or a person
// holding a good one is refused with nothing anywhere saying why.
func TestEveryIssuedTokenIsWellFormed(t *testing.T) {
	for i := range 200 {
		tok, err := NewToken(bytes.NewReader(bytes.Repeat([]byte{byte(i)}, TokenBytes)), TokenBytes)
		if err != nil {
			t.Fatal(err)
		}
		if !TokenWellFormed(tok) {
			t.Fatalf("an issued token is not well formed: %q", tok)
		}
	}
	if TokenBytes != 20 {
		t.Fatalf("TokenBytes = %d: 160 bits is what no guessing at any request rate reaches", TokenBytes)
	}
}

// TestTokenWellFormedRefusesWhatNoIssueProduces: what is refused here is refused without a
// read, so each case has to be something NewToken over TokenBytes can never make.
func TestTokenWellFormedRefusesWhatNoIssueProduces(t *testing.T) {
	good, err := NewToken(bytes.NewReader(bytes.Repeat([]byte{0x5a}, TokenBytes)), TokenBytes)
	if err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{
		"nothing":                "",
		"a symbol short":         good[:len(good)-1],
		"a symbol long":          good + "a",
		"upper case":             strings.ToUpper(good),
		"padding":                good[:len(good)-1] + "=",
		"a digit outside base32": good[:len(good)-1] + "1",
		"a space around it":      " " + good,
		"another token's length": strings.Repeat("a", 26),
	} {
		if TokenWellFormed(tok) {
			t.Errorf("%s: %q is well formed", name, tok)
		}
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
