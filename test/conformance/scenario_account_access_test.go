package conformance

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// Account setup must exercise the current owner, revision and app-ceiling
// checks. Raw grants used to suffice for the campaigns fixture, but bypassing
// the governed writer would hide the account hierarchy that production uses.
// This seed permits only the two setup operations that fixture needs. It reads
// a fresh revision and stops on any refusal; it never repairs policy or retries
// a failed change with broader authority.
func validateScenarioAccountAccessSeed(seed scenarioSeed) error {
	if seed.Builtin != "accountAccessUpdate" || seed.Mutation != "" {
		return fmt.Errorf("a builtin seed names only accountAccessUpdate, without a mutation")
	}
	if seed.As == nil || seed.As.Role != string(auth.RoleOwner) || !scenarioTaggedID(seed.As.UserID) || !scenarioTaggedID(seed.Args["accountId"]) {
		return fmt.Errorf("account-access setup names its tagged account and explicit tagged owner")
	}
	allowed := map[string]bool{"accountId": true, "operation": true, "apps": true}
	switch seed.Args["operation"] {
	case "apps":
	case "members":
		allowed["userIds"], allowed["role"] = true, true
		users, ok := seed.Args["userIds"].([]any)
		if !ok || len(users) == 0 || len(users) > 8 || seed.Args["role"] != "member" {
			return fmt.Errorf("account-access setup assigns at most eight tagged members")
		}
		for _, user := range users {
			if !scenarioTaggedID(user) {
				return fmt.Errorf("account-access members must use tagged fixture ids")
			}
		}
	default:
		return fmt.Errorf("account-access setup permits only apps and members")
	}
	for key := range seed.Args {
		if !allowed[key] {
			return fmt.Errorf("account-access setup does not accept %q", key)
		}
	}
	apps, ok := seed.Args["apps"].([]any)
	if !ok || len(apps) == 0 || len(apps) > 8 {
		return fmt.Errorf("account-access setup names at most eight apps")
	}
	for _, app := range apps {
		if name, ok := app.(string); !ok || !strings.HasPrefix(name, "app:") {
			return fmt.Errorf("account-access setup names app resources")
		}
	}
	return nil
}

func scenarioTaggedID(value any) bool {
	object, ok := value.(map[string]any)
	name, _ := object["$id"].(string)
	return ok && len(object) == 1 && strings.TrimSpace(name) != ""
}

func (v *variant) seedAccountAccess(label string, seed scenarioSeed) {
	v.t.Helper()
	if err := validateScenarioAccountAccessSeed(seed); err != nil {
		v.t.Fatalf("%s: %v", label, err)
	}
	user := v.resolve(seed.As.UserID).(string)
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: user, Role: auth.RoleOwner})
	ctx = auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: user})
	args := v.resolve(seed.Args).(map[string]any)
	account := args["accountId"].(string)
	call := func(name string, args map[string]any) map[string]any {
		v.t.Helper()
		keys := make([]string, 0, len(args))
		for key := range args {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, key+": "+renderScenarioArg(v.t, args[key]))
		}
		result, err := v.rig.env.Eng.Execute(ctx, "builtin "+name+"("+strings.Join(parts, ", ")+")")
		if err != nil {
			v.t.Fatalf("%s, %s: %v", label, name, err)
		}
		rows := memql.MaterializeRows(result)
		if len(rows) != 1 {
			v.t.Fatalf("%s, %s returned %d rows, want one account roster", label, name, len(rows))
		}
		if payload, ok := rows[0]["payload"].(map[string]any); ok {
			return payload
		}
		return rows[0]
	}
	state := call("accountAccess", map[string]any{"accountId": account})
	revision, ok := state["revision"].(string)
	if !ok {
		v.t.Fatalf("%s: accountAccess returned no revision", label)
	}
	args["revision"] = revision
	// The governed writer makes deterministic membership ids rather than
	// returning graph rows. Register only this fixture's tagged rows for the
	// same cleanup and snapshot mechanism used by ordinary mutation seeds.
	v.seeded = append(v.seeded, [2]string{"v1:accounts:account", account})
	if args["operation"] == "members" {
		for _, member := range args["userIds"].([]any) {
			v.seeded = append(v.seeded, [2]string{"v1:identity:groupMembership", "acct-" + account + "-" + member.(string)})
		}
	}
	call(seed.Builtin, args)
}

func TestScenarioAccountAccessSeedIsBounded(t *testing.T) {
	valid := func() scenarioSeed {
		return scenarioSeed{Builtin: "accountAccessUpdate", As: &scenarioActor{UserID: map[string]any{"$id": "owner"}, Role: "owner"}, Args: map[string]any{"accountId": map[string]any{"$id": "account"}, "operation": "apps", "apps": []any{"app:email"}}}
	}
	for name, mutate := range map[string]func(*scenarioSeed){
		"another builtin":    func(s *scenarioSeed) { s.Builtin = "runQuery" },
		"mixed writer":       func(s *scenarioSeed) { s.Mutation = "createUser" },
		"ownership transfer": func(s *scenarioSeed) { s.Args["operation"] = "transfer" },
		"supplied revision":  func(s *scenarioSeed) { s.Args["revision"] = "stale" },
		"untagged account":   func(s *scenarioSeed) { s.Args["accountId"] = "self" },
		"implicit actor":     func(s *scenarioSeed) { s.As = nil },
		"untagged member": func(s *scenarioSeed) {
			s.Args["operation"], s.Args["role"], s.Args["userIds"] = "members", "member", []any{"existing-user"}
		},
		"owner member": func(s *scenarioSeed) {
			s.Args["operation"], s.Args["role"], s.Args["userIds"] = "members", "owner", []any{map[string]any{"$id": "member"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			seed := valid()
			mutate(&seed)
			if err := validateScenarioAccountAccessSeed(seed); err == nil {
				t.Fatal("unbounded account setup was accepted")
			}
		})
	}
	seed := valid()
	if err := validateScenarioAccountAccessSeed(seed); err != nil {
		t.Fatal(err)
	}
	seed.Args["operation"], seed.Args["role"], seed.Args["userIds"] = "members", "member", []any{map[string]any{"$id": "author"}}
	if err := validateScenarioAccountAccessSeed(seed); err != nil {
		t.Fatal(err)
	}
}
