package pipelines

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
)

var (
	// logTimestamp is the RFC 3339 prefix a log store puts on every line:
	// GitHub's job logs, `kubectl logs --timestamps`.
	logTimestamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z `)
	// passLine is a passing package's result line. It is anchored: a test's
	// own output can quote "ok <pkg> 1.0s" mid-line, and reading that as a
	// measurement would teach the table a number nobody measured. A
	// "(cached)" result carries no duration, so it is not a measurement
	// either. The CI bridge's okLine (scripts/ci/selection) is the same
	// expression; the root module's parity test holds the two readers
	// together.
	passLine = regexp.MustCompile(`^ok\s+(\S+)\s+([0-9]+(?:\.[0-9]+)?)s(?:\s|$)`)
)

// maxResultLine bounds the lines ParseGoTestOutput reads whole. A result line
// is a package path and a duration; a longer line is a test's output, and is
// skipped rather than allowed to stop the read.
const maxResultLine = 64 * 1024

// ParseGoTestOutput reads `go test` output and returns each PASSING
// package's wall time by import path: only anchored `ok  <pkg>  <N>s`
// lines count; cached results, failures and mid-line quotes do not.
//
// A failed package's time is cut short by the failure or the timeout and
// would teach the table the wrong weight, so it is never read. A package
// reported more than once (a step that tests it twice) gets the median of
// its times, as the CI bridge's table refresh reads repeated samples. Every
// package is returned, whichever module it belongs to; which ones are the
// repository's is the caller's question.
func ParseGoTestOutput(r io.Reader) (map[string]float64, error) {
	samples := map[string][]float64{}
	br := bufio.NewReaderSize(r, maxResultLine)
	for {
		line, isPrefix, err := br.ReadLine()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading go test output: %w", err)
		}
		if isPrefix {
			// Longer than any result line: drop the rest of it.
			for isPrefix && err == nil {
				_, isPrefix, err = br.ReadLine()
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("reading go test output: %w", err)
			}
			continue
		}
		m := passLine.FindSubmatch(logTimestamp.ReplaceAll(line, nil))
		if m == nil {
			continue
		}
		secs, err := strconv.ParseFloat(string(m[2]), 64)
		if err != nil {
			return nil, fmt.Errorf("reading %q: %w", clip(string(line)), err)
		}
		samples[string(m[1])] = append(samples[string(m[1])], secs)
	}
	out := make(map[string]float64, len(samples))
	for importPath, xs := range samples {
		out[importPath] = medianOf(xs)
	}
	return out, nil
}

// MergeTimings returns table with every observed package's time replaced
// and every other row kept.
//
// A row the observation lacks is kept because a package missing from one
// run's output is not a package that stopped existing; Partition ignores
// rows for packages it is not asked to place. An observation that cannot be
// a time -- negative, not finite, or for no package -- is not written. The
// inputs are not changed.
func MergeTimings(table, observed map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(table)+len(observed))
	for importPath, secs := range table {
		out[importPath] = secs
	}
	for importPath, secs := range observed {
		if importPath == "" || secs < 0 || math.IsNaN(secs) || math.IsInf(secs, 0) {
			continue
		}
		out[importPath] = secs
	}
	return out
}

func medianOf(xs []float64) float64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}
