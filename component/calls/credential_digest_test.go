package calls

import "testing"

func TestCredentialDigestPreservesStoredEncoding(t *testing.T) {
	if got := CredentialDigest([]byte("abc")); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("persisted public-credential fingerprint changed: %s", got)
	}
}
