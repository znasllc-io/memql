package logstore

import (
	"testing"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func TestLogRetentionReadsTheLiveConceptDeclaration(t *testing.T) {
	t.Setenv(EnvRetentionDays, "45")
	c := &memorynodes.Concept{Name: "v1:observability:logLine"}
	registry := memorynodes.NewRegistry(map[string]*memorynodes.Concept{c.Name: c})
	i := &Integration{concepts: registry}
	if got := i.retentionDays(); got != 45 {
		t.Fatalf("undeclared behavior=%d", got)
	}
	declared := *c
	declared.Retention = &memorynodes.RetentionPolicy{TTL: 14 * 24 * time.Hour, KeepLatest: false, Archive: "required"}
	registry.MergeAll(map[string]*memorynodes.Concept{c.Name: &declared})
	if got := i.sweeper().RetentionDays; got != 14 {
		t.Fatalf("declared sweep window=%d", got)
	}
}
