package adminops

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// cors_test.go -- memql#3716.
//
// The gate itself is covered by gate_test.go's table (every role beneath the
// floor, refused and audited, engine untouched). What is left here is the part
// specific to this operation: the origin grammar refuses on the way IN, and both
// engine calls carry the internal-origin stamp the @serverOnly mutation needs.

// corsClientEngine answers the by-id lookup with one registered client and
// records the origin of every call.
//
// A slice rather than a map keyed by construct, for the reason
// originRecordingEngine states: a map is last-write-wins, so an unstamped call
// placed before a stamped one would be overwritten and vanish.
type corsClientEngine struct {
	clientId string
	calls    []struct {
		query  string
		origin auth.CallOrigin
	}
}

func (e *corsClientEngine) Execute(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	e.calls = append(e.calls, struct {
		query  string
		origin auth.CallOrigin
	}{q, auth.OriginFromContext(ctx)})

	// Answered for THIS client only. A fake that resolves every id cannot tell
	// the existence check apart from a write-blind path.
	if strings.Contains(q, "oAuthClientByClientId(") && strings.Contains(q, `"`+e.clientId+`"`) {
		return &memqlengine.ExecuteResult{
			Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{
				Id: e.clientId,
				Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
					"clientId":         structpb.NewStringValue(e.clientId),
					"clientName":       structpb.NewStringValue("A Customer Site"),
					"redirectURIsJSON": structpb.NewStringValue(`["https://shop.customer.test/cb"]`),
				}},
			}}},
		}, nil
	}
	return &memqlengine.ExecuteResult{}, nil
}

func newCORSGrantService(t *testing.T, clientId string) (*Service, *corsClientEngine, *capturingAudit) {
	t.Helper()
	eng := &corsClientEngine{clientId: clientId}
	audit := &capturingAudit{}
	svc, err := New(&Service{Engine: eng, Audit: audit})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, eng, audit
}

// TestSetCORSOriginsRefusesMalformedOrigins is the validate-on-the-way-in half.
//
// Every case is a value an operator could plausibly paste, and each is refused
// with the offending entry QUOTED IN THE MESSAGE -- because the person on the
// other end typed a list and needs to know which line is wrong. Validating only
// on the read side would store the bad entry, never match it, and present a
// grant that silently does nothing.
func TestSetCORSOriginsRefusesMalformedOrigins(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry string
	}{
		{"a path", "https://shop.customer.test/app"},
		{"a trailing slash", "https://shop.customer.test/"},
		{"a query string", "https://shop.customer.test?x=1"},
		{"a fragment", "https://shop.customer.test#frag"},
		{"the wildcard", "*"},
		{"a wildcard host", "https://*.customer.test"},
		{"no scheme", "shop.customer.test"},
		{"a non-http scheme", "ftp://shop.customer.test"},
		{"userinfo", "https://user:pass@shop.customer.test"},
		{"empty", "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, eng, audit := newCORSGrantService(t, "mcp_customer")

			res := svc.SetOAuthClientCORSOrigins(ctxAs("owner"), "mcp_customer",
				[]string{"https://good.customer.test", tc.entry})

			if res.OK {
				t.Fatalf("%q was accepted as an origin", tc.entry)
			}
			if res.Code != CodeInvalidArgument {
				t.Errorf("code = %d, want %d (INVALID_ARGUMENT)", res.Code, CodeInvalidArgument)
			}
			trimmed := strings.TrimSpace(tc.entry)
			if trimmed != "" && !strings.Contains(res.ErrorMessage, trimmed) {
				t.Errorf("error message %q does not name the offending entry %q -- an operator "+
					"cannot fix a list they are not told the bad line of", res.ErrorMessage, tc.entry)
			}
			// Nothing may be written: a partially-applied allowance is the state
			// nobody can reason about afterwards, and the GOOD entry beside the
			// bad one is what makes that reachable.
			for _, call := range eng.calls {
				if strings.Contains(call.query, "setOAuthClientCORSOrigins(") {
					t.Errorf("a refused list was still written: %s", call.query)
				}
			}
			if len(audit.events) != 1 || audit.events[0].FailureReason == "" {
				t.Errorf("want exactly one audited failure, got %+v", audit.events)
			}
		})
	}
}

