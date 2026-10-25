package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func projectedVersion(text string) ReviewFileVersion {
	return ReviewFileVersion{Present: true, Mode: "100644", BlobSHA: reviewBlob(text), Content: text}
}
func projectionSource(files ...ReviewFileChange) ReviewDiff {
	return ReviewDiff{BeforeSHA: strings.Repeat("1", 40), AfterSHA: strings.Repeat("2", 40), BeforeTree: strings.Repeat("3", 40), AfterTree: strings.Repeat("4", 40), Files: files}
}
func TestReviewProjectionReconstructsEverySourceRepresentation(t *testing.T) {
	for _, tc := range []struct{ name, before, after string }{
		{"added", "", "one\n"}, {"deleted", "one\n", ""}, {"CRLF", "a\r\nb\r\n", "a\r\nc\r\n"}, {"EOF", "a\n", "a"}, {"missing-final", "a", "b"}, {"empty", "", ""},
		{"repeated", "x\nx\na\nx\nx\n", "x\nx\nb\nx\nx\n"}, {"moves", "one\ntwo\nthree\n", "three\none\ntwo\n"}, {"unicode", "xé\n", "x中\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := ReviewFileChange{"source", projectedVersion(tc.before), projectedVersion(tc.after)}
			if tc.name == "added" || tc.name == "empty" {
				f.Before = ReviewFileVersion{}
			}
			if tc.name == "deleted" {
				f.After = ReviewFileVersion{}
			}
			d := projectionSource(f)
			before := d.Digest()
			p, err := ProjectReviewDiff(t.Context(), d)
			if err != nil {
				t.Fatal(err)
			}
			if err = verifyReviewHunks(reviewLines(tc.before), reviewLines(tc.after), p.Files[0].Hunks); err != nil {
				t.Fatal(err)
			}
			if p.SourceDigest != before || d.Digest() != before {
				t.Fatal("full evidence changed")
			}
			if err = ValidateReviewProjection(t.Context(), d, p); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestReviewProjectionInventoryIncludesEmptyModesTestsAndGenerated(t *testing.T) {
	mode := projectedVersion("unchanged\n")
	executable := mode
	executable.Mode = "100755"
	d := projectionSource(ReviewFileChange{"a.empty", ReviewFileVersion{}, projectedVersion("")}, ReviewFileChange{"b_test.go", projectedVersion("old\n"), projectedVersion("new\n")}, ReviewFileChange{"generated.go", projectedVersion("old generated\n"), projectedVersion("new generated\n")}, ReviewFileChange{"mode.sh", mode, executable}, ReviewFileChange{"rename-new", ReviewFileVersion{}, projectedVersion("same\n")}, ReviewFileChange{"rename-old", projectedVersion("same\n"), ReviewFileVersion{}})
	p, err := ProjectReviewDiff(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Files) != 6 || len(p.Files[0].Hunks) != 0 || len(p.Files[3].Hunks) != 0 || p.Files[3].After.Mode != "100755" || p.Files[0].Before.Present || !p.Files[0].After.Present {
		t.Fatalf("lost inventory: %+v", p)
	}
	for _, i := range []int{1, 2, 4, 5} {
		if len(p.Files[i].Hunks) == 0 {
			t.Fatal("omitted changed file")
		}
	}
}
func numberedLines(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "line-%04d\n", i)
	}
	return b.String()
}
func TestReviewProjectionFixedContextAndCoalescing(t *testing.T) {
	before := numberedLines(100)
	after := strings.ReplaceAll(before, "line-0030\n", "changed30\n")
	after = strings.ReplaceAll(after, "line-0040\n", "changed40\n")
	after = strings.ReplaceAll(after, "line-0080\n", "changed80\n")
	p, err := ProjectReviewDiff(t.Context(), projectionSource(ReviewFileChange{"source", projectedVersion(before), projectedVersion(after)}))
	if err != nil {
		t.Fatal(err)
	}
	hs := p.Files[0].Hunks
	if len(hs) != 2 || hs[0].BeforeStart != 23 || hs[0].BeforeCount != 27 || hs[1].BeforeStart != 73 || hs[1].BeforeCount != 17 {
		t.Fatalf("context not exact: %+v", hs)
	}
	if hs[0].Lines[0].Text != "line-0022\n" || hs[0].Lines[len(hs[0].Lines)-1].Text != "line-0048\n" {
		t.Fatal("wrong adjacent context")
	}
}
func TestReviewProjectionRejectsChangedOrIncompleteHunks(t *testing.T) {
	d := projectionSource(ReviewFileChange{"source", projectedVersion(numberedLines(80)), projectedVersion(strings.ReplaceAll(numberedLines(80), "line-0040\n", "changed\r\n"))})
	original, err := ProjectReviewDiff(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		alter func(*ReviewProjection)
	}{
		{"missing-file", func(p *ReviewProjection) { p.Files = nil }}, {"missing-hunk", func(p *ReviewProjection) { p.Files[0].Hunks = nil }},
		{"duplicate-file", func(p *ReviewProjection) { p.Files = append(p.Files, p.Files[0]) }}, {"truncated", func(p *ReviewProjection) { h := &p.Files[0].Hunks[0]; h.Lines = h.Lines[:len(h.Lines)-1] }},
		{"newline", func(p *ReviewProjection) { p.Files[0].Hunks[0].Lines[0].Text = "forged\n" }}, {"count", func(p *ReviewProjection) { p.Files[0].Hunks[0].AfterCount++ }},
		{"format", func(p *ReviewProjection) { p.Format = "other" }}, {"algorithm", func(p *ReviewProjection) { p.Algorithm = "other" }}, {"context", func(p *ReviewProjection) { p.ContextLines++ }}, {"source", func(p *ReviewProjection) { p.SourceDigest = strings.Repeat("0", 64) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(original)
			var p ReviewProjection
			if err := json.Unmarshal(b, &p); err != nil {
				t.Fatal(err)
			}
			tc.alter(&p)
			if ValidateReviewProjection(t.Context(), d, p) == nil {
				t.Fatal("changed projection accepted")
			}
			if p.Digest() == original.Digest() {
				t.Fatal("projection identity did not bind change")
			}
		})
	}
	// Reconstruction independently catches a missing actual edit even if a
	// caller changes counters to look internally consistent.
	if verifyReviewHunks(reviewLines(d.Files[0].Before.Content), reviewLines(d.Files[0].After.Content), nil) == nil {
		t.Fatal("missing edit reconstructed")
	}
}
func TestReviewProjectionRefusesBoundsAndInvalidSource(t *testing.T) {
	small := projectionSource(ReviewFileChange{"source", projectedVersion("a\n"), projectedVersion("b\n")})
	for _, tc := range []struct {
		name  string
		alter func(*ReviewDiff)
	}{
		{"path-bound", func(d *ReviewDiff) { d.Files[0].Path = strings.Repeat("x", 4097) }},
		{"file-count", func(d *ReviewDiff) { d.Files = make([]ReviewFileChange, 257) }},
		{"bad-blob", func(d *ReviewDiff) { d.Files[0].After.Content = "forged" }}, {"duplicate", func(d *ReviewDiff) { d.Files = append(d.Files, d.Files[0]) }},
		{"unsupported-mode", func(d *ReviewDiff) { d.Files[0].After.Mode = "120000" }}, {"absent-content", func(d *ReviewDiff) { d.Files[0].Before.Present = false }},
		{"file-limit", func(d *ReviewDiff) { d.Files[0].After = projectedVersion(strings.Repeat("a", (1<<20)+1)) }},
		{"line-work", func(d *ReviewDiff) {
			d.Files[0].After = projectedVersion(strings.Repeat("\n", reviewProjectionMaxLines))
		}},
		{"output", func(d *ReviewDiff) {
			d.Files[0].After = projectedVersion(strings.Repeat("x", ReviewProjectionMaxBytes))
		}},
		{"escaped-output", func(d *ReviewDiff) {
			d.Files[0].After = projectedVersion(strings.Repeat("\t", ReviewProjectionMaxBytes/2))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := small
			d.Files = append([]ReviewFileChange(nil), small.Files...)
			tc.alter(&d)
			if _, err := ProjectReviewDiff(t.Context(), d); err == nil {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ProjectReviewDiff(ctx, small); err == nil {
		t.Fatal("cancel ignored")
	}
}
func TestReviewProjectionDeterministicRepeatedLineCoverage(t *testing.T) {
	for n := 1; n <= 96; n++ {
		a := strings.Repeat("same\n", n) + "anchor\n" + strings.Repeat("same\n", n)
		b := strings.Repeat("same\n", n/2) + "replacement\n" + strings.Repeat("same\n", n) + "anchor\n"
		d := projectionSource(ReviewFileChange{"source", projectedVersion(a), projectedVersion(b)})
		p, err := ProjectReviewDiff(t.Context(), d)
		if err != nil {
			t.Fatal(err)
		}
		q, err := ProjectReviewDiff(t.Context(), d)
		if err != nil || !reflect.DeepEqual(p, q) {
			t.Fatal("nondeterministic projection", err)
		}
	}
}
