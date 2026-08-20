package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

// pack_state.go reads v1:platform:packState -- the per-instance pack
// enablement rows (module-registry design section 4.1) -- with a RAW query
// rather than the engine's executor, because its first caller runs where no
// executor exists yet: app phase 3, after the database starts and before
// engine.Init loads a single DSL construct. The module registry reuses the
// same reader at request time so boot and inventory can never disagree on
// how a row is interpreted.
//
// One row per pack at id v1:platform:packState:<domain>; the table is
// append-only with PK (id, "createdAt"), so "the current state" is the
// newest version per id -- DISTINCT ON (id) ... ORDER BY id, "createdAt"
// DESC, the same latest-version rule every engine read applies.

// PackStateConceptID is the canonical concept id enablement rows live under.
const PackStateConceptID = "v1:platform:packState"

// PackStateRow is the decoded current state of one pack's enablement row.
type PackStateRow struct {
	PackDomain string
	Enabled    bool
	Reason     string
	UpdatedAt  time.Time
	UpdatedBy  string
}

// packStatePayload is the JSONB payload shape the mutation writes.
type packStatePayload struct {
	PackDomain string `json:"packDomain"`
	Enabled    bool   `json:"enabled"`
	Reason     string `json:"reason"`
}

// ReadPackStates returns the newest version of every v1:platform:packState
// row, keyed by pack domain.
//
// A database with no MemoryNodes relation yet -- the very first boot, before
// migrations have run -- reads as EMPTY, not as an error: absence of state
// means every pack is enabled, and a fresh install must not need seeding to
// boot (design section 4.2). Every other error is returned; the boot caller
// treats it as fatal, because silently booting all-enabled against a
// database that refused the read would let one node run a pack the rest of
// the mesh has switched off.
func ReadPackStates(ctx context.Context, db *bun.DB) (map[string]PackStateRow, error) {
	if db == nil {
		return nil, fmt.Errorf("pack state read: nil database handle")
	}

	var rows []struct {
		ID        string          `bun:"id"`
		CreatedAt time.Time       `bun:"createdAt"`
		CreatedBy string          `bun:"createdBy"`
		Payload   json.RawMessage `bun:"payload"`
	}
	err := db.NewRaw(
		`SELECT DISTINCT ON (id) id, "createdAt", "createdBy", payload
		   FROM "MemoryNodes"
		  WHERE concept = ?
		  ORDER BY id, "createdAt" DESC`,
		PackStateConceptID,
	).Scan(ctx, &rows)
	if err != nil {
		if isMissingRelation(err) {
			return map[string]PackStateRow{}, nil
		}
		return nil, fmt.Errorf("pack state read: %w", err)
	}

	out := make(map[string]PackStateRow, len(rows))
	for _, r := range rows {
		var p packStatePayload
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			// A row this reader cannot decode is a row the loaders cannot
			// honor. Refuse rather than guess -- same fail-loud posture as
			// strict DSL boot.
			return nil, fmt.Errorf("pack state read: row %s has undecodable payload: %w", r.ID, err)
		}
		domain := strings.TrimSpace(p.PackDomain)
		if domain == "" {
			// The id's short form is the domain by convention; fall back to
			// it so a hand-written row without the payload field still
			// resolves instead of silently vanishing.
			domain = strings.TrimPrefix(r.ID, PackStateConceptID+":")
		}
		if domain == "" || domain == r.ID {
			continue
		}
		out[domain] = PackStateRow{
			PackDomain: domain,
			Enabled:    p.Enabled,
			Reason:     p.Reason,
			UpdatedAt:  r.CreatedAt,
			UpdatedBy:  r.CreatedBy,
		}
	}
	return out, nil
}

// DisabledPackDomainsFromStates projects the read into the boot-time
// disabled set: exactly the domains whose newest row says enabled=false.
func DisabledPackDomainsFromStates(states map[string]PackStateRow) []string {
	var out []string
	for domain, row := range states {
		if !row.Enabled {
			out = append(out, domain)
		}
	}
	return out
}

// isMissingRelation reports the Postgres undefined-table state (42P01),
// which is the expected shape of a first boot racing ahead of migrations.
// String match over the driver error, the same way
// component/database/timescaledb.go detects it.
func isMissingRelation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "42P01") || strings.Contains(msg, "does not exist")
}
