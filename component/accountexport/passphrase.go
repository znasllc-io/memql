package accountexport

import (
	"crypto/rand"
	"fmt"
)

// passphraseAlphabet leaves out I, O, 0 and 1, which are read for one another
// when a passphrase is copied by hand. Its 32 characters divide 256, so a
// random byte masked to five bits picks each one with the same chance.
const passphraseAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

const (
	passphraseGroups    = 5
	passphraseGroupSize = 5
)

// GeneratePassphrase answers 25 random characters in five groups of five
// joined by "-": 125 bits, long enough that scrypt's cost is not what
// protects the file.
func GeneratePassphrase() (string, error) {
	raw := make([]byte, passphraseGroups*passphraseGroupSize)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("accountexport: generate passphrase: %w", err)
	}
	out := make([]byte, 0, len(raw)+passphraseGroups-1)
	for i, b := range raw {
		if i > 0 && i%passphraseGroupSize == 0 {
			out = append(out, '-')
		}
		out = append(out, passphraseAlphabet[b&31])
	}
	return string(out), nil
}
