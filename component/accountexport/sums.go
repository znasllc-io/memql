package accountexport

import (
	"bufio"
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

// parseSums reads what renderSums wrote.
func parseSums(data []byte) (map[string]string, []string, error) {
	out := map[string]string{}
	var order []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	n := 0
	for sc.Scan() {
		n++
		line := sc.Text()
		digest, path, ok := strings.Cut(line, "  ")
		if !ok || len(digest) != 64 || path == "" {
			return nil, nil, fmt.Errorf("line %d is not `<sha256>  <path>`", n)
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return nil, nil, fmt.Errorf("line %d: %v", n, err)
		}
		if _, dup := out[path]; dup {
			return nil, nil, fmt.Errorf("line %d lists %s twice", n, path)
		}
		out[path] = digest
		order = append(order, path)
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	return out, order, nil
}
