package scenario

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
)

// digest wraps sha256 so the corpus fingerprint has one spelling and the
// package's only crypto import sits in one file.
type digest struct{ h hash.Hash }

func newDigest() *digest { return &digest{h: sha256.New()} }

func (d *digest) Write(b []byte) { d.h.Write(b) }

// Hex returns the digest, truncated to 16 characters. Full width buys nothing
// here -- the fingerprint is a same-or-different marker on a row a person
// reads, not a security boundary -- and 64 characters of hex in a scorecard
// column is 64 characters nobody reads.
func (d *digest) Hex() string { return hex.EncodeToString(d.h.Sum(nil))[:16] }
