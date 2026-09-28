package conformance

// router_structured_ledger_db_test.go -- a STRUCTURED call's decision row,
// written by the real router's ledger writer into a real database.
//
// The structured surface (goal triage, the classifiers, the answer validator)
// had no observer, so its calls never reached v1:router:call, which is what
// Fleet History reads. component/router's own tests pin the row the observer
// RENDERS against a fake engine; this pins that the row LANDS -- a rendered
// row the mutation refuses is logged once and dropped, and every package test
// stays green (the memql "missing rows: run the write for real" rule).
//
// It drives the case the owner decided on 2026-09-28: a scheduled automation
// (a system actor, no owner) whose route puts Claude Code first. The app door
// is passed with its reason, the vendor behind it serves, and the row keeps
// both facts.
//
// Green-by-skip warning: without a database this skips. To verify for real:
//
//	MEMQL_DATABASE_DSN=... MEMQL_REQUIRE_DB=1 go test -count=1 \
//	  -run 'TestAStructuredCallsDecisionRowLands' ./test/conformance/

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/router"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// openClaudeCode is an app door open to anyone who asks -- so only the owner
// rule can keep a system actor away from it.
type openClaudeCode struct{ calls int }

func (o *openClaudeCode) Doors(context.Context, string) ([]memql.AppDoor, error) {
	return []memql.AppDoor{{AppId: "claude-code", Machines: []memql.AppMachine{{
		RegistrationId: "laptop", Name: "laptop", Online: true, LocalStream: true, StructuredResult: true, FollowUps: true,
	}}}}, nil
}
func (o *openClaudeCode) AppOrder(context.Context, string) ([]string, error) { return nil, nil }
func (o *openClaudeCode) Call(context.Context, memql.AppCallRequest) (memql.AppCallResult, error) {
	o.calls++
	return memql.AppCallResult{Content: `{"ok":true}`}, nil
}

// structuredVendor serves the structured call the app door passed.
type structuredVendor struct{}

func (structuredVendor) Call(context.Context, string) (any, error) { return "", nil }
func (structuredVendor) CallChatStructured(context.Context, []common.ChatMessage, common.StructuredSchema) (string, error) {
	return `{"ok":true}`, nil
}

func TestAStructuredCallsDecisionRowLandsWithTheAppDoorItPassed(t *testing.T) {
	env := newEnv(t)
	if !env.HasDB {
		if os.Getenv("MEMQL_REQUIRE_DB") == "1" {
			t.Fatal("the router ledger needs Postgres, and MEMQL_REQUIRE_DB=1 makes its absence a failure")
		}
		t.Skip("the router ledger needs Postgres (MEMQL_DATABASE_DSN)")
	}

	providers := memql.NewProviderRegistryForTest()
	apps := &openClaudeCode{}
	providers.SetAppInference(apps)
	providers.RegisterWithParamsForTest("streamClaudeSonnet", "AnthropicStream", "claude-sonnet",
		map[string]any{"contextWindow": 200000}, structuredVendor{})
	policies := memql.NewPolicyRegistryForTest(map[string][]string{
		"appFirst": {memql.AppReferencePrefix + "claude-code", "streamClaudeSonnet"},
	})
	rules := memql.NewRuleRegistry()
	if err := rules.Register(&memql.RuleConfig{
		Name: memql.DefaultRuleName, When: memql.RuleWhen{Present: map[string]bool{}}, Policy: "appFirst",
		OnUnavailable: memql.OnUnavailableDegrade, Locked: true, SourceFile: "dsl/rules/rules.memql",
	}); err != nil {
		t.Fatal(err)
	}
	if err := rules.Finalize(); err != nil {
		t.Fatal(err)
	}
	r := router.New(providers, policies, rules, env.Eng, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	// A prompt name unique to this run is the row's handle: the ledger mints
	// its own call id, and the database is shared.
	promptName := fmt.Sprintf("conformanceStructuredLedger%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = env.DB.NewDelete().Model((*memoryNodes.MemoryNode)(nil)).
			Where("concept = ? AND payload ->> 'promptName' = ?", "v1:router:call", promptName).
			Exec(context.Background())
	})

	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "system:automation:workerAppSessionStaleSweep", Role: auth.RoleReader, Unranked: true, Synthetic: true,
	})
	resolved, err := r.ResolveFor(ctx, router.ResolveRequest{
		Level: airoute.LevelFast, Modality: airoute.ModalityStructured, PromptName: promptName,
		Needs: airoute.Needs{MinContextTokens: 8000, Structured: true},
	})
	if err != nil {
		t.Fatalf("an ownerless structured call was failed at the app door: %v", err)
	}
	if _, err := resolved.Client.(common.ChatStructuredProvider).CallChatStructured(ctx,
		[]common.ChatMessage{{Role: "user", Content: "classify"}}, common.StructuredSchema{Name: "s"}); err != nil {
		t.Fatal(err)
	}
	if apps.calls != 0 {
		t.Fatalf("the app was called %d time(s) for a system actor", apps.calls)
	}

	// The ledger writes on its own goroutine; wait for the row.
	var payload map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var nodes []memoryNodes.MemoryNode
		if err := env.DB.NewSelect().Model(&nodes).
			Where("concept = ? AND payload ->> 'promptName' = ?", "v1:router:call", promptName).
			Scan(context.Background()); err != nil {
			t.Fatalf("read the ledger: %v", err)
		}
		if len(nodes) > 0 {
			if err := json.Unmarshal(nodes[len(nodes)-1].Payload, &payload); err != nil {
				t.Fatalf("decode the ledger row: %v", err)
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if payload == nil {
		t.Fatalf("no v1:router:call row landed for the structured call (dropped writes: %d)", r.RecordsDropped())
	}

	if payload["outcome"] != "ok" || payload["providerName"] != "streamClaudeSonnet" || payload["door"] != "federation" {
		t.Errorf("row = outcome %v provider %v door %v, want ok / streamClaudeSonnet / federation",
			payload["outcome"], payload["providerName"], payload["door"])
	}
	if payload["callerKind"] != auth.CallerKindSystem {
		t.Errorf("callerKind = %v, want %q", payload["callerKind"], auth.CallerKindSystem)
	}
	considered, _ := payload["considered"].([]any)
	var appReason string
	for _, raw := range considered {
		if m, _ := raw.(map[string]any); m["entry"] == "app:claude-code" {
			appReason, _ = m["reason"].(string)
		}
	}
	if !strings.Contains(appReason, memql.AppNoOwnerReason) {
		t.Errorf("the stored row's considered = %v, want app:claude-code passed with %q", considered, memql.AppNoOwnerReason)
	}
}
