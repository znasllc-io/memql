package memoryNodes

import (
	"strings"
	"testing"
	"time"
)

func TestDeclaredRetentionPolicy(t *testing.T) {
	for _, tc := range []struct {
		annotation string
		ttl        time.Duration
		latest     bool
		archive    string
	}{
		{`@retention(ttl="30d")`, 30 * 24 * time.Hour, true, "required"},
		{`@retention(ttl="48h",keepLatest=false,archive="none")`, 48 * time.Hour, false, "none"},
	} {
		c, err := ParseConceptMemQL([]byte(tc.annotation+"\nconcept probe { value string }"), "v1/ref/probe")
		if err != nil {
			t.Fatal(err)
		}
		if c.Retention == nil || c.Retention.TTL != tc.ttl || c.Retention.KeepLatest != tc.latest || c.Retention.Archive != tc.archive {
			t.Fatalf("policy = %+v", c.Retention)
		}
	}
	c := buildFixtureConcept(t, "concept probe { value string }")
	if c.Retention != nil {
		t.Fatal("undeclared concept acquired a retention policy")
	}
	for _, annotation := range []string{`@retention(keepLatest=true)`, `@retention(ttl="0d")`, `@retention(ttl="-1h")`, `@retention(ttl="999999999999999d")`, `@retention(ttl="1ns")`, `@retention(ttl="30d",keepLatest="false")`, `@retention(ttl="30d",archive="maybe")`, `@retention(ttl="30d",unknown=true)`, `@retention(ttl="30d") @retention(ttl="60d")`} {
		_, err := ParseConceptMemQL([]byte(annotation+"\nconcept probe { value string }"), "v1/ref/probe")
		if err == nil {
			t.Errorf("accepted %s", annotation)
		}
	}
}

func TestLogRetentionRequiresRecoverableWholeDays(t *testing.T) {
	for _, annotation := range []string{`@retention(ttl="1h",keepLatest=false)`, `@retention(ttl="30d")`, `@retention(ttl="30d",keepLatest=false,archive="none")`} {
		_, err := ParseConceptMemQL([]byte(annotation+"\nconcept logLine { message string }"), "v1/observability/logLine")
		if err == nil || !strings.Contains(err.Error(), "whole days") {
			t.Fatalf("%s: %v", annotation, err)
		}
	}
}