// TestSetCORSOriginsCanonicalisesAndDeduplicates covers the accepted shapes. The
// stored form is what a browser sends -- scheme and host lowercased, port kept --
// so an entry an operator typed in mixed case still matches.
func TestSetCORSOriginsCanonicalisesAndDeduplicates(t *testing.T) {
	svc, eng, audit := newCORSGrantService(t, "mcp_customer")

	res := svc.SetOAuthClientCORSOrigins(ctxAs("admin"), "mcp_customer", []string{
		"HTTPS://Shop.Customer.Test",
		"  https://shop.customer.test  ",
		"https://shop.customer.test:8443",
		"http://localhost:5173",
	})
	if !res.OK {
		t.Fatalf("grant failed: code=%d %s", res.Code, res.ErrorMessage)
	}

	var written string
	for _, call := range eng.calls {
		if strings.Contains(call.query, "setOAuthClientCORSOrigins(") {
			written = call.query
		}
	}
	if written == "" {
		t.Fatal("no setOAuthClientCORSOrigins call was made")
	}
	for _, want := range []string{
		`https://shop.customer.test`,
		`https://shop.customer.test:8443`,
		`http://localhost:5173`,
	} {
		if !strings.Contains(written, want) {
			t.Errorf("written allowance %s does not carry %q", written, want)
		}
	}
	// Three spellings of one origin went in (mixed case, padded, and the plain
	// form); exactly one canonical entry must come out. Matched with its closing
	// quote so the :8443 sibling does not count as a second occurrence.
	if n := strings.Count(written, `\"https://shop.customer.test\"`); n != 1 {
		t.Errorf("the canonical origin appears %d time(s), want 1 -- two spellings of one "+
			"origin were stored twice: %s", n, written)
	}

	// The trail records the CANONICAL forms, not the operator's input: an audit
	// event quoting what was typed while the row holds something else answers
	// the wrong question.
	if len(audit.events) != 1 {
		t.Fatalf("want one audit event, got %d", len(audit.events))
	}
	origins, _ := audit.events[0].Detail["origins"].([]string)
	if len(origins) != 3 {
		t.Fatalf("audit detail origins = %#v, want the 3 canonical forms", audit.events[0].Detail["origins"])
	}
	if origins[0] != "https://shop.customer.test" {
		t.Errorf("audit detail origins[0] = %q, want the canonicalised form", origins[0])
	}
}

// TestSetCORSOriginsRevokeWritesEmptyNotAnEmptyArray pins the state collapse at
// the wire level (fix round 1, item 6).
//
// `filter corsOriginsJSON != ""` already excludes the ABSENT field a
// freshly-registered client carries, but it MATCHES "[]". A revoke written that
// way would leave the row in oAuthClientCORSGrants's result set forever while
// contributing no origins -- an @unbounded read that only ever grows, and a query
// comment claiming more than it delivers. "" returns the row to the state it had
// before it was ever granted.
func TestSetCORSOriginsRevokeWritesEmptyNotAnEmptyArray(t *testing.T) {
	svc, eng, _ := newCORSGrantService(t, "mcp_customer")

	res := svc.SetOAuthClientCORSOrigins(ctxAs("owner"), "mcp_customer", nil)
	if !res.OK {
		t.Fatalf("revoke failed: code=%d %s", res.Code, res.ErrorMessage)
	}

	var written string
	for _, call := range eng.calls {
		if strings.Contains(call.query, "setOAuthClientCORSOrigins(") {
			written = call.query
		}
	}
	if written == "" {
		t.Fatal("no setOAuthClientCORSOrigins call was made")
	}
	if strings.Contains(written, "[]") {
		t.Errorf("a revoke wrote an empty JSON ARRAY: %s\n"+
			"That is a third spelling of 'no allowance' -- absent and \"\" are the other two -- "+
			"and it is the only one the query's `corsOriginsJSON != \"\"` filter matches.", written)
	}
	if !strings.Contains(written, `corsOriginsJSON: ""`) {
		t.Errorf("a revoke did not write an empty corsOriginsJSON: %s", written)
	}
}

