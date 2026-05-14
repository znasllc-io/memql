package memql

import (
	"context"
	"testing"

	"github.com/visionarys-io/memql/component/auth"
)

// findCallerValue walks the expression tree and returns the Value of
// the first ComparisonExpression whose Field is payload.name.
func findPayloadNameValue(e ExpressionNode) any {
	switch n := e.(type) {
	case *LogicalExpression:
		if v := findPayloadNameValue(n.Left); v != nil {
			return v
		}
		return findPayloadNameValue(n.Right)
	case *ComparisonExpression:
		if len(n.Field.Parts) >= 2 && n.Field.Parts[0] == "payload" && n.Field.Parts[1] == "name" {
			return n.Value
		}
	}
	return nil
}

func parseListPartitionsFilter(t *testing.T) ExpressionNode {
	t.Helper()
	query := `concept==v1:platform:partition; payload.name in caller.partitions`
	tokens, err := tokenize(query)
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	p := newParser(tokens, nil)
	expr, err := p.parse()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return expr
}

func TestCallerReferenceParsesAsInRHS(t *testing.T) {
	expr := parseListPartitionsFilter(t)
	value := findPayloadNameValue(expr)
	ref, ok := value.(*CallerReference)
	if !ok {
		t.Fatalf("payload.name value is %T, want *CallerReference", value)
	}
	if ref.Path != "partitions" {
		t.Errorf("CallerReference.Path = %q, want %q", ref.Path, "partitions")
	}
}

func TestResolveCallerReferences_NoAccessContext_OwnerBypass(t *testing.T) {
	// When no AccessContext is attached, we treat the caller as an
	// owner (dev-mode fallback); caller.partitions resolves to the
	// owner-wildcard sentinel.
	resolved, err := resolveCallerReferences(context.Background(), parseListPartitionsFilter(t))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, ok := findPayloadNameValue(resolved).(ownerWildcardSentinel); !ok {
		t.Fatalf("want ownerWildcardSentinel, got %T", findPayloadNameValue(resolved))
	}
}

func TestResolveCallerReferences_OwnerBypass(t *testing.T) {
	ac := &auth.AccessContext{Role: auth.RoleOwner}
	ctx := auth.ContextWithAccess(context.Background(), ac)
	resolved, err := resolveCallerReferences(ctx, parseListPartitionsFilter(t))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, ok := findPayloadNameValue(resolved).(ownerWildcardSentinel); !ok {
		t.Fatalf("owner expected to get wildcard sentinel, got %T", findPayloadNameValue(resolved))
	}
}

func TestResolveCallerReferences_NonOwnerGetsPartitionList(t *testing.T) {
	ac := &auth.AccessContext{
		Role: auth.RoleWriter,
		PartitionACL: auth.PartitionACL{
			"default": auth.RoleWriter,
			"acme":    auth.RoleAdmin,
		},
	}
	ctx := auth.ContextWithAccess(context.Background(), ac)
	resolved, err := resolveCallerReferences(ctx, parseListPartitionsFilter(t))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	value := findPayloadNameValue(resolved)
	partitions, ok := value.([]string)
	if !ok {
		t.Fatalf("want []string, got %T", value)
	}
	if len(partitions) != 2 {
		t.Fatalf("want 2 partitions, got %d: %v", len(partitions), partitions)
	}
	seen := map[string]bool{}
	for _, p := range partitions {
		seen[p] = true
	}
	if !seen["default"] || !seen["acme"] {
		t.Errorf("missing partitions: got %v, want default+acme", partitions)
	}
}

func TestResolveCallerReferences_ScalarPaths(t *testing.T) {
	ac := &auth.AccessContext{
		UserId:       "user-xyz",
		PrimaryEmail: "alice@example.com",
		Role:         auth.RoleWriter,
		IdentityId:   "identity-abc",
	}
	cases := []struct {
		path string
		want any
	}{
		{"userId", "user-xyz"},
		{"identityId", "identity-abc"},
		{"role", "writer"},
		{"primaryEmail", "alice@example.com"},
		{"isOwner", false},
	}
	ctx := auth.ContextWithAccess(context.Background(), ac)
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got, err := resolveCallerPath(ctx, tc.path, OpEq)
			if err != nil {
				t.Fatalf("resolve %q: %v", tc.path, err)
			}
			if got != tc.want {
				t.Errorf("path=%q got=%v want=%v", tc.path, got, tc.want)
			}
		})
	}
}

func TestResolveCallerReferences_UnknownPathErrors(t *testing.T) {
	_, err := resolveCallerPath(context.Background(), "bogus", OpEq)
	if err == nil {
		t.Fatal("expected error for unknown caller path")
	}
}

func TestResolveCallerReferences_PartitionsNotWithEq(t *testing.T) {
	_, err := resolveCallerPath(context.Background(), "partitions", OpEq)
	if err == nil {
		t.Fatal("expected error when caller.partitions is used with non-in operator")
	}
}
