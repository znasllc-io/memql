package appsession

import (
	"strings"
	"testing"
)

func TestRedactor_ReplacesEverySecretItHolds(t *testing.T) {
	r := newRedactor(testBearer)
	const renewed = "eyJhbGciOiJSUzI1NiJ9.the-second-bearer.sig"
	r.add(renewed)

	in := "config says Authorization: Bearer " + testBearer + " and later " + renewed + " too"
	got := string(r.apply([]byte(in)))
	if strings.Contains(got, testBearer) || strings.Contains(got, renewed) {
		t.Fatalf("a credential survived redaction: %q", got)
	}
	// Renewal is why both are held: a chunk buffered before a renewal
	// can still carry the previous bearer.
	if strings.Count(got, string(redactedMarker)) != 2 {
		t.Errorf("want two visible redactions, got: %q", got)
	}
}

// TestRedactor_MarkerIsVisible. A reader should be able to tell that
// something was removed, rather than seeing a transcript that silently
// does not match what the app printed.
func TestRedactor_MarkerIsVisible(t *testing.T) {
	r := newRedactor(testBearer)
	got := string(r.apply([]byte(testBearer)))
	if got != string(redactedMarker) {
		t.Errorf("got %q, want the visible marker", got)
	}
}

func TestRedactorMarkerCannotRecreateASecret(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		secrets    []string
	}{
		{"boundary", "00000000000000[", []string{"0000000["}},
		{"inside-marker", "a session credential here", []string{"session credential"}},
		{"renewal-order", "00000000000000[", []string{"redacted", "0000000["}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRedactor(tc.secrets...)
			got := r.apply2(tc.body)
			if got != "***" {
				t.Fatalf("ambiguous replacement must suppress the chunk: %q", got)
			}
			for _, secret := range tc.secrets {
				if strings.Contains(got, secret) {
					t.Fatal("a replacement reconstructed a registered value")
				}
			}
			if r.apply2(got) != got {
				t.Fatal("fallback redaction is not stable")
			}
		})
	}
}

// TestRedactor_IgnoresShortSecrets: a redactor that matched a
// two-character string would scribble over ordinary output.
func TestRedactor_IgnoresShortSecrets(t *testing.T) {
	r := newRedactor("ab")
	if got := string(r.apply([]byte("a table of abbreviations"))); got != "a table of abbreviations" {
		t.Errorf("a short secret was matched: %q", got)
	}
	if r.holdBack() != 0 {
		t.Errorf("holdBack = %d with no usable secret", r.holdBack())
	}
}

// TestRedactor_HoldBackCoversASplit is the reason streamReader retains a
// tail on an early flush: a credential split across two chunks would
// survive redaction in two halves, and the two halves are the credential.
func TestRedactor_HoldBackCoversASplit(t *testing.T) {
	r := newRedactor(testBearer)
	if got := r.holdBack(); got != len(testBearer)-1 {
		t.Errorf("holdBack = %d, want %d", got, len(testBearer)-1)
	}
}

func TestRedactor_NilIsSafe(t *testing.T) {
	var r *redactor
	if got := string(r.apply([]byte("anything"))); got != "anything" {
		t.Errorf("nil redactor altered data: %q", got)
	}
	if r.holdBack() != 0 {
		t.Error("nil redactor should hold nothing back")
	}
	r.add("something")
}

// TestSecretScanFindsACredentialAcrossWrites: the scan runs over a file
// in the reads that hash it, so a credential split between two of them --
// or spread over several short ones -- is still found, and nothing that
// is not a credential is.
func TestSecretScanFindsACredentialAcrossWrites(t *testing.T) {
	r := newRedactor(testBearer)
	body := []byte("before " + testBearer + " after")
	for size := 1; size <= len(body); size++ {
		scan := r.scanner()
		for i := 0; i < len(body); i += size {
			end := min(i+size, len(body))
			_, _ = scan.Write(body[i:end])
		}
		if !scan.found {
			t.Fatalf("writes of %d bytes hid the credential", size)
		}
	}
	clean := r.scanner()
	_, _ = clean.Write([]byte("before " + testBearer[:len(testBearer)-1]))
	_, _ = clean.Write([]byte("X after"))
	if clean.found {
		t.Error("a near miss was taken for the credential")
	}
	if newRedactor().scanner() != nil {
		t.Error("a redactor with nothing to find still scans")
	}
}
