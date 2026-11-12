package memql

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// The two write rules of the platform's configuration rows, held without a
// database: who writes a variable's row (platform_variable_write_guard.go) and
// what a write may put on a source credential
// (platform_source_credential_write_guard.go). The db-gated tests beside them
// drive the same rules through executeWrite; these hold the rules' own tables
// to the tree, so a concept or a field added later is decided the day it is.

// A ROW THE RESOLVERS READ BY NAME IS WRITTEN BY THE DEPLOYMENT'S AUDIENCE.
//
// engine_variables.go resolves a name to the first row carrying it, ahead of
// the environment, for every node. So each concept it reads must have its
// writers decided: a variable concept by the variable write rule, a secret
// concept by the cluster-owner tier. The concepts are read off the resolver's
// own source, so one it starts reading tomorrow is held to this then.
func TestEveryConceptAResolverReadsHasItsWritersDecided(t *testing.T) {
	loadedConceptRegistry(t)

	file, err := parser.ParseFile(token.NewFileSet(), "engine_variables.go", nil, 0)
	if err != nil {
		t.Fatalf("parse engine_variables.go: %v", err)
	}
	read := map[string][]string{} // reader -> the concepts it is called with
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "readVariable" && sel.Sel.Name != "readSecret") {
			return true
		}
		lit, ok := call.Args[1].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Errorf("%s is called with a concept that is not a string literal; this gate reads the literal", sel.Sel.Name)
			return true
		}
		conceptName, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Errorf("%s: unreadable concept literal %s", sel.Sel.Name, lit.Value)
			return true
		}
		read[sel.Sel.Name] = append(read[sel.Sel.Name], conceptName)
		return true
	})

	// POSITIVE CONTROL: both readers were found, with the concepts this file
	// was written against. Fewer means the scan matched nothing.
	if len(read["readVariable"]) < 2 || len(read["readSecret"]) < 2 {
		t.Fatalf("found %d readVariable and %d readSecret calls in engine_variables.go; this gate would measure nothing",
			len(read["readVariable"]), len(read["readSecret"]))
	}

	for _, conceptName := range read["readVariable"] {
		if !platformVariableConcepts[conceptName] {
			t.Errorf("the variable resolver reads %s by name, and platformVariableConcepts does not name it: "+
				"its rows would be written by anyone the data plane admits. Add it to the variable write rule "+
				"(platform_variable_write_guard.go)", conceptName)
		}
		c, err := memoryNodes.Get(conceptName)
		if err != nil || c == nil {
			t.Errorf("%s is not in the loaded registry: %v", conceptName, err)
			continue
		}
		// The rule is write-only on purpose. A tier here would decide reads as
		// well, and these rows are read by callers below every rung of the
		// ladder; whoever adds one must also re-decide every reader, and
		// TestVariableRowsAreStillReadByEveryCaller with it.
		if c.RowAuthz != nil {
			t.Errorf("%s declares %s. Its write rule is platform_variable_write_guard.go's and its reads are "+
				"open by design; a tier decides both, so adding one is a decision about every reader of the row",
				conceptName, rowAuthzTierDescription(c.RowAuthz))
		}
	}
	for _, conceptName := range read["readSecret"] {
		c, err := memoryNodes.Get(conceptName)
		if err != nil || c == nil {
			t.Errorf("%s is not in the loaded registry: %v", conceptName, err)
			continue
		}
		if c.RowAuthz == nil || c.RowAuthz.Tier != langparser.RowAuthzClusterOwner {
			t.Errorf("the secret resolver reads %s by name, and it does not declare @rowAuthz(clusterOwner)", conceptName)
		}
	}
}