// TestSetCORSOriginsReportsAReadFailureAsInternalNotNotFound pins the split added
// in fix round 1, item 1.
//
// Collapsed into one NOT_FOUND, a transient database error told an owner their
// client did not exist AND pointed them at MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS --
// the one path that needs no gate and a restart. That is advice to route around
// this operation's own authorization, delivered exactly when the cluster is
// unhealthy.
func TestSetCORSOriginsReportsAReadFailureAsInternalNotNotFound(t *testing.T) {
	audit := &capturingAudit{}
	// recordingEngine fails every Execute, which IS the read-failure case: the
	// lookup errors rather than coming back empty.
	eng := &recordingEngine{}
	svc, err := New(&Service{Engine: eng, Audit: audit})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res := svc.SetOAuthClientCORSOrigins(ctxAs("owner"), "mcp_customer",
		[]string{"https://shop.customer.test"})

	if res.OK {
		t.Fatal("a failed read was reported as a successful grant")
	}
	if res.Code != CodeInternal {
		t.Errorf("code = %d, want %d (INTERNAL) -- a read that FAILED is not a client that is "+
			"missing, and the two call for different operator responses", res.Code, CodeInternal)
	}
	if strings.Contains(res.ErrorMessage, "MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS") {
		t.Errorf("a database failure pointed the operator at the un-gated env list: %q\n"+
			"That advice belongs only on the genuinely-missing-client path.", res.ErrorMessage)
	}
	if strings.Contains(res.ErrorMessage, "no such") {
		t.Errorf("a database failure was described as a missing client: %q", res.ErrorMessage)
	}
	// The audit already carried the truth before this fix; assert it still does.
	if len(audit.events) != 1 {
		t.Fatalf("want one audit event, got %d", len(audit.events))
	}
	if reason := audit.events[0].FailureReason; !strings.Contains(reason, "test engine") {
		t.Errorf("audit failure reason = %q, want the underlying engine error", reason)
	}
	// Nothing may be written after a read we could not complete.
	for _, q := range eng.queries {
		if strings.Contains(q, "setOAuthClientCORSOrigins(") {
			t.Errorf("an allowance was written after the existence check failed: %s", q)
		}
	}
}

// TestSetCORSOriginsStampsInternalOrigin is the load-bearing half of making the
// mutation @serverOnly.
//
// setOAuthClientCORSOrigins is enforced as `fn.ServerOnly &&
// !auth.OriginFromContext(ctx).IsInternal()`, so an unstamped call is REFUSED at
// runtime -- and would fail in an operator's console rather than in any test.
// Behavioural at the Engine seam rather than a source scan, for the reasons
// TestAdmittedWritesStampInternalOrigin records at length.
func TestSetCORSOriginsStampsInternalOrigin(t *testing.T) {
	svc, eng, _ := newCORSGrantService(t, "mcp_customer")

	res := svc.SetOAuthClientCORSOrigins(ctxAs("owner"), "mcp_customer",
		[]string{"https://shop.customer.test"})
	if !res.OK {
		t.Fatalf("grant failed against the stub engine: code=%d %s", res.Code, res.ErrorMessage)
	}

	var sawRead, sawWrite bool
	for i, call := range eng.calls {
		if strings.Contains(call.query, "oAuthClientByClientId(") {
			sawRead = true
		}
		if strings.Contains(call.query, "setOAuthClientCORSOrigins(") {
			sawWrite = true
		}
		if call.origin.IsInternal() {
			continue
		}
		t.Errorf("call %d of %d ran with origin %v, not internal: %s",
			i+1, len(eng.calls), call.origin, call.query)
	}
	if !sawRead {
		t.Error("no call named oAuthClientByClientId -- the existence check is what stops an " +
			"allowance being written against a mistyped clientId; if the query was renamed, " +
			"move this guard with it")
	}
	if !sawWrite {
		t.Error("no call named setOAuthClientCORSOrigins -- if the mutation was renamed, move " +
			"this guard with it")
	}
}

