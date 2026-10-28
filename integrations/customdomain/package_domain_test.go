package customdomain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

type packageDomainEngine struct {
	*fakeEngine
	writeCtx context.Context
	writeErr error
}

func (e *packageDomainEngine) Execute(ctx context.Context, query string) (any, error) {
	if strings.HasPrefix(query, "mutation createCustomDomain") {
		e.writeCtx = ctx
		if e.writeErr != nil {
			return nil, e.writeErr
		}
	} else if strings.HasPrefix(query, "query siteById") {
		actor, _ := auth.AccessFromContext(ctx)
		if actor == nil || actor.UserId != "selected-requester" {
			e.t.Fatal("domain selection replaced the caller for the site read")
		}
	}
	return e.fakeEngine.Execute(ctx, query)
}

func TestPackageDomainAddKeepsSelectedIdentityAndNormalCaller(t *testing.T) {
	e := &packageDomainEngine{fakeEngine: &fakeEngine{t: t, siteRows: []map[string]any{{"id": "site", "accountId": "account"}}}}
	i := &Integration{store: NewStore(e), resolver: &fakeResolver{}, cfg: Config{EdgeHost: "routing.example.net"}}
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "selected-requester", Role: auth.RoleOwner})
	selected := memql.PackageDomainIntent{Key: "domain", PublicationKey: "site/serving", DomainID: "stable-domain", SiteID: "site", AccountID: "account", Hostname: "client.example.net"}
	ctx = memql.ContextWithPackageDomainFence(ctx, "run", strings.Repeat("1", 64), "lease", selected)
	nodes, err := i.handleAdd(ctx, map[string]any{"siteId": "site", "hostname": selected.Hostname}, 0)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("selected domain: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &result); err != nil {
		t.Fatal(err)
	}
	if result["domainId"] != selected.DomainID || result["token"] == "" || len(e.mutations()) != 1 {
		t.Fatalf("ordinary domain creation lost its identity/token: %+v", result)
	}
	got, carried := memql.PackageDomainIntentFromContext(e.writeCtx)
	actor, _ := auth.AccessFromContext(e.writeCtx)
	if !carried || got != selected || actor == nil || actor.UserId != systemCustomDomainActor {
		t.Fatal("ordinary internal writer lost the native restriction")
	}
}

func TestPackageDomainAddRefusesRetargetingAndReturnsRecordedOutcome(t *testing.T) {
	for _, fault := range []string{"site", "account", "hostname", "unreadable", "recorded"} {
		t.Run(fault, func(t *testing.T) {
			e := &packageDomainEngine{fakeEngine: &fakeEngine{t: t, siteRows: []map[string]any{{"id": "site", "accountId": "account"}}}}
			i := &Integration{store: NewStore(e), resolver: &fakeResolver{}, cfg: Config{EdgeHost: "routing.example.net"}}
			selected := memql.PackageDomainIntent{Key: "domain", PublicationKey: "site/serving", DomainID: "stable-domain", SiteID: "site", AccountID: "account", Hostname: "client.example.net"}
			switch fault {
			case "site":
				selected.SiteID = "foreign"
			case "account":
				selected.AccountID = "foreign"
			case "hostname":
				selected.Hostname = "foreign.example.net"
			case "unreadable":
				e.siteRows = nil
			case "recorded":
				e.writeErr = memql.ErrPackageDomainRecorded
			}
			ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "selected-requester", Role: auth.RoleOwner})
			ctx = memql.ContextWithPackageDomainFence(ctx, "run", strings.Repeat("1", 64), "lease", selected)
			nodes, err := i.handleAdd(ctx, map[string]any{"siteId": "site", "hostname": "client.example.net"}, 0)
			if err == nil || len(nodes) != 0 {
				t.Fatal("refused or observed result returned a new verification token")
			}
			if fault == "recorded" {
				if !errors.Is(err, memql.ErrPackageDomainRecorded) {
					t.Fatalf("lost exact recorded-outcome marker: %v", err)
				}
			} else if e.writeCtx != nil {
				t.Fatal("retargeted or unreadable selection reached a write")
			}
		})
	}
}
