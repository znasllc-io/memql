package memoryNodes

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

const (
	MemoryNodesTable       = "MemoryNodes"
	SecretMemoryNodesTable = "SecretMemoryNodes"
)

type (
	MemoryNode struct {
		bun.BaseModel `bun:"table:MemoryNodes,alias:mn"`
		ID            string          `bun:",pk" json:"id"`
		CreatedAt     time.Time       `bun:"\"createdAt\",pk,type:TIMESTAMPTZ" json:"createdAt"`
		CreatedBy     string          `bun:"\"createdBy\",notnull" json:"createdBy"`
		Concept       string          `bun:",notnull" json:"concept"`
		Type          string          `bun:"type,notnull" json:"type"`
		Schema        json.RawMessage `bun:"type:JSONB,notnull" json:"schema"`
		Payload       json.RawMessage `bun:"type:JSONB,notnull" json:"payload"`
		Metadata      json.RawMessage `bun:"type:JSONB,notnull,default:'{}'" json:"metadata,omitempty"`

		// Provenance is engine-stamped per-version metadata describing
		// where this row came from -- seed name, mutation name,
		// originating automation + trigger event, etc. NOT NULL: the
		// mutation executor rejects writes whose Go context carries no
		// provenance.Provenance value, so every row that ever lands
		// has a real attribution. See component/provenance for the
		// shape + helpers.
		Provenance json.RawMessage `bun:"type:JSONB,notnull" json:"provenance"`
	}

	SecretMemoryNode struct {
		bun.BaseModel `bun:"table:SecretMemoryNodes,alias:smn"`
		ID            string          `bun:",pk" json:"id"`
		CreatedAt     time.Time       `bun:"\"createdAt\",pk,type:TIMESTAMPTZ" json:"createdAt"`
		CreatedBy     string          `bun:"\"createdBy\",notnull" json:"createdBy"`
		Concept       string          `bun:",notnull" json:"concept"`
		Type          string          `bun:"type,notnull" json:"type"`
		Schema        json.RawMessage `bun:"type:JSONB,notnull" json:"schema"`
		Payload       json.RawMessage `bun:"type:JSONB,notnull" json:"payload"`
		Metadata      json.RawMessage `bun:"type:JSONB,notnull,default:'{}'" json:"metadata,omitempty"`

		// Provenance: same shape + contract as on MemoryNode.
		Provenance json.RawMessage `bun:"type:JSONB,notnull" json:"provenance"`
	}
)