// The variable write rule's own table: who it admits and who it refuses, on
// the concepts it names and on none other.
func TestTheVariableWriteRuleAdmitsTheDeploymentsAudience(t *testing.T) {
	as := func(role auth.Role) context.Context {
		return auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "variable-rule-" + string(role), Role: role})
	}
	cases := []struct {
		name    string
		ctx     context.Context
		admits  bool
		because string
	}{
		{"a call with no identity", context.Background(), false, "no caller is not the deployment"},
		{"a viewer", as(auth.RoleReader), false, ""},
		{"a user", as(auth.RoleWriter), false, ""},
		{"an admin", as(auth.RoleAdmin), false, ""},
		{"a developer", as(auth.RoleDeveloper), false, "a developer's saves go through a settings handler"},
		{"a cluster owner", as(auth.RoleOwner), true, "the operator credential's stream, and an owner"},
		{"server code for a developer", auth.ContextWithInternalOrigin(as(auth.RoleDeveloper)), true, "a settings save that authorized its caller"},
		{"server code with no identity", auth.ContextWithInternalOrigin(context.Background()), true, "internal origin is the stamp, not the actor"},
		{"a system actor", auth.ContextWithSystemActor(context.Background(), "variable-rule-test"), true, "the deployment acting"},
	}
	for conceptName := range platformVariableConcepts {
		for _, tc := range cases {
			err := guardPlatformVariableWrite(tc.ctx, conceptName)
			if tc.admits && err != nil {
				t.Errorf("%s: %s was refused (%s): %v", conceptName, tc.name, tc.because, err)
			}
			if !tc.admits {
				if err == nil {
					t.Errorf("%s: %s was admitted", conceptName, tc.name)
				} else if !strings.Contains(err.Error(), conceptName) || !strings.Contains(err.Error(), "configuration variable") {
					t.Errorf("%s: %s's refusal does not name the concept and the rule: %v", conceptName, tc.name, err)
				}
			}
		}
	}
	// The rule is two concepts wide. Everything else is some other rule's.
	for _, other := range []string{memoryNodes.ConceptPlatformGlobalSecret, "v1:todos:todo", ""} {
		if err := guardPlatformVariableWrite(context.Background(), other); err != nil {
			t.Errorf("the variable write rule judged %q: %v", other, err)
		}
	}
}

// EVERY FIELD OF A SOURCE CREDENTIAL HAS A WRITER.
//
// There is no annotation that marks a field server-written against the raw
// literal, so the rule keeps its own list -- and a field added to the concept
// without a line here would be a field a person's own write could set. This
// holds the list to the concept in both directions: a declared field the rule
// does not name fails, and so does a name the concept no longer declares.
func TestEverySourceCredentialFieldHasAWriter(t *testing.T) {
	loadedConceptRegistry(t)
	c, err := memoryNodes.Get(conceptPlatformSourceCredential)
	if err != nil || c == nil {
		t.Fatalf("%s is not in the loaded registry: %v", conceptPlatformSourceCredential, err)
	}
	declared := map[string]bool{}
	for _, field := range c.DeclaredFields() {
		declared[field] = true
	}
	if len(declared) < len(sourceCredentialServerFields) {
		t.Fatalf("%s declares %d fields; the registry did not load the concept this rule was written against",
			conceptPlatformSourceCredential, len(declared))
	}

	decided := map[string]bool{
		sourceCredentialOwnerField:     true,
		sourceCredentialStatusField:    true,
		sourceCredentialRevokedAtField: true,
	}
	for _, field := range sourceCredentialServerFields {
		if decided[field] {
			t.Errorf("`%s` is listed twice in the source credential rule", field)
		}
		decided[field] = true
	}

	var undecided, stale []string
	for field := range declared {
		if !decided[field] {
			undecided = append(undecided, field)
		}
	}
	for field := range decided {
		if !declared[field] {
			stale = append(stale, field)
		}
	}
	sort.Strings(undecided)
	sort.Strings(stale)
	if len(undecided) > 0 {
		t.Errorf("%s declares field(s) the write rule does not name: %s. Until one is named, a person's own "+
			"raw write may set it. Add it to sourceCredentialServerFields "+
			"(platform_source_credential_write_guard.go), or give it a rule of its own there",
			conceptPlatformSourceCredential, strings.Join(undecided, ", "))
	}
	if len(stale) > 0 {
		t.Errorf("the source credential rule names field(s) %s no longer declares: %s",
			conceptPlatformSourceCredential, strings.Join(stale, ", "))
	}
	// Every sealed field is among the server-written ones, whatever else moves.
	for _, sealed := range c.SecretFields() {
		found := false
		for _, field := range sourceCredentialServerFields {
			found = found || field == sealed
		}
		if !found {
			t.Errorf("`%s` is declared @secret and is not among the server-written fields", sealed)
		}
	}
}

