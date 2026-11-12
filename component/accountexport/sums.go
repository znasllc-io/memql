package accountexport

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strings"
)

// sumLine is one member's line in SHA256SUMS.
type sumLine struct {
	path   string // relative to the folder
	digest string // lower-case hex
}

// renderSums writes the list in the format `sha256sum -c` reads: the digest,
// two spaces (text mode, which is what both GNU sha256sum and BSD
// `shasum -a 256` read), the path relative to the folder, so that
// `cd <stem> && sha256sum -c SHA256SUMS` checks every member. Paths are held
// to safeSegment's character set, so none needs the escaped form.
func renderSums(lines []sumLine) []byte {
	var b bytes.Buffer
	for _, l := range lines {
		fmt.Fprintf(&b, "%s  %s\n", l.digest, l.path)
	}
	return b.Bytes()
}

// parseSumLine reads one line renderSums wrote, its newline included.
func parseSumLine(line []byte) (sumLine, error) {
	text := strings.TrimSuffix(string(line), "\n")
	digest, path, ok := strings.Cut(text, "  ")
	if !ok || len(digest) != sha256HexLen || path == "" {
		return sumLine{}, fmt.Errorf("not `<sha256>  <path>`")
	}
	if _, err := hex.DecodeString(digest); err != nil || strings.ToLower(digest) != digest {
		return sumLine{}, fmt.Errorf("the digest is not lower-case hex")
	}
	return sumLine{path: path, digest: digest}, nil
}
