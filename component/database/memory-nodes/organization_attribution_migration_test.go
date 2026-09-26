package memoryNodes

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
)

func TestOrganizationAttributionMigrationOnlyRepairsProvableChildren(t *testing.T) {
	dsn := os.Getenv("MEMQL_DATABASE_DSN")
	if dsn == "" {
		if os.Getenv("MEMQL_REQUIRE_DB") == "1" {
			t.Fatal("MEMQL_DATABASE_DSN is required")
		}
		t.Skip("set MEMQL_DATABASE_DSN to run the isolated temporary-table migration test")
	}
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		if os.Getenv("MEMQL_REQUIRE_DB") == "1" {
			t.Fatal(err)
		}
		t.Skipf("Postgres unavailable: %v", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// A transaction-local table shadows any real graph. This test cannot
	// update installation rows even when the DSN names an operator's cluster.
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE "MemoryNodes" (concept text, id text, "createdAt" timestamptz, payload jsonb) ON COMMIT DROP`); err != nil {
		t.Fatal(err)
	}
	fixtures := []struct{ concept, id, at, payload string }{
		{"v1:campaigns:audience", "v1:campaigns:audience:a", "2026-09-01", `{"accountId":"acme"}`},
		{"v1:campaigns:recipient", "v1:campaigns:recipient:r", "2026-09-01", `{"audienceId":"a"}`},
		{"v1:campaigns:recipient", "v1:campaigns:recipient:r", "2026-09-02", `{"audienceId":"a"}`},
		{"v1:campaigns:consentEvent", "v1:campaigns:consentEvent:c", "2026-09-03", `{"recipientId":"v1:campaigns:recipient:r"}`},
		{"v1:campaigns:recipient", "v1:campaigns:recipient:explicit", "2026-09-02", `{"audienceId":"a","accountId":"beta"}`},
		{"v1:campaigns:recipient", "v1:campaigns:recipient:orphan", "2026-09-02", `{"audienceId":"missing"}`},
		{"v1:campaigns:campaign", "v1:campaigns:campaign:untied", "2026-09-02", `{"name":"Do not guess"}`},
		{"v1:identity:group", "v1:identity:group:acct-acme", "2026-09-01", `{"accountId":"acme"}`},
		{"v1:identity:groupMembership", "v1:identity:groupMembership:m", "2026-09-02", `{"groupId":"acct-acme","userId":"u"}`},
		{"v1:other:recipient", "v1:other:recipient:x", "2026-09-02", `{"audienceId":"a"}`},
		{"v1:campaigns:emailRule", "v1:campaigns:emailRule:rule", "2026-09-01", `{"accountId":"acme"}`},
		{"v1:campaigns:delivery", "v1:campaigns:delivery:rule-send", "2026-09-01", `{"campaignId":"rule","emailRuleId":"rule"}`},
		{"v1:campaigns:delivery", "v1:campaigns:delivery:rule-send", "2026-09-02", `{"campaignId":"v1:campaigns:campaign:rule","emailRuleId":"rule"}`},
		{"v1:campaigns:delivery", "v1:campaigns:delivery:missing-rule", "2026-09-02", `{"campaignId":"unknown","emailRuleId":"unknown"}`},
		{"v1:campaigns:campaign", "v1:campaigns:campaign:ambiguous", "2026-09-01", `{"accountId":"beta"}`},
		{"v1:campaigns:emailRule", "v1:campaigns:emailRule:ambiguous", "2026-09-01", `{"accountId":"acme"}`},
		{"v1:campaigns:delivery", "v1:campaigns:delivery:ambiguous", "2026-09-02", `{"campaignId":"ambiguous","emailRuleId":"ambiguous"}`},
	}
	for _, f := range fixtures {
		if _, err := tx.ExecContext(ctx, `INSERT INTO "MemoryNodes" VALUES ($1,$2,$3,$4)`, f.concept, f.id, f.at, f.payload); err != nil {
			t.Fatal(err)
		}
	}
	migration, err := os.ReadFile("migrations/20260922180000_organization_child_attribution.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		if _, err := tx.ExecContext(ctx, string(migration)); err != nil {
			t.Fatal(err)
		}
		for _, want := range []struct{ id, at, campaign string }{
			{"v1:campaigns:delivery:rule-send", "2026-09-01", "rule"},
			{"v1:campaigns:delivery:rule-send", "2026-09-02", ""},
			{"v1:campaigns:delivery:missing-rule", "2026-09-02", "unknown"},
			{"v1:campaigns:delivery:ambiguous", "2026-09-02", "ambiguous"},
		} {
			var got string
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(payload->>'campaignId','') FROM "MemoryNodes" WHERE id=$1 AND "createdAt"=$2`, want.id, want.at).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want.campaign {
				t.Fatalf("campaign relationship %s/%s = %q, want %q", want.id, want.at, got, want.campaign)
			}
		}
		for _, want := range []struct{ id, at, account string }{
			{"v1:campaigns:recipient:r", "2026-09-01", ""},
			{"v1:campaigns:delivery:rule-send", "2026-09-01", ""},
			{"v1:campaigns:delivery:rule-send", "2026-09-02", "acme"},
			{"v1:campaigns:delivery:ambiguous", "2026-09-02", ""},
			{"v1:campaigns:recipient:r", "2026-09-02", "acme"},
			{"v1:campaigns:consentEvent:c", "2026-09-03", "acme"},
			{"v1:campaigns:recipient:explicit", "2026-09-02", "beta"},
			{"v1:campaigns:recipient:orphan", "2026-09-02", ""},
			{"v1:campaigns:campaign:untied", "2026-09-02", ""},
			{"v1:identity:groupMembership:m", "2026-09-02", "acme"},
			{"v1:other:recipient:x", "2026-09-02", ""},
		} {
			var got string
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(payload->>'accountId','') FROM "MemoryNodes" WHERE id=$1 AND "createdAt"=$2`, want.id, want.at).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want.account {
				t.Errorf("pass %d %s/%s got %q want %q", pass, want.id, want.at, got, want.account)
			}
		}
	}
}
