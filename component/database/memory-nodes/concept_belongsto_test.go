package memoryNodes

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/annotations"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// belongsToCode returns the @belongsTo rule code err carries anywhere on its
// chain, or "". It requires the code to be the LAST bracketed word of the text
// as well as reachable as a *BelongsToError: a wrapper that dropped the cause,
// or one that appended after the code, would leave a load report unable to key
// on the rule, and both must fail here rather than in the corpus.
func belongsToCode(err error) string {
	var refusal *langparser.BelongsToError
	if err == nil || !errors.As(err, &refusal) {
		return ""
	}
	if !strings.HasSuffix(err.Error(), " ["+refusal.Code+"]") {
		return ""
	}
	return refusal.Code
}

// The half of a declaration only the property list can answer: an account form
// records the account in a string field named `accountId`, a `via` names a
// field the concept declares, and a concept that shares its rows with an
// account is not cluster-wide.
func TestConceptRefusesABelongsToItsFieldsContradict(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		wantCode string
		wantText []string
	}{
		{
			name:     "an account form with no accountId field",
			src:      "@belongsTo(account)\nconcept probe {\n  title  string\n}\n",
			wantCode: langparser.CodeBelongsToField,
			wantText: []string{"@belongsTo(account)", "`accountId`", "declares none", "declared: title", "accountId string"},
		},
		{
			name:     "an accountId that is not a string",
			src:      "@belongsTo(account)\nconcept probe {\n  accountId  int\n}\n",
			wantCode: langparser.CodeBelongsToField,
			wantText: []string{"@belongsTo(account)", "declares as int", "accountId string"},
		},
		{
			// The list is the OTHER field: a concept that still takes several
			// accounts keeps `accountIds` and gains the single field beside it.
			name:     "an accountId that is a list",
			src:      "@belongsTo(account)\nconcept probe {\n  accountId  []string\n}\n",
			wantCode: langparser.CodeBelongsToField,
			wantText: []string{"declares as []string", "accountId string"},
		},
		{
			// A stated concept's own writers fill the field, so the field has to
			// be there for them to fill: every reader of the declaration reads
			// `accountId`, whoever wrote it.
			name:     "a stated concept with no accountId field",
			src:      "@belongsTo(account, stated)\nconcept probe {\n  title  string\n}\n",
			wantCode: langparser.CodeBelongsToField,
			wantText: []string{"@belongsTo(account, stated)", "declares none", "accountId string"},
		},
		{
			name:     "a stated concept whose accountId is not a string",
			src:      "@belongsTo(account, stated)\nconcept probe {\n  accountId  object\n}\n",
			wantCode: langparser.CodeBelongsToField,
			wantText: []string{"@belongsTo(account, stated)", "declares as object"},
		},
		{
			name:     "a shared concept with no accountId field",
			src:      "@belongsTo(account, shared)\nconcept probe {\n  title  string\n}\n",
			wantCode: langparser.CodeBelongsToField,
			wantText: []string{"@belongsTo(account, shared)", "declares none"},
		},
		{
			name:     "a via naming a field the concept does not declare",
			src:      "@belongsTo(account, via=\"goalId\")\nconcept probe {\n  accountId  string\n}\n",
			wantCode: langparser.CodeBelongsToViaUnknown,
			wantText: []string{`@belongsTo(account, via="goalId")`, "does not declare", "declared: accountId"},
		},
		{
			// The account field is checked first: a concept missing both is told
			// about the field every account form needs before the one this form
			// adds, so fixing the first refusal never uncovers a second about the
			// same line.
			name:     "a via concept with no accountId field",
			src:      "@belongsTo(account, via=\"goalId\")\nconcept probe {\n  goalId  string\n}\n",
			wantCode: langparser.CodeBelongsToField,
			wantText: []string{`@belongsTo(account, via="goalId")`, "declares none"},
		},
		{
			// `via` is a field of THIS row. A mis-cased name is not that field.
			name:     "a via whose case does not match the declared field",
			src:      "@belongsTo(account, via=\"goalID\")\nconcept probe {\n  accountId  string\n  goalId     string\n}\n",
			wantCode: langparser.CodeBelongsToViaUnknown,
			wantText: []string{"declared: accountId, goalId"},
		},
		{
			name: "cluster-wide beside the account grant",
			src: "@belongsTo(cluster)\n@rowAuthz(owner=\"ownerUserId\", account=\"accountId\")\n" +
				"concept probe {\n  ownerUserId  string\n  accountId    string\n}\n",
			wantCode: langparser.CodeBelongsToSharingMismatch,
			wantText: []string{"@belongsTo(cluster)", `account="accountId"`, "@belongsTo(account)"},
		},
		{
			// The grant reads a list too (`account="accountIds"`), and a list of
			// accounts is no more cluster-wide than one.
			name: "cluster-wide beside the account grant over a list",
			src: "@belongsTo(cluster)\n@rowAuthz(owner=\"ownerUserId\", account=\"accountIds\")\n" +
				"concept probe {\n  ownerUserId  string\n  accountIds   []string\n}\n",
			wantCode: langparser.CodeBelongsToSharingMismatch,
			wantText: []string{`account="accountIds"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := buildConcept(t, tc.src)
			if err == nil {
				t.Fatalf("BuildConceptFromDecl built %+v, want a refusal:\n%s", c.BelongsTo, tc.src)
			}
			if got := belongsToCode(err); got != tc.wantCode {
				t.Fatalf("code = %q, want %q\nerror: %v", got, tc.wantCode, err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "v1:test:probe") {
				t.Errorf("the refusal does not name the concept: %s", msg)
			}
			for _, want := range tc.wantText {
				if !strings.Contains(msg, want) {
					t.Errorf("the refusal must say %q: %s", want, msg)
				}
			}
		})
	}
}

// What ParseBelongsTo refuses reaches an author through the build with its code
// intact and the concept named, since ParseBelongsTo itself never knows which
// concept it is reading for.
func TestConceptCarriesTheParsersBelongsToRefusals(t *testing.T) {
	cases := []struct {
		annotation string
		wantCode   string
	}{
		{`@belongsTo(account, cluster)`, langparser.CodeBelongsToConflict},
		{`@belongsTo(shared)`, langparser.CodeBelongsToConflict},
		{`@belongsTo(cluster, stated)`, langparser.CodeBelongsToConflict},
		{`@belongsTo(account, stated, via="goalId")`, langparser.CodeBelongsToConflict},
		{`@belongsTo(account, parent="v1:x:y")`, langparser.CodeBelongsToConflict},
		{`@belongsTo(account, via="")`, langparser.CodeBelongsToViaUnknown},
		{`@belongsTo(cluster, stays="signin")`, langparser.CodeBelongsToConflict},
		{`@belongsTo(account, stays="forever")`, langparser.CodeBelongsToStaysUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.annotation, func(t *testing.T) {
			_, err := buildConcept(t, tc.annotation+"\nconcept probe {\n  accountId  string\n  goalId     string\n}\n")
			if got := belongsToCode(err); got != tc.wantCode {
				t.Fatalf("code = %q, want %q\nerror: %v", got, tc.wantCode, err)
			}
			if !strings.Contains(err.Error(), "v1:test:probe") {
				t.Errorf("the refusal does not name the concept: %v", err)
			}
		})
	}
}

// Which keys exist, what shape each takes and that the annotation is written
// once are the annotation registry's to refuse, before ParseBelongsTo reads
// what the keys mean. A SECOND declaration is the case worth pinning: the
// attributes are folded in source order, so without the registry's repeat rule
// the last one would win and a reader scanning top-down would see a declaration
// the engine does not use.
func TestConceptLeavesBelongsToSpellingToTheRegistry(t *testing.T) {
	cases := []struct {
		name     string
		preamble string
		wantCode string
		wantText string
	}{
		{"written bare", `@belongsTo`, annotations.CodeForm, "@belongsTo on a concept takes keyword arguments"},
		{"empty parentheses", `@belongsTo()`, annotations.CodeForm, "@belongsTo on a concept takes keyword arguments"},
		{"a positional value", `@belongsTo("account")`, annotations.CodeForm, "@belongsTo on a concept takes keyword arguments"},
		{"a misspelled key", `@belongsTo(acount)`, annotations.CodeKey, "has no key acount -- did you mean account?"},
		{"a flag given a value", `@belongsTo(account="yes")`, annotations.CodeKey, "account is a flag and takes no value"},
		{"a valued key written bare", `@belongsTo(account, via)`, annotations.CodeKey, "via takes a value"},
		{"declared twice", "@belongsTo(cluster)\n@belongsTo(account)", annotations.CodeRepeated, "@belongsTo is written more than once on a concept"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildConcept(t, tc.preamble+"\nconcept probe {\n  accountId  string\n}\n")
			var refusal *annotations.Refusal
			if err == nil || !errors.As(err, &refusal) {
				t.Fatalf("want a registry refusal, got %v", err)
			}
			if refusal.Code != tc.wantCode {
				t.Errorf("code = %q, want %q\nerror: %v", refusal.Code, tc.wantCode, err)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantText)
			}
			if code := belongsToCode(err); code != "" {
				t.Errorf("a spelling refusal also carries the belongs-to code %q; one refusal has one code", code)
			}
		})
	}
}

// Every declaration that builds lands on the Concept with the meaning it was
// written with, and the four accessors answer from it. An UNDECLARED concept is
// the row to read twice: it is not cluster-wide, it is undeclared, and every
// accessor says no -- ClusterWide included, because the accessor reports what
// the concept declared and "nothing" is a different statement.
func TestBelongsToAnswersWhatAConceptsRowsBelongTo(t *testing.T) {
	const fields = "concept probe {\n  accountId  string\n  goalId     string\n  title      string\n}\n"
	cases := []struct {
		name          string
		preamble      string
		want          *langparser.BelongsToDecl
		accountScoped bool
		clusterWide   bool
		sharedRows    bool
		engineManaged bool
	}{
		{name: "undeclared", preamble: ""},
		{
			name:        "cluster-wide",
			preamble:    "@belongsTo(cluster)\n",
			want:        &langparser.BelongsToDecl{Scope: langparser.BelongsToCluster},
			clusterWide: true,
		},
		{
			name:          "an account",
			preamble:      "@belongsTo(account)\n",
			want:          &langparser.BelongsToDecl{Scope: langparser.BelongsToAccount},
			accountScoped: true,
			engineManaged: true,
		},
		{
			name:          "the account of a parent row",
			preamble:      "@belongsTo(account, via=\"goalId\")\n",
			want:          &langparser.BelongsToDecl{Scope: langparser.BelongsToAccount, Via: "goalId"},
			accountScoped: true,
			engineManaged: true,
		},
		{
			name:          "the account of a parent row of a named concept",
			preamble:      "@belongsTo(account, via=\"goalId\", parent=\"v1:work:goal\")\n",
			want:          &langparser.BelongsToDecl{Scope: langparser.BelongsToAccount, Via: "goalId", Parent: "v1:work:goal"},
			accountScoped: true,
			engineManaged: true,
		},
		{
			name:          "an account, with cluster-provided rows beside them",
			preamble:      "@belongsTo(account, shared)\n",
			want:          &langparser.BelongsToDecl{Scope: langparser.BelongsToAccount, Shared: true},
			accountScoped: true,
			sharedRows:    true,
			engineManaged: true,
		},
		{
			name:          "an account its own writers state",
			preamble:      "@belongsTo(account, stated)\n",
			want:          &langparser.BelongsToDecl{Scope: langparser.BelongsToAccount, Stated: true},
			accountScoped: true,
		},
		{
			name:          "shared, and stated by its own writers",
			preamble:      "@belongsTo(account, shared, stated)\n",
			want:          &langparser.BelongsToDecl{Scope: langparser.BelongsToAccount, Shared: true, Stated: true},
			accountScoped: true,
			sharedRows:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := buildConcept(t, tc.preamble+fields)
			if err != nil {
				t.Fatalf("BuildConceptFromDecl: %v", err)
			}
			switch {
			case tc.want == nil && c.BelongsTo != nil:
				t.Fatalf("BelongsTo = %+v, want nil for an undeclared concept", *c.BelongsTo)
			case tc.want != nil && c.BelongsTo == nil:
				t.Fatalf("BelongsTo is nil, want %+v", *tc.want)
			case tc.want != nil && *c.BelongsTo != *tc.want:
				t.Fatalf("BelongsTo = %+v, want %+v", *c.BelongsTo, *tc.want)
			}
			if got := c.AccountScoped(); got != tc.accountScoped {
				t.Errorf("AccountScoped() = %v, want %v", got, tc.accountScoped)
			}
			if got := c.ClusterWide(); got != tc.clusterWide {
				t.Errorf("ClusterWide() = %v, want %v", got, tc.clusterWide)
			}
			if got := c.SharedRows(); got != tc.sharedRows {
				t.Errorf("SharedRows() = %v, want %v", got, tc.sharedRows)
			}
			if got := c.AccountEngineManaged(); got != tc.engineManaged {
				t.Errorf("AccountEngineManaged() = %v, want %v", got, tc.engineManaged)
			}
		})
	}
}

// A nil *Concept answers every question no. The write path asks these of
// whatever the registry handed back, and a concept that is not registered must
// read as "nothing to stamp" rather than as a nil dereference in executeWrite.
func TestBelongsToAccessorsAreNilSafe(t *testing.T) {
	var c *Concept
	if c.AccountScoped() || c.ClusterWide() || c.SharedRows() || c.AccountEngineManaged() {
		t.Fatalf("a nil concept answered yes: scoped=%v cluster=%v shared=%v managed=%v",
			c.AccountScoped(), c.ClusterWide(), c.SharedRows(), c.AccountEngineManaged())
	}
}

// A cluster-wide concept may carry a field named `accountId` that means
// something else entirely (an entitlement's `accountId` is a user id). The
// declaration is what makes the field the row's account, so cluster-wide beside
// such a field builds, whatever the field's type.
func TestBelongsToClusterMayCarryAnUnrelatedAccountIdField(t *testing.T) {
	for _, fieldType := range []string{"string", "int", "[]string", "object"} {
		c, err := buildConcept(t, "@belongsTo(cluster)\nconcept probe {\n  accountId  "+fieldType+"\n}\n")
		if err != nil {
			t.Fatalf("accountId %s on a cluster-wide concept: %v", fieldType, err)
		}
		if !c.ClusterWide() || c.AccountScoped() {
			t.Fatalf("accountId %s: ClusterWide=%v AccountScoped=%v, want cluster-wide", fieldType, c.ClusterWide(), c.AccountScoped())
		}
	}
}

// The account grant and an account form agree with each other in every
// combination, and a concept that shares through the LIST keeps doing so while
// it records the single field.
func TestBelongsToAccountFormsBuildBesideTheAccountGrant(t *testing.T) {
	sources := []string{
		"@belongsTo(account)\n@rowAuthz(owner=\"ownerUserId\", account=\"accountId\")\n" +
			"concept probe {\n  ownerUserId  string\n  accountId    string\n}\n",
		"@belongsTo(account, stated)\n@rowAuthz(owner=\"ownerUserId\", account=\"accountId\")\n" +
			"concept probe {\n  ownerUserId  string\n  accountId    string\n}\n",
		"@belongsTo(account, stated)\n@rowAuthz(owner=\"ownerUserId\", account=\"accountIds\")\n" +
			"concept probe {\n  ownerUserId  string\n  accountId    string\n  accountIds   []string\n}\n",
		// Cluster-wide beside a tier that grants NO account is not a mismatch.
		"@belongsTo(cluster)\n@rowAuthz(owner=\"ownerUserId\", clusterOwner)\n" +
			"concept probe {\n  ownerUserId  string\n}\n",
		"@belongsTo(cluster)\n@rowAuthz(clusterOwner)\nconcept probe {\n  title  string\n}\n",
	}
	for _, src := range sources {
		if _, err := buildConcept(t, src); err != nil {
			t.Errorf("want this to build:\n%s\nerror: %v", src, err)
		}
	}
}

// The declaration RECORDS; it must not change what the concept validates,
// filters or traverses. A byte-identical schema and an unchanged relationship
// list are the direct statement of that: nothing downstream of the schema can
// behave differently because a concept said which account its rows belong to.
func TestBelongsToDoesNotChangeTheConceptSchema(t *testing.T) {
	const fields = "concept probe {\n  accountId  string\n  goalId     string\n}\n"
	plain, err := buildConcept(t, fields)
	if err != nil {
		t.Fatalf("undeclared concept: %v", err)
	}
	for _, annotation := range []string{
		`@belongsTo(cluster)`,
		`@belongsTo(account)`,
		`@belongsTo(account, shared)`,
		`@belongsTo(account, stated)`,
		`@belongsTo(account, via="goalId")`,
		`@belongsTo(account, via="goalId", parent="v1:work:goal")`,
		`@belongsTo(account, stays="transit")`,
	} {
		declared, err := buildConcept(t, annotation+"\n"+fields)
		if err != nil {
			t.Fatalf("%s: %v", annotation, err)
		}
		if len(declared.Schemas) != len(plain.Schemas) {
			t.Fatalf("%s: schema set changed size", annotation)
		}
		for key, want := range plain.Schemas {
			if got := declared.Schemas[key]; string(got) != string(want) {
				t.Fatalf("%s: schema %q changed\n got: %s\nwant: %s", annotation, key, got, want)
			}
		}
		if declared.NodeType != plain.NodeType || declared.Version != plain.Version ||
			len(declared.Relationships) != len(plain.Relationships) {
			t.Fatalf("%s: concept metadata changed", annotation)
		}
	}
}

// The declaration reaches the concept surface through the Concept's JSON form,
// so it has to survive it -- and an undeclared concept must not grow an empty
// object there, which a client would read as a declaration.
func TestBelongsToSurvivesConceptJSON(t *testing.T) {
	const fields = "concept probe {\n  accountId  string\n  goalId     string\n}\n"
	for _, annotation := range []string{
		`@belongsTo(cluster)`,
		`@belongsTo(account, shared, stated)`,
		`@belongsTo(account, via="goalId", parent="v1:work:goal")`,
		`@belongsTo(account, via="goalId", stays="machine")`,
	} {
		c, err := buildConcept(t, annotation+"\n"+fields)
		if err != nil {
			t.Fatalf("%s: %v", annotation, err)
		}
		blob, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("%s: marshal: %v", annotation, err)
		}
		var back Concept
		if err := json.Unmarshal(blob, &back); err != nil {
			t.Fatalf("%s: unmarshal: %v", annotation, err)
		}
		if back.BelongsTo == nil || *back.BelongsTo != *c.BelongsTo {
			t.Fatalf("%s did not survive JSON: %+v -> %+v", annotation, c.BelongsTo, back.BelongsTo)
		}
	}

	plain, err := buildConcept(t, fields)
	if err != nil {
		t.Fatalf("undeclared concept: %v", err)
	}
	blob, err := json.Marshal(plain)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), "belongsTo") {
		t.Fatalf("an undeclared concept serialises a belongsTo key: %s", blob)
	}
}

// Stays and Travels are the two questions an account export asks of a kind.
// Travels is narrower than AccountScoped: a kind that stays is the account's
// all the same, and only the export leaves it behind. A cluster-wide kind and
// an undeclared one neither travel nor stay -- they are in no account's file.
func TestStaysAndTravelsAnswerForEveryDeclaration(t *testing.T) {
	const fields = "concept probe {\n  accountId  string\n  goalId     string\n}\n"
	cases := []struct {
		name     string
		preamble string
		stays    string
		travels  bool
	}{
		{name: "undeclared", preamble: ""},
		{name: "cluster-wide", preamble: "@belongsTo(cluster)\n"},
		{name: "an account", preamble: "@belongsTo(account)\n", travels: true},
		{name: "the account of a parent", preamble: "@belongsTo(account, via=\"goalId\")\n", travels: true},
		{name: "staying for sign-in", preamble: "@belongsTo(account, stays=\"signin\")\n", stays: "signin"},
		{name: "staying in transit, through a parent", preamble: "@belongsTo(account, via=\"goalId\", stays=\"transit\")\n", stays: "transit"},
		{name: "staying on a machine, shared and stated", preamble: "@belongsTo(account, shared, stated, stays=\"machine\")\n", stays: "machine"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := buildConcept(t, tc.preamble+fields)
			if err != nil {
				t.Fatalf("BuildConceptFromDecl: %v", err)
			}
			if got := c.Stays(); got != tc.stays {
				t.Errorf("Stays() = %q, want %q", got, tc.stays)
			}
			if got := c.Travels(); got != tc.travels {
				t.Errorf("Travels() = %v, want %v", got, tc.travels)
			}
		})
	}
	var nilConcept *Concept
	if nilConcept.Stays() != "" || nilConcept.Travels() {
		t.Errorf("a nil concept answered Stays=%q Travels=%v", nilConcept.Stays(), nilConcept.Travels())
	}
}

// @bytes and @secretName reach the concept's schema and come back through
// their accessors, sorted by name, with @bytes's two flags and its place read
// as written.
func TestBytesAndSecretNameFieldsAreReadBackFromTheSchema(t *testing.T) {
	c, err := buildConcept(t, `@belongsTo(account)
@rowAuthz(owner="ownerUserId")
concept probe {
  accountId    string
  ownerUserId  string
  blobUrl      string    @bytes(under="library/{ownerUserId}")
  bundleRef    string    @bytes(tree, under="sites/{id}")
  snapshot     string    @bytes(counted, under="packages")
  archives     []string  @bytes(tree, counted, under="archives/{id}")
  names      []string  @secretName
  ref        string    @secretName
  title      string
}
`)
	if err != nil {
		t.Fatalf("BuildConceptFromDecl: %v", err)
	}
	wantBytes := []BytesField{
		{Name: "archives", Tree: true, Counted: true, Under: "archives/{id}", Concept: "v1:test:probe", OwnerField: "ownerUserId"},
		{Name: "blobUrl", Under: "library/{ownerUserId}", Concept: "v1:test:probe", OwnerField: "ownerUserId"},
		{Name: "bundleRef", Tree: true, Under: "sites/{id}", Concept: "v1:test:probe", OwnerField: "ownerUserId"},
		{Name: "snapshot", Counted: true, Under: "packages", Concept: "v1:test:probe", OwnerField: "ownerUserId"},
	}
	gotBytes := c.BytesFields()
	if len(gotBytes) != len(wantBytes) {
		t.Fatalf("BytesFields() = %+v, want %+v", gotBytes, wantBytes)
	}
	for i := range wantBytes {
		if gotBytes[i] != wantBytes[i] {
			t.Fatalf("BytesFields() = %+v, want %+v", gotBytes, wantBytes)
		}
	}
	if got := strings.Join(c.SecretNameFields(), ","); got != "names,ref" {
		t.Errorf("SecretNameFields() = %q, want \"names,ref\"", got)
	}

	plain, err := buildConcept(t, "concept probe {\n  title  string\n}\n")
	if err != nil {
		t.Fatalf("plain concept: %v", err)
	}
	if plain.BytesFields() != nil || plain.SecretNameFields() != nil {
		t.Errorf("a concept with neither annotation answered bytes=%+v secretNames=%v", plain.BytesFields(), plain.SecretNameFields())
	}
	var nilConcept *Concept
	if nilConcept.BytesFields() != nil || nilConcept.SecretNameFields() != nil {
		t.Error("a nil concept answered a bytes or secret-name field")
	}
}

// The schema keywords are the contract the accessors read, and a reader of a
// stored schema (an import, a client) reads them too.
func TestBytesAndSecretNameEmitTheirSchemaKeywords(t *testing.T) {
	c, err := buildConcept(t, "concept probe {\n  bundleRef  string  @bytes(tree, under=\"sites/{id}\")\n  ref  string  @secretName\n}\n")
	if err != nil {
		t.Fatalf("BuildConceptFromDecl: %v", err)
	}
	var schema struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(c.Schemas[definitionSchemaKey], &schema); err != nil {
		t.Fatalf("definition schema: %v", err)
	}
	bytesKw, ok := schema.Properties["bundleRef"]["x-bytes"].(map[string]any)
	if !ok || bytesKw["tree"] != true || bytesKw["counted"] != false || bytesKw["under"] != "sites/{id}" {
		t.Errorf("bundleRef x-bytes = %#v, want {tree: true, counted: false, under: sites/{id}}", schema.Properties["bundleRef"]["x-bytes"])
	}
	if schema.Properties["ref"]["x-secret-name"] != true {
		t.Errorf("ref x-secret-name = %#v, want true", schema.Properties["ref"]["x-secret-name"])
	}
	// The keywords are markers: the schema still compiles, and a row is
	// validated as it would be without them. The compiled schema is cached by
	// concept name, so this probe takes a name no other test compiles under.
	c.Name = "v1:test:fieldReferenceProbe"
	if err := c.validate(definitionSchemaKey, map[string]any{"bundleRef": "sites/a/", "ref": "token"}); err != nil {
		t.Errorf("a row under the marked schema: %v", err)
	}
}

// Both annotations name something by a string, so on any other type they
// would be a declaration nothing can read. Refused at load, with the field,
// its type and what to write.
func TestBytesAndSecretNameAreRefusedOnAFieldThatIsNotAString(t *testing.T) {
	for _, annotation := range []string{`@bytes(counted, under="spaces")`, `@bytes(tree, under="sites/{id}")`, "@secretName"} {
		for _, fieldType := range []string{"int", "bool", "object", "[]int", "map[string]string"} {
			t.Run(annotation+" on "+fieldType, func(t *testing.T) {
				_, err := buildConcept(t, "concept probe {\n  target  "+fieldType+"  "+annotation+"\n}\n")
				if err == nil {
					t.Fatalf("%s on %s built", annotation, fieldType)
				}
				var coded interface{ RuleCode() string }
				if !errors.As(err, &coded) || coded.RuleCode() != CodeFieldAnnotationType {
					t.Fatalf("error = %v, want code %s", err, CodeFieldAnnotationType)
				}
				name := strings.SplitN(annotation, "(", 2)[0]
				for _, want := range []string{name, "`target`", "string or a list of strings", "[" + CodeFieldAnnotationType + "]"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want it to contain %q", err, want)
					}
				}
			})
		}
	}
}

// The accessors read top-level fields only, so a @bytes or @secretName inside
// a nested block would load and be read by nobody. Refused with the same code.
func TestBytesAndSecretNameAreRefusedOnANestedField(t *testing.T) {
	for _, annotation := range []string{`@bytes(under="spaces/{id}")`, "@secretName"} {
		t.Run(annotation, func(t *testing.T) {
			_, err := buildConcept(t, "concept probe {\n  bundle {\n    ref  string  "+annotation+"\n  }\n}\n")
			var coded interface{ RuleCode() string }
			if err == nil || !errors.As(err, &coded) || coded.RuleCode() != CodeFieldAnnotationType {
				t.Fatalf("error = %v, want code %s", err, CodeFieldAnnotationType)
			}
			if !strings.Contains(err.Error(), "top-level") {
				t.Errorf("error = %q, want it to say the field must be top-level", err)
			}
		})
	}
}

// `under` is the place a row's bytes live in, and it is required: a kind that
// does not say where its bytes are has none an export may read. Every way of
// writing a place that names nothing, or names something other than the row's
// own values, is refused with bytes_under. A CARRIED field (not `counted`) is
// read, so its place must be the row's own: it names the row's id or the owner
// field the concept's @rowAuthz holds to the writer, and nothing else. A
// counted field is only sized, so it may name a literal place or any string
// field.
func TestBytesUnderIsRequiredAndChecked(t *testing.T) {
	const owned = "@rowAuthz(owner=\"ownerUserId\")\n"
	const body = "concept probe {\n  ownerUserId  string\n  folderId  string\n  size  int\n  details {\n    owner  string\n  }\n  blobUrl  string  "
	cases := []struct {
		name       string
		preamble   string
		annotation string
		wantText   string
	}{
		{"written bare", owned, `@bytes`, "names no place"},
		{"flags and no place", owned, `@bytes(tree)`, "names no place"},
		{"an empty place", owned, `@bytes(under="")`, "names no place"},
		{"an empty segment", owned, `@bytes(under="library//{id}")`, "an empty segment"},
		{"a leading slash", owned, `@bytes(under="/library/{id}")`, "an empty segment"},
		{"a trailing slash", owned, `@bytes(under="library/{id}/")`, "an empty segment"},
		{"a dot segment", owned, `@bytes(under="library/./{id}")`, "a relative segment"},
		{"a dot-dot segment", owned, `@bytes(under="library/../{id}")`, "a relative segment"},
		{"a field the concept does not declare", owned, `@bytes(counted, under="library/{nobody}")`, "`{nobody}` names neither the row's id nor a string field"},
		{"a field that is not a string", owned, `@bytes(counted, under="library/{size}")`, "`{size}` names neither the row's id nor a string field"},
		{"a nested field", owned, `@bytes(counted, under="library/{details}")`, "`{details}` names neither the row's id nor a string field"},
		{"the export prefix", owned, `@bytes(under="exports/{id}")`, "`exports`"},
		{"a brace inside a literal segment", owned, `@bytes(under="library/x{id}")`, "a placeholder is a whole segment"},
		{"an empty placeholder", owned, `@bytes(under="library/{}")`, "a placeholder is a whole segment"},
		{"a tree at the top level", owned, `@bytes(tree, under="{id}")`, "a tree"},
		{"a carried field with no placeholder", owned, `@bytes(under="spaces")`, "has no placeholder"},
		{"a carried tree with no placeholder", owned, `@bytes(tree, under="sites/all")`, "has no placeholder"},
		{"a carried field naming a field the writer chooses", owned, `@bytes(under="library/{folderId}")`, "`{folderId}` is neither the row's id, `{id}`, nor the owner field `{ownerUserId}`"},
		{"a carried field naming a field on a concept with no owner", "", `@bytes(under="library/{ownerUserId}")`, "declares no @rowAuthz(owner=...), so a carried field's place may use only `{id}`"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildConcept(t, tc.preamble+body+tc.annotation+"\n}\n")
			var coded interface{ RuleCode() string }
			if err == nil || !errors.As(err, &coded) || coded.RuleCode() != CodeBytesUnder {
				t.Fatalf("error = %v, want code %s", err, CodeBytesUnder)
			}
			for _, want := range []string{tc.wantText, "`blobUrl`", "[" + CodeBytesUnder + "]"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
	for _, ok := range []struct{ preamble, annotation string }{
		{owned, `@bytes(under="library/{ownerUserId}")`},
		{owned, `@bytes(tree, under="sites/{id}")`},
		{owned, `@bytes(tree, under="sites/{id}/{ownerUserId}")`},
		{"", `@bytes(tree, under="sites/{id}")`},
		{"", `@bytes(counted, under="spaces")`},
		{"", `@bytes(counted, under="packages/snapshots/{folderId}")`},
		{owned, `@bytes(counted, tree, under="packages/{folderId}")`},
	} {
		if _, err := buildConcept(t, ok.preamble+body+ok.annotation+"\n}\n"); err != nil {
			t.Errorf("%s%s: %v", ok.preamble, ok.annotation, err)
		}
	}
}

// The fields the shipped tree declares, as BytesFields() fills them: the
// concept and its owner field ride along, so Places and Admits know which
// concept each placeholder's id must be of.
var (
	libraryFile = BytesField{Name: "blobUrl", Under: "library/{ownerUserId}", Concept: "v1:library:file", OwnerField: "ownerUserId"}
	siteBundle  = BytesField{Name: "bundleRef", Tree: true, Under: "sites/{id}", Concept: "v1:platform:site", OwnerField: "ownerUserId"}
	snapshot    = BytesField{Name: "snapshotArtifactId", Counted: true, Under: "packages/snapshots/{packageId}", Concept: "v1:platform:packageDeployment"}
	spacesFile  = BytesField{Name: "blobUrl", Counted: true, Under: "spaces", Concept: "v1:common:attachment"}
)

// Places answers, per placeholder, the value as stored, its short id and the
// short id under the concept the placeholder's id must be of -- every spelling
// a real key can have and no other, because the blob reader compares its
// prefixes exactly.
func TestBytesFieldPlaces(t *testing.T) {
	cases := []struct {
		name    string
		field   BytesField
		rowID   string
		payload map[string]any
		want    []string
	}{
		{"a bare owner", libraryFile, "f1", map[string]any{"ownerUserId": "u1"}, []string{"library/u1", "library/v1:identity:user:u1"}},
		{"a canonical owner", libraryFile, "f1", map[string]any{"ownerUserId": "v1:identity:user:u1"},
			[]string{"library/u1", "library/v1:identity:user:u1"}},
		{"an owner of another concept is only itself", libraryFile, "f1", map[string]any{"ownerUserId": "v1:agents:agent:u1"},
			[]string{"library/v1:agents:agent:u1"}},
		{"the row's id, canonical", siteBundle, "v1:platform:site:s1", nil, []string{"sites/s1", "sites/v1:platform:site:s1"}},
		{"the row's id, bare", siteBundle, "s1", nil, []string{"sites/s1", "sites/v1:platform:site:s1"}},
		{"another concept's row with the same short id", siteBundle, "v1:platform:package:s1", nil, []string{"sites/v1:platform:package:s1"}},
		{"a counted field naming another field, canonical", snapshot, "d1", map[string]any{"packageId": "v1:platform:package:p1"},
			[]string{"packages/snapshots/p1", "packages/snapshots/v1:platform:package:p1"}},
		{"a counted field naming another field, bare", snapshot, "d1", map[string]any{"packageId": "p1"}, []string{"packages/snapshots/p1"}},
		{"a literal counted place", spacesFile, "a1", nil, []string{"spaces"}},
		{"no owner", libraryFile, "f1", map[string]any{}, []string{}},
		{"an empty owner", libraryFile, "f1", map[string]any{"ownerUserId": "  "}, []string{}},
		{"an owner that is not a string", libraryFile, "f1", map[string]any{"ownerUserId": 7}, []string{}},
		{"an owner holding a slash", libraryFile, "f1", map[string]any{"ownerUserId": "u1/../u2"}, []string{}},
		{"no row id", siteBundle, "", nil, []string{}},
		{"a short id that reads as a version", siteBundle, "v1:platform:site:v2", nil, []string{"sites/v1:platform:site:v2"}},
		{"a short id that reads as a concept", siteBundle, "v1:platform:site:v2:a:b", nil, []string{"sites/v1:platform:site:v2:a:b"}},
		{"a row id with surrounding space", siteBundle, " s1", nil, []string{}},
		{"an owner with surrounding space", libraryFile, "f1", map[string]any{"ownerUserId": "u1 "}, []string{}},
		{"a hand-built carried field naming another field", BytesField{Under: "x/{folderId}", Concept: "v1:a:b"}, "r1",
			map[string]any{"folderId": "v1:platform:package:p1"}, []string{}},
		{"two placeholders", BytesField{Under: "x/{id}/{ownerUserId}", Concept: "v1:a:b", OwnerField: "ownerUserId"}, "v1:a:b:r1",
			map[string]any{"ownerUserId": "u1"},
			[]string{"x/r1/u1", "x/r1/v1:identity:user:u1", "x/v1:a:b:r1/u1", "x/v1:a:b:r1/v1:identity:user:u1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.field.Places(tc.rowID, tc.payload)
			if got == nil || strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("Places = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// Admits holds a key to the row's own place: strictly inside it, segment by
// segment, with a placeholder segment the same id as the row's value the way
// core/librarykey judges one -- equal as written, or the same short id with
// each side's concept absent or the one the placeholder expects.
func TestBytesFieldAdmits(t *testing.T) {
	owned := map[string]any{"ownerUserId": "v1:identity:user:u1"}
	bareOwner := map[string]any{"ownerUserId": "u1"}
	cases := []struct {
		name    string
		field   BytesField
		rowID   string
		payload map[string]any
		key     string
		want    bool
	}{
		{"under the owner's bare id", libraryFile, "f1", owned, "library/u1/f1/report.pdf", true},
		{"under the owner's canonical id", libraryFile, "f1", owned, "library/v1:identity:user:u1/f1/report.pdf", true},
		{"a bare stored owner, a canonical key of the right concept", libraryFile, "f1", bareOwner, "library/v1:identity:user:u1/f1/a", true},
		{"a canonical stored owner, a bare key", libraryFile, "f1", owned, "library/u1/f1/a", true},
		{"another concept's canonical id in the owner segment", libraryFile, "f1", bareOwner, "library/v1:agents:agent:u1/f1/a", false},
		{"a stored owner of the wrong concept, a bare key", libraryFile, "f1", map[string]any{"ownerUserId": "v1:agents:agent:u1"}, "library/u1/f1/a", false},
		{"a stored owner of the wrong concept, a user key", libraryFile, "f1", map[string]any{"ownerUserId": "v1:agents:agent:u1"}, "library/v1:identity:user:u1/f1/a", false},
		{"a key that only contains the canonical id", libraryFile, "f1", bareOwner, "library/x:v1:identity:user:u1/f1/a", false},
		{"under another owner", libraryFile, "f1", owned, "library/u2/f1/report.pdf", false},
		{"the place itself", libraryFile, "f1", owned, "library/u1", false},
		{"the place with a trailing slash", libraryFile, "f1", owned, "library/u1/", false},
		{"a key that climbs out", libraryFile, "f1", owned, "library/u1/../u2/x", false},
		{"a key that climbs out with a backslash", libraryFile, "f1", owned, "library/u1/f1\\..\\..\\u2\\x", false},
		{"a prefix that only shares characters", libraryFile, "f1", owned, "library/u10/x", false},
		{"a percent-encoded dot-dot segment", libraryFile, "f1", owned, "library/u1/%2e%2e/u2/x", false},
		{"a percent-encoded dot segment", libraryFile, "f1", owned, "library/u1/%2E/x", false},
		{"a percent-encoded slash", libraryFile, "f1", owned, "library/u1/a%2F..%2Fb", false},
		{"a percent-encoded backslash", libraryFile, "f1", owned, "library/u1/a%5Cb", false},
		{"an ordinary percent sign", libraryFile, "f1", owned, "library/u1/f1/100%25.pdf", true},
		{"an owner key spelled with surrounding space", libraryFile, "f1", map[string]any{"ownerUserId": " u1"}, "library/u1/f1/a", false},
		{"a row with no owner", libraryFile, "f1", map[string]any{}, "library/u1/f1/a", false},
		{"a tree key", siteBundle, "v1:platform:site:s1", nil, "sites/s1/v3/", true},
		{"a tree key under the canonical id", siteBundle, "s1", nil, "sites/v1:platform:site:s1/v3/index.html", true},
		{"another site's tree", siteBundle, "s1", nil, "sites/s2/v3/", false},
		{"another concept's row with the same short id", siteBundle, "v1:platform:package:s1", nil, "sites/s1/v3/", false},
		{"a site's key spelled with another concept", siteBundle, "s1", nil, "sites/v1:platform:package:s1/v3/", false},
		{"a counted field's other placeholder", snapshot, "d1", map[string]any{"packageId": "v1:platform:package:p1"}, "packages/snapshots/p1/3.tar.gz", true},
		{"a counted field's other placeholder, another concept", snapshot, "d1", map[string]any{"packageId": "v1:platform:package:p1"}, "packages/snapshots/v1:platform:site:p1/3.tar.gz", false},
		{"a literal counted place", spacesFile, "a1", nil, "spaces/part/attachments/d/name.txt", true},
		{"an empty key", libraryFile, "f1", owned, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.field.Admits(tc.rowID, tc.payload, tc.key); got != tc.want {
				t.Fatalf("Admits(%q) = %v, want %v (places %v)", tc.key, got, tc.want, tc.field.Places(tc.rowID, tc.payload))
			}
		})
	}
}

// Places and Admits are one rule said twice. Every spelling Places returns
// admits a key under it, and over a set of candidate segments (every spelling
// an id could be written in, for the right concept and the wrong ones) a key is
// admitted exactly when its prefix is one of Places.
func TestBytesFieldPlacesAndAdmitsAgree(t *testing.T) {
	rows := []struct {
		field   BytesField
		rowID   string
		payload map[string]any
	}{
		{libraryFile, "f1", map[string]any{"ownerUserId": "u1"}},
		{libraryFile, "f1", map[string]any{"ownerUserId": "v1:identity:user:u1"}},
		{libraryFile, "f1", map[string]any{"ownerUserId": "v1:agents:agent:u1"}},
		{siteBundle, "s1", nil},
		{siteBundle, "v1:platform:site:s1", nil},
		{siteBundle, "v1:platform:package:s1", nil},
		{snapshot, "d1", map[string]any{"packageId": "v1:platform:package:p1"}},
		{snapshot, "d1", map[string]any{"packageId": "p1"}},
		{siteBundle, "v1:platform:site:v2", nil},
		{siteBundle, "v1:platform:site:v2:a:b", nil},
		{snapshot, "d1", map[string]any{"packageId": "v1:platform:package:v2"}},
	}
	candidates := []string{}
	for _, short := range []string{"u1", "s1", "p1", "u2", "v2", "v2:a:b"} {
		candidates = append(candidates, short)
		for _, concept := range []string{"v1:identity:user", "v1:agents:agent", "v1:platform:site", "v1:platform:package", "v1:library:file"} {
			candidates = append(candidates, concept+":"+short, "x:"+concept+":"+short)
		}
	}
	for _, r := range rows {
		places := r.field.Places(r.rowID, r.payload)
		inPlaces := map[string]bool{}
		for _, p := range places {
			inPlaces[p] = true
			if !r.field.Admits(r.rowID, r.payload, p+"/obj") {
				t.Errorf("%s %s %v: Places gave %q, and Admits refuses %q", r.field.Under, r.rowID, r.payload, p, p+"/obj")
			}
		}
		literal := strings.Split(r.field.Under, "/")
		for _, c := range candidates {
			segs := append([]string(nil), literal...)
			for i, s := range segs {
				if _, ok := bytesPlaceholder(s); ok {
					segs[i] = c
				}
			}
			prefix := strings.Join(segs, "/")
			if got, want := r.field.Admits(r.rowID, r.payload, prefix+"/obj"), inPlaces[prefix]; got != want {
				t.Errorf("%s %s %v: Admits(%q) = %v, but %q in Places is %v", r.field.Under, r.rowID, r.payload, prefix+"/obj", got, prefix, want)
			}
		}
	}
}
