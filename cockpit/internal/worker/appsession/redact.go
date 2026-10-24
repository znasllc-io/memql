package appsession

import (
	"bytes"
	"io"
	"os"
	"sync"
)

// redact.go keeps the session bearer out of the transcript.
//
// The transcript is persisted on the engine side and rendered in the
// portal, so a chunk carrying the credential publishes it to everywhere
// that record reaches. The app can echo its own configuration for
// entirely reasonable reasons -- a debug flag, an error message quoting
// the file it failed to parse, `claude mcp list` -- and none of those are
// misbehaviour worth blocking. Redacting on the way out is what makes
// them harmless.
//
// Renewal means there is more than one secret to hide: a chunk buffered
// before a renewal can still carry the previous bearer, so every
// credential the session has ever held stays in the set.

// redactedMarker replaces a credential in outgoing bytes. It is visible
// on purpose: a reader should be able to tell that something was removed,
// rather than seeing a transcript that silently does not match what the
// app printed.
var redactedMarker = []byte("[redacted: memql session credential]")

type redactor struct {
	mu      sync.RWMutex
	secrets [][]byte
	longest int
}

func newRedactor(secrets ...string) *redactor {
	r := &redactor{}
	for _, s := range secrets {
		r.add(s)
	}
	return r
}

// minRedactableSecret is the shortest string this redactor will act on.
//
// Named rather than inline because it is a real threshold with a real
// consequence in BOTH directions, and a reader needs to see which one it
// buys: below it, a redactor matching a two-character string would
// scribble over ordinary output until a transcript was unreadable; at or
// above it, every credential this cockpit handles is covered, because a
// bearer is never short. The fuzz target references this constant rather
// than a copy of the number, so the two cannot drift.
const minRedactableSecret = 8

// add registers another secret. Short values are ignored -- see
// minRedactableSecret.
func (r *redactor) add(secret string) {
	if r == nil || len(secret) < minRedactableSecret {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.secrets {
		if string(existing) == secret {
			return
		}
	}
	r.secrets = append(r.secrets, []byte(secret))
	if len(secret) > r.longest {
		r.longest = len(secret)
	}
}

// holdBack is how many trailing bytes a partial flush must retain so a
// secret cannot be split across two chunks and survive in halves.
func (r *redactor) holdBack() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.longest == 0 {
		return 0
	}
	return r.longest - 1
}

// apply returns data with every registered secret replaced.
func (r *redactor) apply(data []byte) []byte {
	if r == nil || len(data) == 0 {
		return data
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := data
	for _, secret := range r.secrets {
		if bytes.Contains(out, secret) {
			out = bytes.ReplaceAll(out, secret, redactedMarker)
		}
	}
	// A marker can itself contain a registered value, or recreate one at
	// its boundary with the retained text. Replacing repeatedly can loop
	// forever. In that exceptional case suppress the whole chunk with a
	// marker shorter than every secret we accept, then stop.
	for _, secret := range r.secrets {
		if bytes.Contains(out, secret) {
			return []byte("***")
		}
	}
	return out
}

// apply2 is apply for a string -- the End's error text, which can quote a
// config file or a request the app made.
func (r *redactor) apply2(s string) string {
	if r == nil || s == "" {
		return s
	}
	return string(r.apply([]byte(s)))
}

// holds reports whether data carries any credential this session was
// ever given. apply rewrites a credential in bytes that travel as text,
// but bytes that travel as base64, or as a file pushed whole, never pass
// through apply at all -- and a rewritten copy would not match its own
// digest anyway. So what might carry one is asked this first.
func (r *redactor) holds(data []byte) bool {
	if r == nil || len(data) == 0 {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, secret := range r.secrets {
		if bytes.Contains(data, secret) {
			return true
		}
	}
	return false
}

// count is how many credentials the redactor holds. They are only ever
// added, so a count that grew means bytes checked before may now match.
func (r *redactor) count() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.secrets)
}

// scanner returns a writer that watches everything written through it for
// any credential, including one split across two writes -- so a file can
// be checked in the same pass that hashes it, at any size. nil when there
// is nothing to look for.
func (r *redactor) scanner() *secretScan {
	if r.count() == 0 {
		return nil
	}
	return &secretScan{r: r}
}

// secretScan is the writer scanner returns. found says whether a
// credential went through it.
type secretScan struct {
	r     *redactor
	tail  []byte
	found bool
}

func (s *secretScan) Write(p []byte) (int, error) {
	if s == nil || s.found || len(p) == 0 {
		return len(p), nil
	}
	// A credential split across the last write and this one lies in the
	// tail kept from before plus this write's first holdBack bytes.
	keep := s.r.holdBack()
	head := p
	if len(head) > keep {
		head = head[:keep]
	}
	seam := append(append([]byte(nil), s.tail...), head...)
	if s.r.holds(seam) || s.r.holds(p) {
		s.found = true
		return len(p), nil
	}
	// Keep the last holdBack bytes of everything so far for the next seam.
	if len(p) >= keep {
		s.tail = append(s.tail[:0], p[len(p)-keep:]...)
	} else {
		joined := append(append([]byte(nil), s.tail...), p...)
		if len(joined) > keep {
			joined = joined[len(joined)-keep:]
		}
		s.tail = joined
	}
	return len(p), nil
}

// holdsFile reports whether the file at path carries any credential.
func (r *redactor) holdsFile(path string) (bool, error) {
	scan := r.scanner()
	if scan == nil {
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if _, err := io.Copy(scan, f); err != nil {
		return false, err
	}
	return scan.found, nil
}
