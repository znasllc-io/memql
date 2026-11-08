package sitetraffic

import (
	"log/slog"
	"testing"
	"time"

	"github.com/uptrace/bun"
)

func TestVisitorHashIsStableWithinADayAndDiffersAcrossSitesAndDays(t *testing.T) {
	day1 := []byte("0123456789abcdef0123456789abcdef")
	day2 := []byte("fedcba9876543210fedcba9876543210")
	a := visitorHash(day1, "site-a", "203.0.113.0/24", "UA")
	if len(a) != 32 {
		t.Fatalf("want 32 hex chars, got %d (%q)", len(a), a)
	}
	if a != visitorHash(day1, "site-a", "203.0.113.0/24", "UA") {
		t.Fatal("the same inputs must hash the same")
	}
	if a == visitorHash(day1, "site-b", "203.0.113.0/24", "UA") {
		t.Fatal("another site must hash differently")
	}
	if a == visitorHash(day2, "site-a", "203.0.113.0/24", "UA") {
		t.Fatal("another day's secret must hash differently")
	}
	if a == visitorHash(day1, "site-a", "203.0.113.0/24", "Other") {
		t.Fatal("another user agent must hash differently")
	}
}

func TestNoSecretMeansNoVisitor(t *testing.T) {
	v := NewVisitorSecrets(slog.Default(), func() *bun.DB { return nil }, time.Now)
	if got := v.Visitor("s", "k", "ua", time.Now()); got != "" {
		t.Fatalf("an unstarted keeper must yield no visitor, got %q", got)
	}
}
