package dslconformance

// create_audit_event_target_type_4213_test.go -- memql#4213.
//
// Every passkey login challenge writes targetType=passkeyIdentity through
// createAuditEvent. The durable v1:identity:auditEvent insert is refused when
// that value is missing from the mutation's closed targetType enum, leaving
// only the in-memory slog line (WARN audit_db_write_failed
// action=passkey_login_challenge_issued). The writer is already honest; this
// gate locks the enum so a later tidy-up cannot drop the value again.
//
// The concept field enum is the storage twin of the mutation arg. Both must
// accept the same value or the insert still fails one layer later.

import (
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/dsl"
)

var createAuditEventBlockRe = regexp.MustCompile(
	`(?s)mutate\s+auditEvent\s+createAuditEvent\s*\{.*?targetType\s+enum\(([^)]*)\)`,
)

var auditEventConceptTargetTypeRe = regexp.MustCompile(
	`(?s)concept\s+auditEvent\s*\{.*?targetType\s+enum\(([^)]*)\)`,
)

func enumLiterals(t *testing.T, raw string, label string) []string {
	t.Helper()
	var out []string
	for _, part := range strings.Split(raw, ",") {
		v := strings.Trim(strings.TrimSpace(part), `"`)
		if v == "" {
			continue
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		t.Fatalf("%s: parsed no enum literals from %q", label, raw)
	}
	return out
}

func containsEnum(vals []string, want string) bool {
	for _, v := range vals {
		if v == want {
			return true
		}
	}
	return false
}

func readDSLFile(t *testing.T, path string) string {
	t.Helper()
	f, err := dsl.Tree().Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	raw, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// TestCreateAuditEventTargetTypeAcceptsPasskeyIdentity is the #4213 lock:
// createAuditEvent (and the auditEvent concept field) must accept the
// targetType the passkey writer already emits.
func TestCreateAuditEventTargetTypeAcceptsPasskeyIdentity(t *testing.T) {
	const want = "passkeyIdentity"

	mut := readDSLFile(t, "identity/mutations.memql")
	m := createAuditEventBlockRe.FindStringSubmatch(mut)
	if m == nil {
		t.Fatal("could not find createAuditEvent targetType enum in identity/mutations.memql")
	}
	mutVals := enumLiterals(t, m[1], "createAuditEvent.targetType")
	if !containsEnum(mutVals, want) {
		t.Fatalf("createAuditEvent targetType enum does not accept %q (got %v).\n\n"+
			"The passkey login-challenge writer emits targetType=%s; without this "+
			"value the durable audit insert is refused (memql#4213).",
			want, mutVals, want)
	}

	con := readDSLFile(t, "identity/concepts.memql")
	c := auditEventConceptTargetTypeRe.FindStringSubmatch(con)
	if c == nil {
		t.Fatal("could not find auditEvent.targetType enum in identity/concepts.memql")
	}
	conVals := enumLiterals(t, c[1], "auditEvent.targetType")
	if !containsEnum(conVals, want) {
		t.Fatalf("auditEvent.targetType enum does not accept %q (got %v).\n\n"+
			"The concept field is the storage twin of createAuditEvent's arg; both "+
			"must accept the passkey writer's targetType (memql#4213).",
			want, conVals)
	}
}