// TestSetCORSOriginsRefusesAnUnknownClient pins the existence check.
//
// The mutation is a partial update keyed on the row id, so writing blind would
// leave an allowance attached to nothing: an origin the operator believes is
// granted, on no client, absent from the list they would check it against.
func TestSetCORSOriginsRefusesAnUnknownClient(t *testing.T) {
	// The engine answers the lookup for "mcp_customer" only, so any other id
	// resolves to no row.
	svc, eng, _ := newCORSGrantService(t, "mcp_customer")

	res := svc.SetOAuthClientCORSOrigins(ctxAs("owner"), "mcp_typo",
		[]string{"https://shop.customer.test"})
	if res.OK {
		t.Fatal("an allowance was granted for a client that does not exist")
	}
	if res.Code != CodeNotFound {
		t.Errorf("code = %d, want %d (NOT_FOUND)", res.Code, CodeNotFound)
	}
	if !strings.Contains(res.ErrorMessage, "MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS") {
		t.Errorf("error message %q should point a confused operator at the env list -- a "+
			"STATICALLY configured client carries no row, so this is the likeliest way to "+
			"reach this refusal legitimately", res.ErrorMessage)
	}
	for _, call := range eng.calls {
		if strings.Contains(call.query, "setOAuthClientCORSOrigins(") {
			t.Errorf("an allowance was written for an unknown client: %s", call.query)
		}
	}
}

// TestSetCORSOriginsRefusesAnEmptyClientId keeps the missing-argument case an
// INVALID_ARGUMENT rather than a NOT_FOUND, so an operator can tell "you did not
// name a client" apart from "that client does not exist".
func TestSetCORSOriginsRefusesAnEmptyClientId(t *testing.T) {
	svc, eng, _ := newCORSGrantService(t, "mcp_customer")

	res := svc.SetOAuthClientCORSOrigins(ctxAs("owner"), "   ", []string{"https://shop.customer.test"})
	if res.OK {
		t.Fatal("an allowance was granted with no clientId")
	}
	if res.Code != CodeInvalidArgument {
		t.Errorf("code = %d, want %d (INVALID_ARGUMENT)", res.Code, CodeInvalidArgument)
	}
	if len(eng.calls) != 0 {
		t.Errorf("the engine was reached %d time(s) with no clientId: %+v", len(eng.calls), eng.calls)
	}
}

// TestSetCORSOriginsRecordsWhatWasRevoked pins the revoke trail.
//
// A revoke whose audit event does not say what it removed records that something
// changed without recording what -- which on a trust decision is the half an
// operator comes back for.
func TestSetCORSOriginsRecordsWhatWasRevoked(t *testing.T) {
	eng := &corsClientEngine{clientId: "mcp_customer"}
	audit := &capturingAudit{}
	svc, err := New(&Service{Engine: eng, Audit: audit})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The lookup answers with an allowance already in place, which is the state a
	// revoke acts on.
	eng.calls = nil
	svc.Engine = &corsClientEngineWithGrant{corsClientEngine: eng, grant: `["https://shop.customer.test"]`}

	res := svc.SetOAuthClientCORSOrigins(ctxAs("owner"), "mcp_customer", nil)
	if !res.OK {
		t.Fatalf("revoke failed: code=%d %s", res.Code, res.ErrorMessage)
	}
	if len(audit.events) != 1 {
		t.Fatalf("want one audit event, got %d", len(audit.events))
	}
	ev := audit.events[0]
	if ev.Action != "oauth_client_cors_revoked" {
		t.Errorf("audit action = %q, want oauth_client_cors_revoked -- the two directions are "+
			"different questions and an operator greps for one of them", ev.Action)
	}
	previous, _ := ev.Detail["previousOrigins"].([]string)
	if len(previous) != 1 || previous[0] != "https://shop.customer.test" {
		t.Errorf("audit detail previousOrigins = %#v, want the allowance that was removed",
			ev.Detail["previousOrigins"])
	}
}

// corsClientEngineWithGrant answers the by-id lookup with an allowance already
// on the row.
type corsClientEngineWithGrant struct {
	*corsClientEngine
	grant string
}

func (e *corsClientEngineWithGrant) Execute(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	res, err := e.corsClientEngine.Execute(ctx, q)
	if err != nil || res == nil || res.Bundle == nil || len(res.Bundle.Nodes) == 0 {
		return res, err
	}
	res.Bundle.Nodes[0].Payload.Fields["corsOriginsJSON"] = structpb.NewStringValue(e.grant)
	return res, nil
}

// Compile-time assurance the fakes satisfy the surface the Service depends on.
var (
	_ identity.EngineExecutor = (*corsClientEngine)(nil)
	_ identity.EngineExecutor = (*corsClientEngineWithGrant)(nil)
)
