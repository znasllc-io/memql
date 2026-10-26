package memql

import "testing"

// The operator's cluster address is not a client reservation (memql#5792).
//
// The DB cases in account_self_cluster_address_db_test.go drive the write
// path; these pin the decision itself, including the branches the write path
// reaches only through a particular stored row.

func TestTheOperatorsInheritedClusterAddressIsCleared(t *testing.T) {
	t.Setenv(memqlDomainEnv, "memql.example.test")
	for _, id := range []string{selfAccountId, "self"} {
		payload := map[string]any{
			"memqlDomain":            "memql.example.test",
			"memqlReservedAt":        "2026-01-01T00:00:00Z",
			"memqlReservationReason": accountDomainUnderClusterCode,
		}
		normalizeSelfAccountReservation(id, payload, nil, false)
		for _, field := range []string{"memqlDomain", "memqlReservedAt", "memqlReservationReason"} {
			if payload[field] != "" {
				t.Fatalf("id %q: %s = %v, want it cleared with the name", id, field, payload[field])
			}
		}
		if err := validateAccountMemqlDomain(payload); err != nil {
			t.Fatalf("id %q: the normalized row is still refused: %v", id, err)
		}
	}
}

func TestTheOperatorsDerivedClusterAddressIsCleared(t *testing.T) {
	// The default fill is the other writer: the owner typed the company
	// domain, and `memql.` + that domain is the cluster.
	t.Setenv(memqlDomainEnv, "memql.example.test")
	payload := map[string]any{"domain": "example.test"}
	resetAccountDomainOnChange(payload, "", true)
	if payload["memqlDomain"] != "memql.example.test" {
		t.Fatalf("precondition: memqlDomain = %v, want the default fill", payload["memqlDomain"])
	}
	normalizeSelfAccountReservation(selfAccountId, payload, nil, false)
	if payload["memqlDomain"] != "" || payload["domain"] != "example.test" {
		t.Fatalf("want the name cleared and the domain kept, got %v", payload)
	}
}

func TestAClusterNameTheOperatorAsksForIsLeftForTheGuard(t *testing.T) {
	t.Setenv(memqlDomainEnv, "memql.example.test")
	payload := map[string]any{"memqlDomain": "os.memql.example.test"}
	normalizeSelfAccountReservation(selfAccountId, payload, " OS.memql.example.test ", true)
	if payload["memqlDomain"] != "os.memql.example.test" {
		t.Fatalf("memqlDomain = %v: a name the caller asked for was silently dropped", payload["memqlDomain"])
	}
	if err := validateAccountMemqlDomain(payload); err == nil {
		t.Fatal("the operator's explicit cluster-space name was admitted")
	}
}

func TestTheNormalizationLeavesEveryOtherRowAlone(t *testing.T) {
	t.Setenv(memqlDomainEnv, "memql.example.test")
	// A client in cluster space keeps its name, so the guard refuses it.
	client := map[string]any{"memqlDomain": "memql.example.test", "memqlReservedAt": "2026-01-01T00:00:00Z"}
	normalizeSelfAccountReservation("v1:accounts:account:acme", client, nil, false)
	if client["memqlDomain"] != "memql.example.test" || client["memqlReservedAt"] != "2026-01-01T00:00:00Z" {
		t.Fatalf("a client row was normalized: %v", client)
	}
	// The operator's own name OUTSIDE cluster space is a real reservation.
	own := map[string]any{"memqlDomain": "workspace.znas.io", "memqlReservedAt": "2026-01-01T00:00:00Z"}
	normalizeSelfAccountReservation(selfAccountId, own, nil, false)
	if own["memqlDomain"] != "workspace.znas.io" || own["memqlReservedAt"] != "2026-01-01T00:00:00Z" {
		t.Fatalf("the operator's real reservation was cleared: %v", own)
	}
	normalizeSelfAccountReservation(selfAccountId, nil, nil, false)
}