// The source credential rule's own table, on rows as executeWrite hands them
// over: the stored row, and the row about to be written.
func TestTheSourceCredentialRuleAdmitsOnlyARevocation(t *testing.T) {
	stored := func() map[string]any {
		return map[string]any{
			"ownerUserId": "v1:identity:user:ada", "kind": "github_app", "host": "github.com", "label": "GitHub",
			"encryptedValue": "sealed", "fingerprint": "...aled", "status": "active",
			"login": "octocat", "externalId": "583231", "refreshToken": "sealed-refresh",
			"expiresAt": "2030-01-01T00:00:00Z", "installationIds": []any{"1001"},
		}
	}
	with := func(changes map[string]any) map[string]any {
		row := stored()
		for k, v := range changes {
			row[k] = v
		}
		return row
	}
	person := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "ada", Role: auth.RoleWriter})
	owner := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "root", Role: auth.RoleOwner})
	server := auth.ContextWithInternalOrigin(person)
	revokedRow := with(map[string]any{"status": "revoked", "revokedAt": "2030-02-02T00:00:00Z"})

	cases := []struct {
		name         string
		ctx          context.Context
		prior, final map[string]any
		refusedOn    string // the field the refusal names; "" when admitted
	}{
		{"a person's revoke", person, stored(), revokedRow, ""},
		{"a cluster owner's revoke", owner, stored(), revokedRow, ""},
		{"a write that changes nothing", person, stored(), stored(), ""},
		// The raw literal's owner stamp writes the caller's id bare over the
		// stored canonical one; that is one owner, not a move.
		{"a revoke whose owner was restamped bare", person, stored(), with(map[string]any{"ownerUserId": "ada", "status": "revoked"}), ""},
		{"a person's create", person, nil, stored(), "create"},
		{"a cluster owner's create", owner, nil, stored(), "create"},
		{"a new sealed value", person, stored(), with(map[string]any{"encryptedValue": "composed"}), "encryptedValue"},
		{"a new refresh token", person, stored(), with(map[string]any{"refreshToken": "composed"}), "refreshToken"},
		{"another host", person, stored(), with(map[string]any{"host": "example.test"}), "host"},
		{"another kind", person, stored(), with(map[string]any{"kind": "token"}), "kind"},
		{"more installations", person, stored(), with(map[string]any{"installationIds": []any{"1001", "2002"}}), "installationIds"},
		{"installations emptied", person, stored(), with(map[string]any{"installationIds": []any{}}), "installationIds"},
		{"a field cleared", person, stored(), with(map[string]any{"refreshToken": ""}), "refreshToken"},
		{"a cluster owner's new sealed value", owner, stored(), with(map[string]any{"encryptedValue": "composed"}), "encryptedValue"},
		{"a cluster owner's move to another person", owner, stored(), with(map[string]any{"ownerUserId": "grace"}), "ownerUserId"},
		{"a reactivation", person, revokedRow, with(map[string]any{"status": "active", "revokedAt": "2030-02-02T00:00:00Z"}), "status"},
		{"a status that is not a revocation", person, stored(), with(map[string]any{"status": "paused"}), "status"},
		{"a revocation time on an active row", person, stored(), with(map[string]any{"revokedAt": "2030-02-02T00:00:00Z"}), "revokedAt"},
		{"a revoke that also renames", person, stored(), with(map[string]any{"status": "revoked", "label": "renamed"}), "label"},
		// Server code is the writer of all of it.
		{"server code creates", server, nil, stored(), ""},
		{"server code refreshes", server, stored(), with(map[string]any{"encryptedValue": "resealed", "refreshToken": "resealed"}), ""},
		{"server code reconnects a revoked grant", server, revokedRow, with(map[string]any{"status": "active", "encryptedValue": "resealed"}), ""},
	}
	for _, tc := range cases {
		err := validateSourceCredentialServerOnly(tc.ctx, tc.prior, tc.final)
		switch {
		case tc.refusedOn == "" && err != nil:
			t.Errorf("%s was refused: %v", tc.name, err)
		case tc.refusedOn != "" && err == nil:
			t.Errorf("%s was admitted", tc.name)
		case tc.refusedOn != "" && (!strings.Contains(err.Error(), tc.refusedOn) || !strings.Contains(err.Error(), "server-written")):
			t.Errorf("%s: the refusal does not name `%s` and the rule: %v", tc.name, tc.refusedOn, err)
		}
	}
}
